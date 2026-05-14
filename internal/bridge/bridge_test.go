package bridge

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jelmersnoeck/forge/internal/discord"
	"github.com/jelmersnoeck/forge/internal/forge"
	"github.com/jelmersnoeck/forge/internal/store"
	"github.com/jelmersnoeck/forge/internal/types"
	"github.com/stretchr/testify/require"
)

func setupBridge(t *testing.T) (*Bridge, *discord.StubClient, *forge.StubClient, store.Store) {
	t.Helper()

	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	dc := discord.NewStubClient("bot-123")
	fc := forge.NewStubClient()

	cfg := &Config{
		GuildID:         "guild-1",
		ForgeGatewayURL: "http://localhost:3000",
	}
	cfg.SetChannelsForTest(ChannelsConfig{
		Channels: []ChannelConfig{
			{
				ChannelID:         "channel-1",
				RepoPath:          "/code/forge",
				DefaultBaseBranch: "main",
			},
		},
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := New(fc, dc, st, cfg, logger)

	return b, dc, fc, st
}

func TestBridge_ThreadCreate_CreatesSession(t *testing.T) {
	b, _, fc, st := setupBridge(t)
	ctx := context.Background()

	err := b.OnDiscordEvent(ctx, discord.Event{
		Type:      discord.EventThreadCreate,
		GuildID:   "guild-1",
		ChannelID: "channel-1",
		ThreadID:  "thread-1",
		UserID:    "user-1",
		Username:  "troy.barnes",
	})
	require.NoError(t, err)

	// Forge session created
	sessions := fc.GetSessions()
	require.Len(t, sessions, 1)
	require.Equal(t, "/code/forge", sessions[0].CWD)
	require.Equal(t, "discord", sessions[0].Metadata["source"])

	// Store mapping created
	rec, err := st.GetSessionByThread(ctx, "thread-1")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, store.StatusActive, rec.Status)
}

func TestBridge_ThreadCreate_Idempotent(t *testing.T) {
	b, _, fc, _ := setupBridge(t)
	ctx := context.Background()

	evt := discord.Event{
		Type:      discord.EventThreadCreate,
		ChannelID: "channel-1",
		ThreadID:  "thread-1",
		UserID:    "user-1",
	}

	require.NoError(t, b.OnDiscordEvent(ctx, evt))
	require.NoError(t, b.OnDiscordEvent(ctx, evt)) // duplicate

	// Only one session created
	require.Len(t, fc.GetSessions(), 1)
}

func TestBridge_ThreadCreate_IgnoresNonForgeChannel(t *testing.T) {
	b, _, fc, _ := setupBridge(t)
	ctx := context.Background()

	err := b.OnDiscordEvent(ctx, discord.Event{
		Type:      discord.EventThreadCreate,
		ChannelID: "random-channel",
		ThreadID:  "thread-1",
		UserID:    "user-1",
	})
	require.NoError(t, err)
	require.Empty(t, fc.GetSessions())
}

func TestBridge_ThreadCreate_IgnoresBotUser(t *testing.T) {
	b, _, fc, _ := setupBridge(t)
	ctx := context.Background()

	err := b.OnDiscordEvent(ctx, discord.Event{
		Type:      discord.EventThreadCreate,
		ChannelID: "channel-1",
		ThreadID:  "thread-1",
		UserID:    "bot-123", // bot's own ID
	})
	require.NoError(t, err)
	require.Empty(t, fc.GetSessions())
}

func TestBridge_MessageCreate_ForwardsToForge(t *testing.T) {
	b, _, fc, st := setupBridge(t)
	ctx := context.Background()

	// Create session first
	now := time.Now()
	require.NoError(t, st.UpsertSession(ctx, store.SessionRecord{
		DiscordThreadID: "thread-1",
		ForgeSessionID:  "session-1",
		Status:          store.StatusActive,
		CreatedAt:       now,
		LastEventAt:     now,
	}))

	err := b.OnDiscordEvent(ctx, discord.Event{
		Type:     discord.EventMessageCreate,
		ThreadID: "thread-1",
		UserID:   "user-1",
		Content:  "Fix the bug in main.go",
	})
	require.NoError(t, err)

	msgs := fc.GetMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, "session-1", msgs[0].SessionID)
	require.Equal(t, "Fix the bug in main.go", msgs[0].Text)
}

func TestBridge_MessageCreate_IgnoresEmptyContent(t *testing.T) {
	b, _, fc, st := setupBridge(t)
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, st.UpsertSession(ctx, store.SessionRecord{
		DiscordThreadID: "thread-1",
		ForgeSessionID:  "session-1",
		Status:          store.StatusActive,
		CreatedAt:       now,
		LastEventAt:     now,
	}))

	err := b.OnDiscordEvent(ctx, discord.Event{
		Type:     discord.EventMessageCreate,
		ThreadID: "thread-1",
		UserID:   "user-1",
		Content:  "",
	})
	require.NoError(t, err)
	require.Empty(t, fc.GetMessages())
}

func TestBridge_ReactionPause_InterruptsSession(t *testing.T) {
	b, _, fc, st := setupBridge(t)
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, st.UpsertSession(ctx, store.SessionRecord{
		DiscordThreadID: "thread-1",
		ForgeSessionID:  "session-1",
		Status:          store.StatusActive,
		CreatedAt:       now,
		LastEventAt:     now,
	}))

	err := b.OnDiscordEvent(ctx, discord.Event{
		Type:      discord.EventReactionAdd,
		ThreadID:  "thread-1",
		MessageID: "msg-1",
		UserID:    "user-1",
		Emoji:     "⏸️",
	})
	require.NoError(t, err)

	interrupts := fc.GetInterrupts()
	require.Len(t, interrupts, 1)
	require.Equal(t, "session-1", interrupts[0])
}

func TestBridge_ReactionStop_ArchivesThread(t *testing.T) {
	b, dc, fc, st := setupBridge(t)
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, st.UpsertSession(ctx, store.SessionRecord{
		DiscordThreadID: "thread-1",
		ForgeSessionID:  "session-1",
		Status:          store.StatusActive,
		CreatedAt:       now,
		LastEventAt:     now,
	}))

	err := b.OnDiscordEvent(ctx, discord.Event{
		Type:      discord.EventReactionAdd,
		ThreadID:  "thread-1",
		MessageID: "starter-msg",
		UserID:    "user-1",
		Emoji:     "🛑",
	})
	require.NoError(t, err)

	// Session interrupted and archived
	require.NotEmpty(t, fc.GetInterrupts())
	require.Contains(t, dc.GetArchives(), "thread-1")

	rec, err := st.GetSessionByThread(ctx, "thread-1")
	require.NoError(t, err)
	require.Equal(t, store.StatusDropped, rec.Status)
}

func TestBridge_OnForgeEvent_IdempotentEvents(t *testing.T) {
	b, dc, _, st := setupBridge(t)
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, st.UpsertSession(ctx, store.SessionRecord{
		DiscordThreadID: "thread-1",
		ForgeSessionID:  "session-1",
		Status:          store.StatusActive,
		CreatedAt:       now,
		LastEventAt:     now,
	}))

	// Register translator
	b.mu.Lock()
	b.translators["thread-1"] = NewTranslator("thread-1", "", "session-1", false, false)
	b.mu.Unlock()

	evt := types.OutboundEvent{
		ID:      "evt-1",
		Type:    "text",
		Content: "Hello from Forge",
	}

	// First delivery
	require.NoError(t, b.OnForgeEvent(ctx, "thread-1", evt))

	// Flush with done
	doneEvt := types.OutboundEvent{ID: "evt-2", Type: "done"}
	require.NoError(t, b.OnForgeEvent(ctx, "thread-1", doneEvt))

	msgCount1 := len(dc.GetMessages())

	// Re-register translator for the replay test (done cleared it)
	b.mu.Lock()
	b.translators["thread-1"] = NewTranslator("thread-1", "", "session-1", false, false)
	b.mu.Unlock()

	// Replay same events — should be deduped
	require.NoError(t, b.OnForgeEvent(ctx, "thread-1", evt))
	require.NoError(t, b.OnForgeEvent(ctx, "thread-1", doneEvt))

	msgCount2 := len(dc.GetMessages())
	require.Equal(t, msgCount1, msgCount2, "duplicate events should not produce additional messages")
}

func TestBridge_RestartResilience(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// First bridge run — create a session
	st1, err := store.New(dbPath)
	require.NoError(t, err)

	ctx := context.Background()
	now := time.Now()
	require.NoError(t, st1.UpsertSession(ctx, store.SessionRecord{
		DiscordThreadID: "thread-1",
		ForgeSessionID:  "session-1",
		Status:          store.StatusActive,
		CreatedAt:       now,
		LastEventAt:     now,
		LastEventID:     "evt-5",
	}))

	// Record some events as already seen
	require.NoError(t, st1.RecordEvent(ctx, "session-1", "evt-1", "msg-1"))
	require.NoError(t, st1.RecordEvent(ctx, "session-1", "evt-2", "msg-2"))
	st1.Close()

	// Second bridge run — simulate restart
	st2, err := store.New(dbPath)
	require.NoError(t, err)
	defer st2.Close()

	// Active sessions should be recoverable
	sessions, err := st2.ListActiveSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "session-1", sessions[0].ForgeSessionID)
	require.Equal(t, "evt-2", sessions[0].LastEventID) // last_event_id updated by RecordEvent

	// Previously seen events should still be marked
	seen, err := st2.EventSeen(ctx, "session-1", "evt-1")
	require.NoError(t, err)
	require.True(t, seen)

	seen, err = st2.EventSeen(ctx, "session-1", "evt-99")
	require.NoError(t, err)
	require.False(t, seen)
}

// SetChannelsForTest is a test helper on Config.
func (c *Config) SetChannelsForTest(cfg ChannelsConfig) {
	c.mu.Lock()
	c.channels = cfg
	c.mu.Unlock()
}
