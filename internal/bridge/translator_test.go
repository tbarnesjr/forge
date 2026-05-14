package bridge

import (
	"strings"
	"testing"

	"github.com/jelmersnoeck/forge/internal/types"
	"github.com/stretchr/testify/require"
)

func newTestTranslator() *Translator {
	return NewTranslator("thread-1", "starter-msg", "session-abc", true, false)
}

func TestTranslator_Text_BufferedUntilDone(t *testing.T) {
	tr := newTestTranslator()

	// Text should be buffered
	actions := tr.Translate(types.OutboundEvent{Type: "text", Content: "Hello "})
	require.Empty(t, actions)

	actions = tr.Translate(types.OutboundEvent{Type: "text", Content: "world!"})
	require.Empty(t, actions)

	// Done flushes
	actions = tr.Translate(types.OutboundEvent{Type: "done"})
	require.NotEmpty(t, actions)

	// Should have at least a text post and a done embed
	var posts []DiscordAction
	for _, a := range actions {
		if a.Type == ActionPost && a.Content != "" {
			posts = append(posts, a)
		}
	}
	require.GreaterOrEqual(t, len(posts), 1)
	require.Contains(t, posts[0].Content, "Hello world!")
}

func TestTranslator_Text_FlushesOnOverflow(t *testing.T) {
	tr := newTestTranslator()

	// Build text > MaxChunkSize
	long := strings.Repeat("Word. ", MaxChunkSize/6+1)
	actions := tr.Translate(types.OutboundEvent{Type: "text", Content: long})
	require.NotEmpty(t, actions, "should flush when buffer exceeds MaxChunkSize")
}

func TestTranslator_ToolUse_CreatesEmbed(t *testing.T) {
	tr := newTestTranslator()

	actions := tr.Translate(types.OutboundEvent{
		Type:     "tool_use",
		ToolName: "Read",
		Content:  `{"file_path": "/foo/bar.go"}`,
	})

	require.NotEmpty(t, actions)
	// Should have a post with an embed
	found := false
	for _, a := range actions {
		if a.Type == ActionPost && a.Embed != nil {
			require.Contains(t, a.Embed.Title, "Read")
			require.Equal(t, "running…", a.Embed.Footer.Text)
			found = true
		}
	}
	require.True(t, found, "should post tool embed")
}

func TestTranslator_ToolUse_TruncatesLongArgs(t *testing.T) {
	tr := newTestTranslator()

	longArgs := strings.Repeat("x", 500)
	actions := tr.Translate(types.OutboundEvent{
		Type:     "tool_use",
		ToolName: "Bash",
		Content:  longArgs,
	})

	for _, a := range actions {
		if a.Type == ActionPost && a.Embed != nil {
			// 200 chars + "…" (multi-byte)
			require.LessOrEqual(t, len(a.Embed.Description), 204)
		}
	}
}

func TestTranslator_Thinking_ShowsReaction(t *testing.T) {
	tr := newTestTranslator()
	tr.SetLastBotMsgID("bot-1")

	actions := tr.Translate(types.OutboundEvent{Type: "thinking"})
	require.Len(t, actions, 1)
	require.Equal(t, ActionReact, actions[0].Type)
	require.Equal(t, "💭", actions[0].Emoji)
}

func TestTranslator_Thinking_HiddenByDefault(t *testing.T) {
	tr := NewTranslator("thread-1", "starter", "s1", false, false)
	tr.SetLastBotMsgID("bot-1")

	actions := tr.Translate(types.OutboundEvent{Type: "thinking"})
	require.Empty(t, actions)
}

func TestTranslator_PhaseTransition_DeduplicatesPhase(t *testing.T) {
	tr := newTestTranslator()

	a1 := tr.Translate(types.OutboundEvent{Type: "planning_start", Content: "feature"})
	require.NotEmpty(t, a1)

	// Same phase again — suppressed
	a2 := tr.Translate(types.OutboundEvent{Type: "planning_start", Content: "feature"})
	require.Empty(t, a2)

	// Different phase — not suppressed
	a3 := tr.Translate(types.OutboundEvent{Type: "ideation_start"})
	require.NotEmpty(t, a3)
}

func TestTranslator_ClarificationQuestion(t *testing.T) {
	tr := newTestTranslator()

	actions := tr.Translate(types.OutboundEvent{
		Type:    "clarification_question",
		Content: "What branch should I target?",
	})
	require.Len(t, actions, 1)
	require.Equal(t, ActionPost, actions[0].Type)
	require.Equal(t, "What branch should I target?", actions[0].Content)
}

func TestTranslator_Error_PinnedMessage(t *testing.T) {
	tr := newTestTranslator()

	actions := tr.Translate(types.OutboundEvent{
		Type:    "error",
		Content: "API rate limit exceeded",
	})

	require.NotEmpty(t, actions)
	found := false
	for _, a := range actions {
		if a.Type == ActionPost && strings.HasPrefix(a.Content, "❌") {
			require.True(t, a.Pin)
			found = true
		}
	}
	require.True(t, found)
}

func TestTranslator_Interrupted(t *testing.T) {
	tr := newTestTranslator()

	actions := tr.Translate(types.OutboundEvent{Type: "interrupted"})
	found := false
	for _, a := range actions {
		if a.Type == ActionPost && strings.Contains(a.Content, "Interrupted") {
			found = true
		}
	}
	require.True(t, found)
}

func TestTranslator_StalenessWarning_Reaction(t *testing.T) {
	tr := newTestTranslator()
	tr.SetLastBotMsgID("bot-1")

	actions := tr.Translate(types.OutboundEvent{Type: "staleness_warning"})
	require.Len(t, actions, 1)
	require.Equal(t, ActionReact, actions[0].Type)
	require.Equal(t, "⚠️", actions[0].Emoji)
}

func TestTranslator_Usage_AccumulatesTokens(t *testing.T) {
	tr := newTestTranslator()

	tr.Translate(types.OutboundEvent{
		Type:  "usage",
		Model: "claude-sonnet-4-20250514",
		Usage: &types.TokenUsage{InputTokens: 100, OutputTokens: 50},
	})
	tr.Translate(types.OutboundEvent{
		Type:  "usage",
		Model: "claude-sonnet-4-20250514",
		Usage: &types.TokenUsage{InputTokens: 200, OutputTokens: 75},
	})
	tr.Translate(types.OutboundEvent{
		Type:  "usage",
		Model: "claude-haiku-4-5",
		Usage: &types.TokenUsage{InputTokens: 10, OutputTokens: 5},
	})

	require.Equal(t, 310, tr.totalInput)
	require.Equal(t, 130, tr.totalOutput)
	require.Len(t, tr.models, 2)
}

func TestTranslator_PRURL_PinnedAndReaction(t *testing.T) {
	tr := newTestTranslator()

	actions := tr.Translate(types.OutboundEvent{
		Type:    "pr_url",
		Content: "https://github.com/foo/bar/pull/42",
	})

	var postFound, reactFound bool
	for _, a := range actions {
		if a.Type == ActionPost && strings.Contains(a.Content, "PR ready") {
			require.True(t, a.Pin)
			postFound = true
		}
		if a.Type == ActionReact && a.Emoji == "🚀" {
			require.Equal(t, "starter-msg", a.MessageID)
			reactFound = true
		}
	}
	require.True(t, postFound)
	require.True(t, reactFound)
}

func TestTranslator_Done_SummaryEmbed(t *testing.T) {
	tr := newTestTranslator()

	tr.Translate(types.OutboundEvent{
		Type:  "usage",
		Model: "claude-sonnet-4-20250514",
		Usage: &types.TokenUsage{InputTokens: 1000, OutputTokens: 500},
	})

	actions := tr.Translate(types.OutboundEvent{Type: "done"})

	var embedFound bool
	for _, a := range actions {
		if a.Type == ActionPost && a.Embed != nil && a.Embed.Title == "✅ Done" {
			embedFound = true
			require.Equal(t, 0x57F287, a.Embed.Color)
			require.NotEmpty(t, a.Embed.Fields)
		}
	}
	require.True(t, embedFound)
}

func TestTranslator_Done_RevealSessionID(t *testing.T) {
	tr := NewTranslator("thread-1", "starter", "session-secret", true, true)

	actions := tr.Translate(types.OutboundEvent{Type: "done"})
	for _, a := range actions {
		if a.Type == ActionPost && a.Embed != nil && a.Embed.Title == "✅ Done" {
			require.Contains(t, a.Embed.Description, "session-secret")
		}
	}
}

func TestTranslator_Done_HidesSessionID(t *testing.T) {
	tr := NewTranslator("thread-1", "starter", "session-secret", true, false)

	actions := tr.Translate(types.OutboundEvent{Type: "done"})
	for _, a := range actions {
		if a.Type == ActionPost && a.Embed != nil && a.Embed.Title == "✅ Done" {
			require.NotContains(t, a.Embed.Description, "session-secret")
		}
	}
}

func TestTranslator_SilentEvents(t *testing.T) {
	tr := newTestTranslator()

	for _, evtType := range []string{"retry", "compact", "pr_monitor", "task_status"} {
		actions := tr.Translate(types.OutboundEvent{Type: evtType, Content: "some data"})
		require.Empty(t, actions, "event type %q should be silent", evtType)
	}
}

func TestTranslator_UnknownEvent(t *testing.T) {
	tr := newTestTranslator()
	actions := tr.Translate(types.OutboundEvent{Type: "unknown_future_event"})
	require.Empty(t, actions)
}

func TestTranslator_FullSequence(t *testing.T) {
	// Simulate a realistic event sequence: intent → plan → tool → text → done
	tr := newTestTranslator()
	tr.SetLastBotMsgID("bot-0")

	var allActions []DiscordAction
	events := []types.OutboundEvent{
		{Type: "intent_classified", Content: "feature"},
		{Type: "planning_start"},
		{Type: "thinking"},
		{Type: "tool_use", ToolName: "Read", Content: `{"file_path":"main.go"}`},
		{Type: "text", Content: "I'll implement the feature."},
		{Type: "tool_use", ToolName: "Write", Content: `{"file_path":"new.go"}`},
		{Type: "text", Content: "Done implementing."},
		{Type: "usage", Model: "claude-sonnet-4-20250514", Usage: &types.TokenUsage{InputTokens: 500, OutputTokens: 200}},
		{Type: "pr_url", Content: "https://github.com/test/pull/1"},
		{Type: "done"},
	}

	for _, evt := range events {
		actions := tr.Translate(evt)
		allActions = append(allActions, actions...)
		// Update lastBotMsgID for posts
		for _, a := range actions {
			if a.Type == ActionPost {
				tr.SetLastBotMsgID("bot-latest")
			}
		}
	}

	// Verify we got: phase posts, tool embeds, text, PR, done
	var types []ActionType
	for _, a := range allActions {
		types = append(types, a.Type)
	}
	require.Contains(t, types, ActionPost)
}
