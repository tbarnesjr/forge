package phase

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jelmersnoeck/forge/internal/review"
	"github.com/jelmersnoeck/forge/internal/runtime/loop"
	"github.com/jelmersnoeck/forge/internal/runtime/provider"
	"github.com/jelmersnoeck/forge/internal/runtime/session"
	"github.com/jelmersnoeck/forge/internal/tools"
	"github.com/jelmersnoeck/forge/internal/types"
)

const maxReviewCycles = 5

// Orchestrator chains phases together into a complete workflow.
type Orchestrator struct {
	maxReviewCycles int
}

// OrchestratorOpts configures an orchestrator run.
type OrchestratorOpts struct {
	Provider      types.LLMProvider
	Registry      *tools.Registry
	Bundle        types.ContextBundle
	CWD           string
	SessionStore  *session.Store
	SessionID     string
	Model         string
	Emit          func(types.OutboundEvent)
	AuditLogger   types.AuditLogger
	InitialPrompt string
	SpecPath      string // if set, skip architect phase

	// QAHistoryID, when set, resumes an existing Q&A conversation.
	QAHistoryID string

	// InvestigateHistoryID, when set, resumes an existing investigation conversation.
	InvestigateHistoryID string

	// PipelineHint controls whether to run the ideation pipeline.
	// Values: "ideate" (always ideate), "code" (skip to coding),
	// "auto" or "" (use complexity gate to decide).
	PipelineHint string
}

// OrchestratorResult is the return value from Orchestrator.Run.
type OrchestratorResult struct {
	// Intent is the classified intent for this run.
	Intent Intent
	// QAHistoryID is set when Intent == IntentQuestion.
	// The caller uses this to resume the Q&A loop on follow-up.
	QAHistoryID string
	// InvestigateHistoryID is set when Intent == IntentInvestigate.
	// The caller uses this to resume the investigation loop on follow-up.
	InvestigateHistoryID string
	// CoderHistoryID is set after the SWE pipeline completes.
	// The caller uses this to resume the coder conversation for follow-ups.
	CoderHistoryID string
	// SpecHistoryID is set after spec creation (planner or architect).
	// Available for extension (e.g., re-running the spec phase).
	SpecHistoryID string
}

// NewSWEOrchestrator creates the default software-engineer orchestrator.
func NewSWEOrchestrator() *Orchestrator {
	return &Orchestrator{
		maxReviewCycles: maxReviewCycles,
	}
}

// Run executes the SWE pipeline with intent classification.
//
// When no spec is provided, it first classifies the user's intent:
//   - question    → runs Q&A loop, returns result with QAHistoryID
//   - investigate → runs investigation loop, returns result with InvestigateHistoryID
//   - task        → runs full SWE pipeline (spec → code → review)
//
// When SpecPath is set, classification is skipped (intent is unambiguously task).
//
//	User prompt
//	    │
//	    ▼
//	┌───────────────┐
//	│   Classify     │──▶ question?    → Q&A loop → return
//	│                │──▶ investigate? → investigate loop → return
//	└───────┬───────┘
//	        │ task
//	        ▼
//	┌─────────────┐
//	│  Architect   │──▶ .forge/specs/<id>.md
//	└──────┬──────┘
//	       │
//	       ▼
//	┌─────────────┐
//	│    Coder     │──▶ implementation
//	└──────┬──────┘
//	       │
//	       ▼
//	┌─────────────┐     ┌──────────┐
//	│   Reviewer   │──▶ │ findings │──▶ back to Coder (max 5×, resets on criticals)
//	└─────────────┘     └──────────┘
func (o *Orchestrator) Run(ctx context.Context, opts OrchestratorOpts) (OrchestratorResult, error) {
	specPath := opts.SpecPath

	// Classify intent (skip if spec is provided — that's unambiguously a task).
	if specPath == "" {
		intent, err := ClassifyIntent(ctx, opts.Provider, opts.InitialPrompt)
		if err != nil {
			log.Printf("[orchestrator:%s] classification error (defaulting to task): %v", opts.SessionID, err)
			opts.Emit(types.OutboundEvent{
				ID:        uuid.New().String(),
				SessionID: opts.SessionID,
				Type:      "classification_error",
				Content:   fmt.Sprintf("classification failed (defaulting to task): %v", err),
				Timestamp: time.Now().UnixMilli(),
			})
		}

		opts.Emit(types.OutboundEvent{
			ID:        uuid.New().String(),
			SessionID: opts.SessionID,
			Type:      "intent_classified",
			Content:   string(intent),
			Timestamp: time.Now().UnixMilli(),
		})

		if intent == IntentQuestion {
			historyID, err := o.runQA(ctx, opts)
			return OrchestratorResult{
				Intent:      IntentQuestion,
				QAHistoryID: historyID,
			}, err
		}

		if intent == IntentInvestigate {
			historyID, err := o.runInvestigate(ctx, opts)
			return OrchestratorResult{
				Intent:               IntentInvestigate,
				InvestigateHistoryID: historyID,
			}, err
		}
	}

	// Task path: augment prompt with prior context if transitioning from Q&A or investigation.
	switch {
	case opts.InvestigateHistoryID != "":
		augmented := "Based on our previous investigation, the user now wants to implement: " +
			opts.InitialPrompt + ". Use the context from the investigation to inform the spec."
		log.Printf("[orchestrator:%s] investigate→task transition, augmented prompt (%d chars)", opts.SessionID, len(augmented))
		opts.InitialPrompt = augmented
		opts.InvestigateHistoryID = ""
	case opts.QAHistoryID != "":
		augmented := "Based on our previous discussion, the user now wants to implement: " +
			opts.InitialPrompt + ". Use the context from the conversation to inform the spec."
		log.Printf("[orchestrator:%s] Q&A→task transition, augmented prompt (%d chars)", opts.SessionID, len(augmented))
		opts.InitialPrompt = augmented
		opts.QAHistoryID = ""
	}

	// Run full SWE pipeline.
	return o.runSWEPipeline(ctx, opts, specPath)
}

// runQA runs the Q&A conversation loop. Returns the history ID for resumption.
func (o *Orchestrator) runQA(ctx context.Context, opts OrchestratorOpts) (string, error) {
	return o.runConversationPhase(ctx, opts, QA(), opts.QAHistoryID)
}

// runInvestigate runs the investigation conversation loop. Returns the history ID for resumption.
func (o *Orchestrator) runInvestigate(ctx context.Context, opts OrchestratorOpts) (string, error) {
	return o.runConversationPhase(ctx, opts, Investigate(), opts.InvestigateHistoryID)
}

// runConversationPhase runs a conversation loop for a given phase config,
// resuming from historyID if non-empty. Returns the resulting history ID.
func (o *Orchestrator) runConversationPhase(ctx context.Context, opts OrchestratorOpts, p Phase, historyID string) (string, error) {
	registry := opts.Registry.Filtered(p.AllowedTools, p.DisallowedTools)
	bundle := InjectPhasePrompt(opts.Bundle, p.Name)

	l := loop.New(loop.Options{
		Provider:     opts.Provider,
		Tools:        registry,
		Context:      bundle,
		CWD:          opts.CWD,
		SessionStore: opts.SessionStore,
		SessionID:    opts.SessionID,
		Model:        opts.Model,
		MaxTurns:     p.MaxTurns,
		AuditLogger:  opts.AuditLogger,
	})

	var err error
	switch historyID {
	case "":
		err = l.Send(ctx, opts.InitialPrompt, opts.Emit)
	default:
		err = l.Resume(ctx, historyID, opts.InitialPrompt, opts.Emit)
	}

	return l.HistoryID(), err
}

// runSWEPipeline runs the full spec → code → review pipeline.
func (o *Orchestrator) runSWEPipeline(ctx context.Context, opts OrchestratorOpts, specPath string) (OrchestratorResult, error) {
	result := OrchestratorResult{Intent: IntentTask}

	// Phase 1: Spec Creation (skipped if spec already provided)
	if specPath == "" {
		useIdeation := shouldIdeate(opts.PipelineHint)

		switch useIdeation {
		case true:
			// Full ideation pipeline: ideate → clarify → plan
			o.emitPhaseStart(opts, "ideate")

			debateResult, err := RunDebate(ctx, DebateOpts{
				Provider:     opts.Provider,
				Registry:     opts.Registry,
				Bundle:       opts.Bundle,
				CWD:          opts.CWD,
				SessionStore: opts.SessionStore,
				SessionID:    opts.SessionID,
				Model:        opts.Model,
				Emit:         opts.Emit,
				AuditLogger:  opts.AuditLogger,
				Prompt:       opts.InitialPrompt,
			})
			if err != nil {
				return result, fmt.Errorf("ideation pipeline: %w", err)
			}

			specPath = debateResult.SpecPath
			if specPath == "" {
				specPath = findLatestSpec(opts.CWD)
			}

			result.SpecHistoryID = debateResult.PlannerHistoryID

			o.emitPhaseComplete(opts, "ideate", fmt.Sprintf("spec: %s", specPath))

		default:
			// Single-agent architect (original behavior)
			o.emitPhaseStart(opts, "spec")

			specResult, err := o.runArchitect(ctx, opts)
			if err != nil {
				return result, fmt.Errorf("architect phase: %w", err)
			}

			specPath = specResult.SpecPath
			if specPath == "" {
				specPath = findLatestSpec(opts.CWD)
			}

			result.SpecHistoryID = specResult.HistoryID

			o.emitPhaseComplete(opts, "spec", fmt.Sprintf("spec: %s", specPath))
		}
	}

	// Staleness check before coding
	staleness := CheckStaleness(opts.CWD)
	if staleness.Stale {
		switch {
		case staleness.Error != nil:
			opts.Emit(types.OutboundEvent{
				ID:        uuid.New().String(),
				SessionID: opts.SessionID,
				Type:      "staleness_error",
				Content:   staleness.Error.Error(),
				Timestamp: time.Now().UnixMilli(),
			})
			// If it's a pull failure (not just uncommitted changes warning),
			// halt before coding.
			if staleness.Behind > 0 && !staleness.Pulled {
				return result, fmt.Errorf("branch is stale and auto-pull failed: %w", staleness.Error)
			}
		case staleness.Pulled:
			opts.Emit(types.OutboundEvent{
				ID:        uuid.New().String(),
				SessionID: opts.SessionID,
				Type:      "staleness_warning",
				Content:   fmt.Sprintf("Branch was %d commits behind — auto-pulled with rebase", staleness.Behind),
				Timestamp: time.Now().UnixMilli(),
			})
		}
	}

	// Phase 2: Coder
	o.emitPhaseHandoff(opts, "spec", "code")
	o.emitPhaseStart(opts, "code")

	log.Printf("[orchestrator:%s] spec phase complete (specHistoryID=%s, specPath=%s)", opts.SessionID, result.SpecHistoryID, specPath)

	var coderHistoryID string
	coderHistoryID, err := o.runCoder(ctx, opts, specPath)
	if err != nil {
		return result, fmt.Errorf("coder phase: %w", err)
	}

	o.emitPhaseComplete(opts, "code", "implementation complete")

	// Phase 3: Review → Fix loop
	var prevReviewSHA string
	for cycle := 0; cycle < o.maxReviewCycles; cycle++ {
		o.emitPhaseHandoff(opts, "code", "review")
		o.emitPhaseStart(opts, "review")

		// Determine the diff for this review cycle.
		var diff string
		incremental := false
		if cycle == 0 {
			// First cycle: full branch diff.
			d, err := review.GetDiff(opts.CWD, "")
			if err != nil {
				log.Printf("[orchestrator:%s] get diff error: %v", opts.SessionID, err)
				break
			}
			diff = d
		} else {
			// Subsequent cycles: incremental diff since last review.
			currentSHA, err := review.GetHeadSHA(opts.CWD)
			if err != nil {
				log.Printf("[orchestrator:%s] get HEAD SHA error: %v", opts.SessionID, err)
				break
			}
			if currentSHA == prevReviewSHA {
				opts.Emit(types.OutboundEvent{
					ID:        uuid.New().String(),
					SessionID: opts.SessionID,
					Type:      "review_summary",
					Content:   "No new changes to review — coder made no commits.",
					Timestamp: time.Now().Unix(),
				})
				break
			}
			d, err := review.GetIncrementalDiff(opts.CWD, prevReviewSHA)
			if err != nil {
				log.Printf("[orchestrator:%s] incremental diff error: %v", opts.SessionID, err)
				break
			}
			if strings.TrimSpace(d) == "" {
				opts.Emit(types.OutboundEvent{
					ID:        uuid.New().String(),
					SessionID: opts.SessionID,
					Type:      "review_summary",
					Content:   "No new changes to review.",
					Timestamp: time.Now().Unix(),
				})
				break
			}
			diff = d
			incremental = true
		}

		// Capture HEAD before running the review.
		sha, err := review.GetHeadSHA(opts.CWD)
		if err != nil {
			log.Printf("[orchestrator:%s] get HEAD SHA error: %v", opts.SessionID, err)
			break
		}
		prevReviewSHA = sha

		reviewResult, err := o.runReviewerWithDiff(ctx, opts, specPath, diff, incremental)
		if err != nil {
			log.Printf("[orchestrator:%s] reviewer error (continuing): %v", opts.SessionID, err)
			break
		}

		o.emitPhaseComplete(opts, "review", fmt.Sprintf("%d findings", len(reviewResult.Consolidated)))

		// Use consolidated findings for severity checks and coder formatting.
		if !review.HasConsolidatedHighSeverity(reviewResult.Consolidated) {
			break
		}

		// Feed findings back to coder
		if cycle+1 >= o.maxReviewCycles {
			if review.HasConsolidatedCritical(reviewResult.Consolidated) {
				// Critical findings remain — reset counter for another pass.
				cycle = -1 // will become 0 after loop increment
				log.Printf("[orchestrator:%s] critical findings remain after %d cycles, resetting counter", opts.SessionID, o.maxReviewCycles)
			} else {
				opts.Emit(types.OutboundEvent{
					ID:        uuid.New().String(),
					SessionID: opts.SessionID,
					Type:      "warning",
					Content:   fmt.Sprintf("Max review cycles (%d) reached — completing with remaining findings", o.maxReviewCycles),
					Timestamp: time.Now().Unix(),
				})
				break
			}
		}

		o.emitPhaseHandoff(opts, "review", "code")
		o.emitPhaseStart(opts, "code")

		fixMsg := formatConsolidatedFindingsForCoder(reviewResult.Consolidated)
		coderHistoryID, err = o.runCoderResume(ctx, opts, coderHistoryID, fixMsg)
		if err != nil {
			return result, fmt.Errorf("coder fix phase: %w", err)
		}

		o.emitPhaseComplete(opts, "code", "fixes applied")
	}

	// Review → Fix loop complete. Squash branch commits before PR creation.
	squashResult := tools.SquashBranchCommits(ctx, opts.CWD)
	if squashResult.Squashed {
		o.emitPhaseEvent(opts, "squash",
			fmt.Sprintf("squashed %d commits into 1", squashResult.CommitCount))
	}
	if squashResult.Error != nil {
		log.Printf("[orchestrator:%s] squash warning: %v", opts.SessionID, squashResult.Error)
	}

	result.CoderHistoryID = coderHistoryID
	return result, nil
}

// RunSinglePhase runs a single phase in isolation.
func RunSinglePhase(ctx context.Context, opts OrchestratorOpts, phase Phase) error {
	registry := opts.Registry
	if len(phase.AllowedTools) > 0 || len(phase.DisallowedTools) > 0 {
		registry = opts.Registry.Filtered(phase.AllowedTools, phase.DisallowedTools)
	}

	model := opts.Model
	if phase.Model != "" {
		model = phase.Model
	}

	// Inject phase-specific prompt into the context bundle.
	bundle := InjectPhasePrompt(opts.Bundle, phase.Name)

	loopOpts := loop.Options{
		Provider:     opts.Provider,
		Tools:        registry,
		Context:      bundle,
		CWD:          opts.CWD,
		SessionStore: opts.SessionStore,
		SessionID:    opts.SessionID,
		Model:        model,
		MaxTurns:     phase.MaxTurns,
		AuditLogger:  opts.AuditLogger,
	}

	l := loop.New(loopOpts)
	return l.Send(ctx, opts.InitialPrompt, opts.Emit)
}

func (o *Orchestrator) runArchitect(ctx context.Context, opts OrchestratorOpts) (Result, error) {
	phase := Architect()
	registry := opts.Registry.Filtered(phase.AllowedTools, phase.DisallowedTools)
	bundle := InjectPhasePrompt(opts.Bundle, phase.Name)

	model := opts.Model
	if phase.Model != "" {
		model = phase.Model
	}

	loopOpts := loop.Options{
		Provider:     opts.Provider,
		Tools:        registry,
		Context:      bundle,
		CWD:          opts.CWD,
		SessionStore: opts.SessionStore,
		SessionID:    opts.SessionID,
		Model:        model,
		MaxTurns:     phase.MaxTurns,
		AuditLogger:  opts.AuditLogger,
	}

	l := loop.New(loopOpts)
	if err := l.Send(ctx, opts.InitialPrompt, opts.Emit); err != nil {
		log.Printf("[orchestrator:%s] architect phase Send failed (historyID=%s, promptLen=%d): %v",
			opts.SessionID, l.HistoryID(), len(opts.InitialPrompt), err)
		return Result{Phase: "spec", HistoryID: l.HistoryID()}, err
	}

	// Try to find the spec that was just written.
	specPath := findLatestSpec(opts.CWD)
	return Result{Phase: "spec", SpecPath: specPath, HistoryID: l.HistoryID()}, nil
}

// runCoder creates a new conversation loop for the coder phase and returns its historyID.
func (o *Orchestrator) runCoder(ctx context.Context, opts OrchestratorOpts, specPath string) (string, error) {
	prompt := buildCoderPrompt(specPath)

	phase := Coder()
	bundle := InjectPhasePrompt(opts.Bundle, phase.Name)

	model := opts.Model
	if phase.Model != "" {
		model = phase.Model
	}

	loopOpts := loop.Options{
		Provider:     opts.Provider,
		Tools:        opts.Registry, // coder gets all tools
		Context:      bundle,
		CWD:          opts.CWD,
		SessionStore: opts.SessionStore,
		SessionID:    opts.SessionID,
		Model:        model,
		MaxTurns:     phase.MaxTurns,
		AuditLogger:  opts.AuditLogger,
	}

	l := loop.New(loopOpts)
	if err := l.Send(ctx, prompt, opts.Emit); err != nil {
		log.Printf("[orchestrator:%s] coder phase Send failed (historyID=%s, promptLen=%d): %v",
			opts.SessionID, l.HistoryID(), len(prompt), err)
		return "", err
	}
	return l.HistoryID(), nil
}

// runCoderResume resumes an existing coder conversation with a new message.
func (o *Orchestrator) runCoderResume(ctx context.Context, opts OrchestratorOpts, historyID, message string) (string, error) {
	phase := Coder()
	bundle := InjectPhasePrompt(opts.Bundle, phase.Name)

	model := opts.Model
	if phase.Model != "" {
		model = phase.Model
	}

	loopOpts := loop.Options{
		Provider:     opts.Provider,
		Tools:        opts.Registry, // coder gets all tools
		Context:      bundle,
		CWD:          opts.CWD,
		SessionStore: opts.SessionStore,
		SessionID:    opts.SessionID,
		Model:        model,
		MaxTurns:     phase.MaxTurns,
		AuditLogger:  opts.AuditLogger,
	}

	l := loop.New(loopOpts)
	if err := l.Resume(ctx, historyID, message, opts.Emit); err != nil {
		log.Printf("[orchestrator:%s] coder resume failed (historyID=%s, messageLen=%d): %v",
			opts.SessionID, historyID, len(message), err)
		// Return original historyID — the caller can retry with the same session.
		return historyID, err
	}
	return l.HistoryID(), nil
}

func (o *Orchestrator) runReviewer(ctx context.Context, opts OrchestratorOpts, specPath string) (Result, error) {
	// Full branch diff for manual /review — not incremental.
	diff, err := review.GetDiff(opts.CWD, "")
	if err != nil {
		return Result{Phase: "review"}, fmt.Errorf("get diff: %w", err)
	}
	return o.runReviewerWithDiff(ctx, opts, specPath, diff, false)
}

func (o *Orchestrator) runReviewerWithDiff(ctx context.Context, opts OrchestratorOpts, specPath string, diff string, incremental bool) (Result, error) {
	result := Result{Phase: "review"}

	// Collect available providers.
	providers := make(map[string]types.LLMProvider)
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		providers["anthropic"] = provider.NewAnthropic(key)
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		providers["openai"] = provider.NewOpenAI(key)
	}

	if len(providers) == 0 {
		opts.Emit(types.OutboundEvent{
			ID:        uuid.New().String(),
			SessionID: opts.SessionID,
			Type:      "review_error",
			Content:   "No providers available for review. Set ANTHROPIC_API_KEY and/or OPENAI_API_KEY.",
			Timestamp: time.Now().Unix(),
		})
		return result, nil
	}

	if diff == "" {
		opts.Emit(types.OutboundEvent{
			ID:        uuid.New().String(),
			SessionID: opts.SessionID,
			Type:      "review_summary",
			Content:   "No changes to review.",
			Timestamp: time.Now().Unix(),
		})
		return result, nil
	}

	result.Diff = diff

	// Pick reviewers.
	var reviewers []review.Reviewer
	hasActiveSpecs := false
	for _, spec := range opts.Bundle.Specs {
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
		Diff:        diff,
		Specs:       opts.Bundle.Specs,
		Context:     opts.Bundle,
		BaseBranch:  "",
		CWD:         opts.CWD,
		Incremental: incremental,
	}

	cr := orch.Run(ctx, req, opts.Emit)

	// Flatten all raw findings into the result.
	for _, r := range cr.Raw {
		result.Findings = append(result.Findings, r.Findings...)
	}
	result.Consolidated = cr.Consolidated

	return result, nil
}

// buildCoderPrompt constructs the initial prompt for the coder phase.
func buildCoderPrompt(specPath string) string {
	if specPath == "" {
		return "Implement the changes discussed. Follow the project's coding standards and test everything."
	}

	specContent, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Sprintf(
			"Implement the spec at %s. Read the spec file first, then implement it. "+
				"Follow the spec's Behavior, Constraints, and Interfaces sections precisely. "+
				"When done, reconcile the spec with any corrections or discoveries.",
			specPath,
		)
	}

	return fmt.Sprintf(
		"Implement the following spec. The spec is the source of truth "+
			"— follow its Behavior, Constraints, and Interfaces sections precisely.\n\n"+
			"Spec file: %s\n\n%s\n\n"+
			"When done, reconcile this spec file with any corrections or "+
			"discoveries made during implementation.",
		specPath, string(specContent),
	)
}

// formatConsolidatedFindingsForCoder converts consolidated review findings into
// a prompt for the coder. Only critical and warning findings are included.
func formatConsolidatedFindingsForCoder(findings []review.ConsolidatedFinding) string {
	return review.FormatConsolidatedForCoder(findings)
}

// findLatestSpec finds the most recently modified spec in .forge/specs/.
func findLatestSpec(cwd string) string {
	specsDir := filepath.Join(cwd, ".forge", "specs")
	entries, err := os.ReadDir(specsDir)
	if err != nil {
		return ""
	}

	var latest string
	var latestTime time.Time

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
			latest = filepath.Join(specsDir, entry.Name())
		}
	}

	return latest
}

// InjectPhasePrompt adds the phase-specific prompt to the context bundle
// as an AgentsMD entry so it gets included in the system prompt.
func InjectPhasePrompt(bundle types.ContextBundle, phaseName string) types.ContextBundle {
	phasePrompt := PromptForPhase(phaseName)
	if phasePrompt == "" {
		return bundle
	}

	// Deep copy AgentsMD to avoid mutating the original.
	newAgentsMD := make([]types.AgentsMDEntry, len(bundle.AgentsMD))
	copy(newAgentsMD, bundle.AgentsMD)

	newAgentsMD = append(newAgentsMD, types.AgentsMDEntry{
		Path:    fmt.Sprintf("phase:%s", phaseName),
		Content: phasePrompt,
		Level:   "phase",
	})

	// Return a copy with the new entry.
	return types.ContextBundle{
		AgentsMD:          newAgentsMD,
		Rules:             bundle.Rules,
		SkillDescriptions: bundle.SkillDescriptions,
		AgentDefinitions:  bundle.AgentDefinitions,
		Specs:             bundle.Specs,
		Settings:          bundle.Settings,
	}
}

func (o *Orchestrator) emitPhaseStart(opts OrchestratorOpts, phase string) {
	opts.Emit(types.OutboundEvent{
		ID:        uuid.New().String(),
		SessionID: opts.SessionID,
		Type:      "phase_start",
		Content:   phase,
		Timestamp: time.Now().Unix(),
	})
}

func (o *Orchestrator) emitPhaseComplete(opts OrchestratorOpts, phase, summary string) {
	opts.Emit(types.OutboundEvent{
		ID:        uuid.New().String(),
		SessionID: opts.SessionID,
		Type:      "phase_complete",
		Content:   fmt.Sprintf("%s: %s", phase, summary),
		Timestamp: time.Now().Unix(),
	})
}

func (o *Orchestrator) emitPhaseHandoff(opts OrchestratorOpts, from, to string) {
	opts.Emit(types.OutboundEvent{
		ID:        uuid.New().String(),
		SessionID: opts.SessionID,
		Type:      "phase_handoff",
		Content:   fmt.Sprintf("%s → %s", from, to),
		Timestamp: time.Now().Unix(),
	})
}

func (o *Orchestrator) emitPhaseEvent(opts OrchestratorOpts, eventType, content string) {
	opts.Emit(types.OutboundEvent{
		ID:        uuid.New().String(),
		SessionID: opts.SessionID,
		Type:      "phase_event",
		Content:   fmt.Sprintf("%s: %s", eventType, content),
		Timestamp: time.Now().Unix(),
	})
}

// RunReviewOnly runs the review phase in isolation.
func RunReviewOnly(ctx context.Context, opts OrchestratorOpts) error {
	o := NewSWEOrchestrator()
	result, err := o.runReviewer(ctx, opts, opts.SpecPath)
	if err != nil {
		return err
	}
	_ = result
	return nil
}

// shouldCreatePR checks if we're in a state where PR creation makes sense.
// Returns (true, "") if yes, or (false, reason) if not.
func shouldCreatePR(cwd string) (bool, string) {
	// Must be a git repo.
	if err := tools.RunGitCmd(cwd, "rev-parse", "--git-dir"); err != nil {
		return false, "not a git repository"
	}

	// Must be on a feature branch.
	branch, err := tools.GitOutput(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false, fmt.Sprintf("cannot determine branch: %v", err)
	}
	if branch == "main" || branch == "master" {
		return false, fmt.Sprintf("on %s branch", branch)
	}

	// Must have changes relative to base.
	base := detectDefaultBranchSafe(cwd)
	diffStat, err := tools.GitOutput(cwd, "diff", "--stat", "origin/"+base+"...HEAD")
	if err != nil {
		return false, fmt.Sprintf("cannot diff against origin/%s: %v", base, err)
	}
	if diffStat == "" {
		return false, "no changes relative to base"
	}

	return true, ""
}

// detectDefaultBranchSafe returns the default branch name without failing.
func detectDefaultBranchSafe(cwd string) string {
	for _, candidate := range []string{"main", "master"} {
		if err := tools.RunGitCmd(cwd, "rev-parse", "--verify", "origin/"+candidate); err == nil {
			return candidate
		}
	}
	return "main"
}

// shouldIdeate determines whether to run the ideation pipeline.
//
//	"ideate" → always ideate
//	"code"   → never ideate
//	"auto"/""→ use complexity gate (for now: default to single-agent architect)
func shouldIdeate(hint string) bool {
	switch hint {
	case "ideate":
		return true
	case "code":
		return false
	default:
		// "auto" or empty: complexity gate decides.
		// For now, default to false (single-agent architect) until
		// the complexity gate is implemented in a separate spec.
		return false
	}
}
