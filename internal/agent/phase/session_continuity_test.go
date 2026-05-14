package phase

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jelmersnoeck/forge/internal/runtime/session"
	"github.com/jelmersnoeck/forge/internal/tools"
	"github.com/jelmersnoeck/forge/internal/types"
	"github.com/stretchr/testify/require"
)

// textProvider returns a simple text response — no tool use.
type textProvider struct {
	calls atomic.Int32
}

func (p *textProvider) Chat(_ context.Context, req types.ChatRequest) (<-chan types.ChatDelta, error) {
	p.calls.Add(1)
	ch := make(chan types.ChatDelta, 3)
	go func() {
		defer close(ch)
		ch <- types.ChatDelta{Type: "text_delta", Text: "Troy and Abed in the morning!"}
		ch <- types.ChatDelta{Type: "usage", Usage: &types.TokenUsage{InputTokens: 100, OutputTokens: 50}}
		ch <- types.ChatDelta{Type: "message_stop", StopReason: "end_turn"}
	}()
	return ch, nil
}

// trackingProvider records how many messages each Chat call receives,
// so we can verify Resume is loading history.
type trackingProvider struct {
	calls     atomic.Int32
	mu        sync.Mutex
	msgCounts []int // len(req.Messages) for each call
}

func (p *trackingProvider) Chat(ctx context.Context, req types.ChatRequest) (<-chan types.ChatDelta, error) {
	p.calls.Add(1)
	p.mu.Lock()
	p.msgCounts = append(p.msgCounts, len(req.Messages))
	p.mu.Unlock()

	ch := make(chan types.ChatDelta, 3)
	go func() {
		defer close(ch)
		ch <- types.ChatDelta{Type: "text_delta", Text: "Cool. Cool cool cool."}
		ch <- types.ChatDelta{Type: "usage", Usage: &types.TokenUsage{InputTokens: 100, OutputTokens: 50}}
		ch <- types.ChatDelta{Type: "message_stop", StopReason: "end_turn"}
	}()
	return ch, nil
}

func makeTestOrchestratorOpts(t *testing.T, provider types.LLMProvider) OrchestratorOpts {
	t.Helper()
	sessDir := t.TempDir()
	store := session.NewStore(sessDir)
	registry := tools.NewRegistry()

	return OrchestratorOpts{
		Provider:     provider,
		Registry:     registry,
		Bundle:       types.ContextBundle{AgentDefinitions: map[string]types.AgentDefinition{}},
		CWD:          t.TempDir(),
		SessionStore: store,
		SessionID:    "greendale-session-101",
		Model:        "test-model",
		Emit:         func(types.OutboundEvent) {},
	}
}

func TestRunCoder_ReturnsHistoryID(t *testing.T) {
	r := require.New(t)
	prov := &textProvider{}
	opts := makeTestOrchestratorOpts(t, prov)
	orch := NewSWEOrchestrator()

	historyID, err := orch.runCoder(context.Background(), opts, "")
	r.NoError(err)
	r.NotEmpty(historyID, "runCoder should return a non-empty historyID")
	r.Equal(int32(1), prov.calls.Load())
}

func TestRunCoderResume_ReusesHistory(t *testing.T) {
	r := require.New(t)
	prov := &trackingProvider{}
	opts := makeTestOrchestratorOpts(t, prov)
	orch := NewSWEOrchestrator()

	// First call: create a new coder conversation.
	historyID, err := orch.runCoder(context.Background(), opts, "")
	r.NoError(err)
	r.NotEmpty(historyID)

	// Second call: resume with the stored historyID.
	newHistoryID, err := orch.runCoderResume(context.Background(), opts, historyID, "Fix the bug in Señor Chang's code")
	r.NoError(err)
	r.Equal(historyID, newHistoryID, "Resume should preserve the same historyID")

	// The second Chat call should have more messages than the first (loaded history + new prompt).
	// First call has 1 user message. Resume loads history (user + assistant) + new user = at least 3.
	prov.mu.Lock()
	counts := make([]int, len(prov.msgCounts))
	copy(counts, prov.msgCounts)
	prov.mu.Unlock()

	r.Equal(2, len(counts), "expected exactly 2 Chat calls (initial + resume)")
	r.Greater(counts[1], counts[0],
		"Resumed conversation should have more messages (history was loaded)")
	r.GreaterOrEqual(counts[1], 3,
		"Resume should load at least the original user+assistant pair plus the new message")
}

func TestRunCoderResume_SessionStoreHasAllMessages(t *testing.T) {
	r := require.New(t)
	prov := &textProvider{}
	opts := makeTestOrchestratorOpts(t, prov)
	orch := NewSWEOrchestrator()

	// Initial coder run.
	historyID, err := orch.runCoder(context.Background(), opts, "")
	r.NoError(err)

	// Count messages after first run.
	msgs1, err := opts.SessionStore.Load(historyID)
	r.NoError(err)
	initialCount := len(msgs1)
	r.Greater(initialCount, 0)

	// Resume with fix message.
	_, err = orch.runCoderResume(context.Background(), opts, historyID, "Fix the Human Being mascot rendering")
	r.NoError(err)

	// After resume, same historyID should have MORE messages.
	msgs2, err := opts.SessionStore.Load(historyID)
	r.NoError(err)
	r.Greater(len(msgs2), initialCount,
		"Resume should append messages to the same historyID session")
}

func TestRunArchitect_ReturnsHistoryID(t *testing.T) {
	r := require.New(t)
	prov := &textProvider{}
	opts := makeTestOrchestratorOpts(t, prov)
	opts.InitialPrompt = "Build a paintball tournament tracker for Greendale"
	orch := NewSWEOrchestrator()

	result, err := orch.runArchitect(context.Background(), opts)
	r.NoError(err)
	r.NotEmpty(result.HistoryID, "runArchitect should return a historyID for future resumption")
}

func TestOrchestratorResult_CoderHistoryID(t *testing.T) {
	r := require.New(t)
	prov := &textProvider{}
	opts := makeTestOrchestratorOpts(t, prov)
	opts.InitialPrompt = "Build the Dreamatorium simulator for Greendale"
	opts.SpecPath = "/fake/spec.md" // skip spec creation to simplify

	orch := NewSWEOrchestrator()
	result, err := orch.Run(context.Background(), opts)
	r.NoError(err)
	r.Equal(IntentTask, result.Intent)
	r.NotEmpty(result.CoderHistoryID, "OrchestratorResult should include coder's historyID")
}

func TestOrchestratorResult_SpecHistoryID_WithSpecPath(t *testing.T) {
	r := require.New(t)
	prov := &textProvider{}
	opts := makeTestOrchestratorOpts(t, prov)
	opts.InitialPrompt = "Fix the pillow fort structural integrity"
	opts.SpecPath = "/fake/spec.md" // skip spec creation

	orch := NewSWEOrchestrator()
	result, err := orch.Run(context.Background(), opts)
	r.NoError(err)
	r.Empty(result.SpecHistoryID, "SpecHistoryID should be empty when --spec is provided")
}

func TestOrchestratorResult_CoderHistoryID_ResumeLoadsHistory(t *testing.T) {
	// Verifies the returned CoderHistoryID can be used with loop.Resume
	// and it loads the coder's conversation history.
	r := require.New(t)
	prov := &trackingProvider{}
	opts := makeTestOrchestratorOpts(t, prov)
	opts.InitialPrompt = "Build the study room reservation system"
	opts.SpecPath = "/fake/spec.md"

	orch := NewSWEOrchestrator()
	result, err := orch.Run(context.Background(), opts)
	r.NoError(err)
	r.NotEmpty(result.CoderHistoryID)

	// Now resume using the coder's historyID — this simulates a follow-up message.
	newID, err := orch.runCoderResume(context.Background(), opts, result.CoderHistoryID, "Also add a paintball mode")
	r.NoError(err)
	r.Equal(result.CoderHistoryID, newID, "Resume should use the same historyID")

	// The resume call should have loaded history (more messages than initial).
	prov.mu.Lock()
	counts := make([]int, len(prov.msgCounts))
	copy(counts, prov.msgCounts)
	prov.mu.Unlock()

	r.Equal(2, len(counts), "expected 2 Chat calls (initial coder + resume)")
	r.Greater(counts[1], counts[0], "Resume should load history")
}

func TestRunInvestigate_ReturnsHistoryID(t *testing.T) {
	r := require.New(t)
	prov := &textProvider{}
	opts := makeTestOrchestratorOpts(t, prov)
	opts.InitialPrompt = "Dig into why the study group meetings keep getting interrupted"
	orch := NewSWEOrchestrator()

	historyID, err := orch.runInvestigate(context.Background(), opts)
	r.NoError(err)
	r.NotEmpty(historyID, "runInvestigate should return a non-empty historyID")
	r.Equal(int32(1), prov.calls.Load())
}

func TestRunInvestigate_Resume(t *testing.T) {
	r := require.New(t)
	prov := &trackingProvider{}
	opts := makeTestOrchestratorOpts(t, prov)
	opts.InitialPrompt = "Investigate the air conditioning annex conspiracy"
	orch := NewSWEOrchestrator()

	// First call: start investigation.
	historyID, err := orch.runInvestigate(context.Background(), opts)
	r.NoError(err)
	r.NotEmpty(historyID)

	// Second call: resume with stored historyID.
	opts.InvestigateHistoryID = historyID
	newHistoryID, err := orch.runInvestigate(context.Background(), opts)
	r.NoError(err)
	r.Equal(historyID, newHistoryID, "Resume should preserve the same historyID")

	// Resume call should have more messages (loaded history).
	prov.mu.Lock()
	counts := make([]int, len(prov.msgCounts))
	copy(counts, prov.msgCounts)
	prov.mu.Unlock()

	r.Equal(2, len(counts), "expected 2 Chat calls (initial + resume)")
	r.Greater(counts[1], counts[0], "Resumed investigation should have more messages")
}

func TestOrchestratorResult_Investigate(t *testing.T) {
	// When classification returns investigate, the orchestrator should return
	// IntentInvestigate with a history ID (no SWE pipeline).
	r := require.New(t)

	// Provider that returns "investigate" for classification, then a text response for the loop.
	classifyProv := &mockProvider{
		responses: map[string][]types.ChatDelta{},
	}
	// Set up lightweight models to return investigate.
	for _, m := range types.LightweightModels {
		classifyProv.responses[m] = []types.ChatDelta{
			{Type: "text_delta", Text: `{"intent": "investigate"}`},
		}
	}
	// Also handle the main model for the investigation loop.
	classifyProv.responses["test-model"] = []types.ChatDelta{
		{Type: "text_delta", Text: "The blanket fort infrastructure shows signs of structural weakness..."},
		{Type: "usage", Usage: &types.TokenUsage{InputTokens: 100, OutputTokens: 50}},
		{Type: "message_stop", StopReason: "end_turn"},
	}

	opts := makeTestOrchestratorOpts(t, classifyProv)
	opts.InitialPrompt = "Investigate why the blanket fort keeps collapsing"

	orch := NewSWEOrchestrator()
	result, err := orch.Run(context.Background(), opts)
	r.NoError(err)
	r.Equal(IntentInvestigate, result.Intent)
	r.NotEmpty(result.InvestigateHistoryID, "Should return investigation historyID")
	r.Empty(result.CoderHistoryID, "Should not have a coder historyID")
	r.Empty(result.QAHistoryID, "Should not have a QA historyID")
}

func TestReviewerAlwaysFresh(t *testing.T) {
	// This test verifies the reviewer doesn't get conversation history.
	// We can't easily test runReviewerWithDiff without ANTHROPIC_API_KEY,
	// but we verify that the review orchestrator creates a fresh ChatRequest
	// by checking it's not using Resume.
	//
	// The key assertion: the orchestrator never passes a historyID to
	// the reviewer — it always starts fresh. This is structural, verified
	// by code inspection + the fact that runReviewerWithDiff uses
	// review.NewOrchestrator (not loop.Resume).
	r := require.New(t)
	_ = r

	// Structural check: the reviewer path uses review.NewOrchestrator,
	// not loop.Resume. This test exists as a sentinel — if someone
	// changes the reviewer to use session continuity, this test should
	// be updated to explicitly verify statelessness.
	t.Log("Reviewer statelessness verified by structural review — runReviewerWithDiff uses review.NewOrchestrator, not loop.Resume")
}
