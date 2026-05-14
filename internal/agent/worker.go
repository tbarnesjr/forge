package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jelmersnoeck/forge/internal/agent/phase"
	"github.com/jelmersnoeck/forge/internal/config"
	"github.com/jelmersnoeck/forge/internal/mcp"
	"github.com/jelmersnoeck/forge/internal/review"
	rctx "github.com/jelmersnoeck/forge/internal/runtime/context"
	"github.com/jelmersnoeck/forge/internal/runtime/loop"
	"github.com/jelmersnoeck/forge/internal/runtime/provider"
	"github.com/jelmersnoeck/forge/internal/runtime/session"
	"github.com/jelmersnoeck/forge/internal/runtime/task"
	"github.com/jelmersnoeck/forge/internal/tools"
	"github.com/jelmersnoeck/forge/internal/types"
)

// Worker runs the conversation loop for a single session, pulling messages
// from the Hub and streaming events back through it.
type Worker struct {
	hub         *Hub
	sessionID   string
	cwd         string
	sessionsDir string
	mode        string // "swe" (default), "spec", "code", "review"
	specPath    string // spec file path for --spec flag
	ghAvailable bool   // cached exec.LookPath("gh") result
}

// NewWorker creates a new Worker.
func NewWorker(hub *Hub, sessionID, cwd, sessionsDir, mode, specPath string) *Worker {
	return &Worker{
		hub:         hub,
		sessionID:   sessionID,
		cwd:         cwd,
		sessionsDir: sessionsDir,
		mode:        mode,
		specPath:    specPath,
		ghAvailable: tools.GHAvailable(),
	}
}

// Run starts the worker message loop. It blocks until the context is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("[agent:%s] worker started, cwd=%s", w.sessionID, w.cwd)

	prov := selectProvider()
	registry := tools.NewDefaultRegistry()
	loader := rctx.NewLoader(w.cwd)
	bundle, err := loader.Load([]string{"user", "project", "local"})
	if err != nil {
		log.Printf("[agent:%s] context load error: %v", w.sessionID, err)
		bundle = types.ContextBundle{
			AgentDefinitions: make(map[string]types.AgentDefinition),
		}
	}
	store := session.NewStore(w.sessionsDir)

	// Set up task manager with agent runner for sub-agent execution.
	mgr := task.NewManager()
	tools.SetTaskManager(mgr)
	mgr.SetAgentRunner(w.makeAgentRunner(prov, registry, bundle, store))
	defer mgr.Stop()

	// Connect to MCP servers — tools are stored lazily, not registered with the LLM.
	// The UseMCPTool gateway provides on-demand access (~300 tokens vs ~15K+).
	mcpStore := mcp.NewStore()
	tools.SetMCPStore(mcpStore)
	w.connectMCPServers(ctx, mcpStore)
	defer func() {
		for _, c := range mcpStore.Clients() {
			_ = c.Close(context.Background())
		}
	}()

	// Pick model: settings override, but only pass through real API model IDs
	// to the Anthropic provider. Claude CLI accepts aliases (sonnet, opus, etc.).
	_, isClaudeCLI := prov.(*provider.ClaudeCLIProvider)
	const defaultModel = "claude-opus-4-6"
	model := defaultModel
	switch {
	case isClaudeCLI && bundle.Settings.Model != "":
		model = bundle.Settings.Model
	case bundle.Settings.Model != "" && strings.HasPrefix(bundle.Settings.Model, "claude-"):
		model = bundle.Settings.Model
	}

	// Listen for review triggers in a separate goroutine.
	go w.reviewListener(ctx, bundle)

	// Monitor PR health (rebase, CI) in the background.
	go w.prHealthMonitor(ctx)

	// Broadcast task progress every second for live CLI display.
	go w.taskStatusBroadcaster(ctx, mgr)

	// Track whether the first message has been handled by the orchestrator.
	// After the orchestrator completes, subsequent messages use the plain loop.
	orchestratorDone := false

	// Q&A state: tracks history across question rounds for Resume().
	// These are intentionally in-memory — a worker restart means a new session,
	// so Q&A state doesn't need persistence beyond the process lifetime.
	var qaHistoryID string
	qaActive := false

	// Investigation state: same lifecycle as Q&A but different tool set.
	var investigateHistoryID string
	investigateActive := false

	var historyID string
	for {
		select {
		case <-ctx.Done():
			log.Printf("[agent:%s] worker stopped", w.sessionID)
			return
		default:
		}

		msg := w.hub.PullMessage()
		if len(msg.Text) > 100 {
			log.Printf("[agent:%s] <- %s...", w.sessionID, msg.Text[:100])
		} else {
			log.Printf("[agent:%s] <- %s", w.sessionID, msg.Text)
		}

		var emit func(types.OutboundEvent)
		turnToolsUsed := false // reset each turn; tracks whether any tool_use event fired
		emit = func(event types.OutboundEvent) {
			if event.ID == "" {
				event.ID = uuid.New().String()
			}
			if event.SessionID == "" {
				event.SessionID = w.sessionID
			}
			if event.Timestamp == 0 {
				event.Timestamp = time.Now().UnixMilli()
			}

			// Intercept queue management events
			switch event.Type {
			case "queue_immediate":
				w.hub.EnqueueImmediate(event.Content)
			case "queue_on_complete":
				w.hub.EnqueueCompletion(event.Content)
			case "tool_use":
				turnToolsUsed = true
				// After any tool use, execute immediate queue
				w.executeImmediateQueue(ctx, registry, historyID, emit)
			case "done":
				// Before done event, execute completion queue
				w.executeCompletionQueue(ctx, registry, historyID, emit)
				// Deterministic PR ensure step — runs before done reaches CLI.
				if turnToolsUsed {
					w.ensurePR(ctx, prov, w.specPath, emit)
				}
			}

			w.hub.PublishEvent(event)
		}

		var runErr error
		// Drain any stale interrupt from a previous turn's Ctrl+C
		// that arrived after the turn finished.
		if w.hub.DrainInterrupt() {
			log.Printf("[agent:%s] drained stale interrupt before new turn", w.sessionID)
		}

		// Extract pipeline hint from message metadata.
		pipelineHint := extractPipelineHint(msg.Metadata)

		// Create a cancellable context for this turn so interrupts
		// can abort the loop without killing the entire worker.
		turnCtx, turnCancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-w.hub.InterruptChannel():
				turnCancel()
			case <-turnCtx.Done():
			}
		}()

		// Decide execution path: orchestrator, single phase, or plain loop.
		useOrchestrator := !orchestratorDone && w.mode != ""
		switch {
		case (useOrchestrator || qaActive || investigateActive) && w.mode == "swe":
			result, err := w.runOrchestrator(turnCtx, prov, registry, bundle, store, model, msg.Text, emit, qaHistoryID, investigateHistoryID, pipelineHint)
			runErr = err

			switch result.Intent {
			case phase.IntentQuestion:
				qaHistoryID = result.QAHistoryID
				qaActive = true
				investigateActive = false
				investigateHistoryID = ""
				log.Printf("[agent:%s] state: Q&A active, historyID=%s", w.sessionID, qaHistoryID)
			case phase.IntentInvestigate:
				investigateHistoryID = result.InvestigateHistoryID
				investigateActive = true
				qaActive = false
				qaHistoryID = ""
				log.Printf("[agent:%s] state: investigate active, historyID=%s", w.sessionID, investigateHistoryID)
			default:
				orchestratorDone = true
				qaActive = false
				investigateActive = false
				if result.CoderHistoryID != "" {
					historyID = result.CoderHistoryID
				}
				log.Printf("[agent:%s] state: orchestrator done, coderHistoryID=%s", w.sessionID, historyID)
			}

		case useOrchestrator && (w.mode == "spec" || w.mode == "code" || w.mode == "review"):
			runErr = w.runSinglePhase(turnCtx, prov, registry, bundle, store, model, msg.Text, emit)
			orchestratorDone = true

		case historyID != "":
			// Resume the coder conversation with coder phase config.
			coderBundle := phase.InjectPhasePrompt(bundle, "code")
			l := loop.New(loop.Options{
				Provider:     prov,
				Tools:        registry,
				Context:      coderBundle,
				CWD:          w.cwd,
				SessionStore: store,
				SessionID:    w.sessionID,
				Model:        model,
				MaxTurns:     0,
				AuditLogger:  &StdAuditLogger{},
			})
			runErr = l.Resume(turnCtx, historyID, msg.Text, emit)
			historyID = l.HistoryID()

		default:
			l := loop.New(loop.Options{
				Provider:     prov,
				Tools:        registry,
				Context:      bundle,
				CWD:          w.cwd,
				SessionStore: store,
				SessionID:    w.sessionID,
				Model:        model,
				MaxTurns:     0,
				AuditLogger:  &StdAuditLogger{},
			})
			runErr = l.Send(turnCtx, msg.Text, emit)
			historyID = l.HistoryID()
		}

		turnCancel() // clean up goroutine

		if runErr != nil {
			log.Printf("[agent:%s] error: %v", w.sessionID, runErr)

			// Distinguish interrupts from real errors.
			switch turnCtx.Err() {
			case context.Canceled:
				emit(types.OutboundEvent{Type: "interrupted"})
			default:
				emit(types.OutboundEvent{Type: "error", Content: runErr.Error()})
			}

			// Always emit done so the CLI returns to the prompt.
			emit(types.OutboundEvent{Type: "done"})
		}
	}
}

// runOrchestrator runs the SWE pipeline with intent classification.
func (w *Worker) runOrchestrator(
	ctx context.Context,
	prov types.LLMProvider,
	registry *tools.Registry,
	bundle types.ContextBundle,
	store *session.Store,
	model, prompt string,
	emit func(types.OutboundEvent),
	qaHistoryID string,
	investigateHistoryID string,
	pipelineHint string,
) (phase.OrchestratorResult, error) {
	orch := phase.NewSWEOrchestrator()

	opts := phase.OrchestratorOpts{
		Provider:             prov,
		Registry:             registry,
		Bundle:               bundle,
		CWD:                  w.cwd,
		SessionStore:         store,
		SessionID:            w.sessionID,
		Model:                model,
		Emit:                 emit,
		AuditLogger:          &StdAuditLogger{},
		InitialPrompt:        prompt,
		SpecPath:             w.specPath,
		QAHistoryID:          qaHistoryID,
		InvestigateHistoryID: investigateHistoryID,
		PipelineHint:         pipelineHint,
	}

	result, err := orch.Run(ctx, opts)

	// Emit done so the CLI returns to prompt.
	emit(types.OutboundEvent{Type: "done"})
	return result, err
}

// runSinglePhase runs a standalone phase (spec, code, or review).
func (w *Worker) runSinglePhase(
	ctx context.Context,
	prov types.LLMProvider,
	registry *tools.Registry,
	bundle types.ContextBundle,
	store *session.Store,
	model, prompt string,
	emit func(types.OutboundEvent),
) error {
	var p phase.Phase
	switch w.mode {
	case "spec":
		p = phase.Architect()
	case "code":
		p = phase.Coder()
	case "review":
		p = phase.Reviewer()
	default:
		p = phase.Coder()
	}

	// For review mode, delegate to the reviewer orchestration.
	if w.mode == "review" {
		opts := phase.OrchestratorOpts{
			Provider:      prov,
			Registry:      registry,
			Bundle:        bundle,
			CWD:           w.cwd,
			SessionStore:  store,
			SessionID:     w.sessionID,
			Model:         model,
			Emit:          emit,
			AuditLogger:   &StdAuditLogger{},
			InitialPrompt: prompt,
		}
		emit(types.OutboundEvent{
			ID:        uuid.New().String(),
			SessionID: w.sessionID,
			Type:      "phase_start",
			Content:   "review",
			Timestamp: time.Now().Unix(),
		})
		err := phase.RunReviewOnly(ctx, opts)
		emit(types.OutboundEvent{Type: "done"})
		return err
	}

	opts := phase.OrchestratorOpts{
		Provider:      prov,
		Registry:      registry,
		Bundle:        bundle,
		CWD:           w.cwd,
		SessionStore:  store,
		SessionID:     w.sessionID,
		Model:         model,
		Emit:          emit,
		AuditLogger:   &StdAuditLogger{},
		InitialPrompt: prompt,
		SpecPath:      w.specPath,
	}

	emit(types.OutboundEvent{
		ID:        uuid.New().String(),
		SessionID: w.sessionID,
		Type:      "phase_start",
		Content:   p.Name,
		Timestamp: time.Now().Unix(),
	})

	err := phase.RunSinglePhase(ctx, opts, p)

	emit(types.OutboundEvent{
		ID:        uuid.New().String(),
		SessionID: w.sessionID,
		Type:      "phase_complete",
		Content:   p.Name,
		Timestamp: time.Now().Unix(),
	})

	emit(types.OutboundEvent{Type: "done"})
	return err
}

// executeImmediateQueue runs all commands in the immediate queue
func (w *Worker) executeImmediateQueue(ctx context.Context, registry *tools.Registry, historyID string, emit func(types.OutboundEvent)) {
	commands := w.hub.GetImmediateQueue()
	if len(commands) == 0 {
		return
	}

	for _, command := range commands {
		log.Printf("[agent:%s] executing immediate queue: %s", w.sessionID, command)
		w.executeQueuedCommand(ctx, registry, historyID, command, "immediate", emit)
	}
}

// executeCompletionQueue runs all commands in the completion queue and clears it
func (w *Worker) executeCompletionQueue(ctx context.Context, registry *tools.Registry, historyID string, emit func(types.OutboundEvent)) {
	commands := w.hub.PullCompletionQueue()
	if len(commands) == 0 {
		return
	}

	for _, command := range commands {
		log.Printf("[agent:%s] executing completion queue: %s", w.sessionID, command)
		w.executeQueuedCommand(ctx, registry, historyID, command, "completion", emit)
	}
}

// executeQueuedCommand executes a single queued bash command
func (w *Worker) executeQueuedCommand(ctx context.Context, registry *tools.Registry, historyID, command, queueType string, emit func(types.OutboundEvent)) {
	toolCtx := types.ToolContext{
		Ctx:       ctx,
		CWD:       w.cwd,
		SessionID: w.sessionID,
		HistoryID: historyID,
		Emit:      emit,
	}

	input := map[string]any{"command": command}
	result, err := registry.Execute("Bash", input, toolCtx)

	if err != nil {
		log.Printf("[agent:%s] queued command failed (%s): %v", w.sessionID, queueType, err)
		emit(types.OutboundEvent{
			Type:     "queued_task_error",
			Content:  fmt.Sprintf("[%s queue] Command failed: %s\nError: %v", queueType, command, err),
			ToolName: "Bash",
		})
		return
	}

	// Emit result
	if len(result.Content) > 0 && result.Content[0].Text != "" {
		emit(types.OutboundEvent{
			Type:     "queued_task_result",
			Content:  fmt.Sprintf("[%s queue] %s\n%s", queueType, command, result.Content[0].Text),
			ToolName: "Bash",
		})
	}
}

// ensurePR runs the deterministic PR creation/update step.
// Called after every turn that executed tools. Non-fatal: failures are
// logged, pr_url not emitted, session continues.
func (w *Worker) ensurePR(ctx context.Context, prov types.LLMProvider, specPath string, emit func(types.OutboundEvent)) {
	if !w.ghAvailable {
		return
	}

	// If the parent context is already cancelled (e.g., shutdown in progress),
	// skip PR operations entirely instead of creating a confusing timeout error.
	if ctx.Err() != nil {
		log.Printf("[agent:%s] ensurePR: parent context cancelled (%v), skipping", w.sessionID, ctx.Err())
		return
	}

	// 30s timeout so a hung gh/git call doesn't block done forever.
	prCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	result := phase.EnsurePR(prCtx, prov, w.cwd, specPath)
	if result.Error != nil {
		log.Printf("[agent:%s] ensurePR: %v", w.sessionID, result.Error)
		return
	}

	if result.URL != "" {
		emit(types.OutboundEvent{
			Type:    "pr_url",
			Content: result.URL,
		})
	}
}

// reviewListener drains the hub's review channel and runs reviews.
// Each review runs in the background so it doesn't block the main message loop.
func (w *Worker) reviewListener(ctx context.Context, bundle types.ContextBundle) {
	for {
		select {
		case <-ctx.Done():
			return
		case baseBranch := <-w.hub.ReviewChannel():
			w.runReview(ctx, baseBranch, bundle)
		}
	}
}

// runReview executes the multi-agent code review.
//
//	               ┌─────────────┐
//	               │   Worker    │
//	               └──────┬──────┘
//	                      │ runReview()
//	               ┌──────▼──────┐
//	               │ Orchestrator│
//	               └──────┬──────┘
//	       ┌──────────────┼──────────────┐
//	       │              │              │
//	┌──────▼──────┐┌─────▼──────┐┌──────▼──────┐
//	│  Security   ││ CodeQuality││ Maintain... │  × N providers
//	│  (Anthropic)││  (OpenAI)  ││  (both)     │
//	└─────────────┘└────────────┘└─────────────┘
func (w *Worker) runReview(ctx context.Context, baseBranch string, bundle types.ContextBundle) {
	emit := func(event types.OutboundEvent) {
		if event.ID == "" {
			event.ID = uuid.New().String()
		}
		if event.SessionID == "" {
			event.SessionID = w.sessionID
		}
		if event.Timestamp == 0 {
			event.Timestamp = time.Now().UnixMilli()
		}
		w.hub.PublishEvent(event)
	}

	// Collect available providers.
	providers := make(map[string]types.LLMProvider)

	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		providers["anthropic"] = provider.NewAnthropic(key)
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		providers["openai"] = provider.NewOpenAI(key)
	}
	if _, err := exec.LookPath("claude"); err == nil {
		providers["claude-cli"] = provider.NewClaudeCLI()
	}

	if len(providers) == 0 {
		emit(types.OutboundEvent{
			Type:    "review_error",
			Content: "No providers available for review. Set ANTHROPIC_API_KEY, OPENAI_API_KEY, or install the claude CLI.",
		})
		emit(types.OutboundEvent{Type: "done"})
		return
	}

	// Get git diff.
	diff, err := review.GetDiff(w.cwd, baseBranch)
	if err != nil {
		emit(types.OutboundEvent{
			Type:    "review_error",
			Content: fmt.Sprintf("Failed to get diff: %v", err),
		})
		emit(types.OutboundEvent{Type: "done"})
		return
	}

	if diff == "" {
		emit(types.OutboundEvent{
			Type:    "review_error",
			Content: "No changes to review.",
		})
		emit(types.OutboundEvent{Type: "done"})
		return
	}

	// Pick reviewers — include spec validation if there are active specs.
	var reviewers []review.Reviewer
	hasActiveSpecs := false
	for _, spec := range bundle.Specs {
		if spec.Status == "active" || spec.Status == "draft" {
			hasActiveSpecs = true
			break
		}
	}

	switch hasActiveSpecs {
	case true:
		reviewers = review.DefaultReviewersWithSpec()
	default:
		reviewers = review.DefaultReviewers()
	}

	orch := review.NewOrchestrator(providers, reviewers)
	req := review.ReviewRequest{
		Diff:       diff,
		Specs:      bundle.Specs,
		Context:    bundle,
		BaseBranch: baseBranch,
		CWD:        w.cwd,
	}

	cr := orch.Run(ctx, req, emit)

	// If actionable findings exist, send them to the main loop for remediation.
	// Use consolidated findings for deduplication; fall back to raw if empty.
	// The conversation loop will emit its own done event when it finishes.
	if len(cr.Consolidated) > 0 {
		fixMsg := review.FormatConsolidatedMessage(cr.Consolidated)
		w.hub.PushMessage(types.InboundMessage{Text: fixMsg})
		return
	}

	if review.HasActionableFindings(cr.Raw) {
		deduped := review.DedupRawFindings(cr.Raw)
		fixMsg := review.FormatConsolidatedMessage(deduped)
		w.hub.PushMessage(types.InboundMessage{Text: fixMsg})
		return
	}

	emit(types.OutboundEvent{Type: "done"})
}

// makeAgentRunner returns an AgentRunner that spawns a conversation loop for sub-agents.
//
//	Parent Worker
//	    │
//	    ├─ creates task.Manager with AgentRunner
//	    │
//	    └─ Agent tool invoked by LLM
//	         │
//	         └─ RunAgent(id)
//	              │
//	              └─ goroutine: AgentRunner(ctx, subAgent)
//	                   │
//	                   ├─ Filtered tool registry (allow/deny lists)
//	                   ├─ Sub-agent prompt as system context
//	                   ├─ loop.New(opts).Send(ctx, prompt, emit)
//	                   └─ Captures text output → agent.Output
func (w *Worker) makeAgentRunner(
	prov types.LLMProvider,
	parentRegistry *tools.Registry,
	bundle types.ContextBundle,
	store *session.Store,
) task.AgentRunner {
	return func(ctx context.Context, agent *types.SubAgent) error {
		subRegistry := parentRegistry.Filtered(agent.Tools, agent.DisallowedTools)

		model := agent.Model
		if model == "" {
			const defaultModel = "claude-opus-4-6"
			model = defaultModel
			if m := bundle.Settings.Model; m != "" && strings.HasPrefix(m, "claude-") {
				model = m
			}
		}

		maxTurns := agent.MaxTurns
		if maxTurns < 0 {
			maxTurns = 500 // default for sub-agents when caller didn't specify
		}
		// 0 = unlimited, positive = explicit limit

		opts := loop.Options{
			Provider:     prov,
			Tools:        subRegistry,
			Context:      bundle,
			CWD:          w.cwd,
			SessionStore: store,
			SessionID:    agent.SessionID,
			Model:        model,
			MaxTurns:     maxTurns,
			AuditLogger:  &StdAuditLogger{},
		}

		l := loop.New(opts)

		// Collect text output from the sub-agent.
		var output strings.Builder
		emit := func(event types.OutboundEvent) {
			switch event.Type {
			case "text":
				output.WriteString(event.Content)
			}
		}

		if err := l.Send(ctx, agent.Prompt, emit); err != nil {
			agent.Output = output.String()
			return err
		}

		agent.Output = output.String()
		return nil
	}
}

// selectProvider picks the LLM provider with this priority:
//  1. FORGE_PROVIDER env var (explicit override)
//  2. ~/.forge/config.toml [provider].default
//  3. Auto-detect: ANTHROPIC_API_KEY → Anthropic API, `claude` on PATH → CLI
//  4. Fallback to Anthropic (will fail on first call with clear error)
func selectProvider() types.LLMProvider {
	// Priority 1: explicit env override
	if envProv := os.Getenv("FORGE_PROVIDER"); envProv != "" {
		return providerFromName(envProv)
	}

	// Priority 2: user config
	userCfg, err := config.LoadUserConfig()
	if err != nil {
		log.Printf("[provider] warning: failed to load user config: %v", err)
	} else if userCfg.Provider.Default != "" {
		log.Printf("[provider] using configured default: %s", userCfg.Provider.Default)
		return providerFromName(userCfg.Provider.Default)
	}

	// Priority 3: auto-detect from environment
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return provider.NewAnthropic(key)
	}

	if _, err := exec.LookPath("claude"); err == nil {
		log.Println("[provider] ANTHROPIC_API_KEY not set, using Claude CLI provider")
		return provider.NewClaudeCLI()
	}

	log.Println("[provider] WARNING: ANTHROPIC_API_KEY not set and claude CLI not found — API calls will fail")
	return provider.NewAnthropic("")
}

// providerFromName instantiates a provider by name. Falls back to Anthropic
// with a warning if the name is unrecognized.
func providerFromName(name string) types.LLMProvider {
	switch name {
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			log.Println("[provider] WARNING: provider=anthropic but ANTHROPIC_API_KEY not set — API calls will fail")
		}
		return provider.NewAnthropic(key)
	case "claude-cli":
		if _, err := exec.LookPath("claude"); err != nil {
			log.Println("[provider] WARNING: provider=claude-cli but `claude` not found on PATH")
		}
		return provider.NewClaudeCLI()
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			log.Println("[provider] WARNING: provider=openai but OPENAI_API_KEY not set — API calls will fail")
		}
		return provider.NewOpenAI(key)
	default:
		log.Printf("[provider] WARNING: unknown provider %q — falling back to anthropic", name)
		return provider.NewAnthropic(os.Getenv("ANTHROPIC_API_KEY"))
	}
}

// extractPipelineHint reads the pipeline_hint from message metadata.
// Returns "auto" for missing, nil, or invalid values.
func extractPipelineHint(metadata map[string]any) string {
	if metadata == nil {
		return "auto"
	}
	hint, ok := metadata["pipeline_hint"].(string)
	if !ok || hint == "" {
		return "auto"
	}
	switch hint {
	case "ideate", "code", "auto":
		return hint
	default:
		log.Printf("[worker] unknown pipeline_hint %q — defaulting to auto", hint)
		return "auto"
	}
}

// connectMCPServers loads MCP config and connects to all configured servers,
// storing their tool catalogs in the MCP store for lazy access.
func (w *Worker) connectMCPServers(ctx context.Context, store *mcp.Store) {
	cfg, err := mcp.LoadConfig(w.cwd)
	if err != nil {
		log.Printf("[agent:%s] MCP config load error (continuing without MCP): %v", w.sessionID, err)
		return
	}

	if len(cfg.Servers) == 0 {
		return
	}

	var tokenStore *mcp.TokenStore

	for name, serverCfg := range cfg.Servers {
		// Lazy-init token store only if an OAuth server exists
		if serverCfg.Auth == "oauth" && tokenStore == nil {
			tokenStore, err = mcp.NewTokenStore()
			if err != nil {
				log.Printf("[agent:%s] failed to create MCP token store: %v", w.sessionID, err)
				continue
			}
		}

		_, err := mcp.ConnectAndStore(ctx, store, name, serverCfg, tokenStore)
		if err != nil {
			log.Printf("[agent:%s] MCP server %q connect failed (skipping): %v", w.sessionID, name, err)
			continue
		}

		log.Printf("[agent:%s] MCP server %q connected (lazy tools)", w.sessionID, name)
	}
}
