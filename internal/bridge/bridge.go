package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jelmersnoeck/forge/internal/discord"
	"github.com/jelmersnoeck/forge/internal/forge"
	"github.com/jelmersnoeck/forge/internal/store"
	"github.com/jelmersnoeck/forge/internal/types"
)

// Bridge connects Discord threads to Forge sessions.
type Bridge struct {
	forge   forge.Client
	discord discord.Client
	store   store.Store
	cfg     *Config
	logger  *slog.Logger

	// Active session translators, keyed by threadID
	mu          sync.Mutex
	translators map[string]*Translator
	cancelFns   map[string]context.CancelFunc

	// Active session count for status updates
	activeCount int
}

// New creates a new Bridge.
func New(f forge.Client, d discord.Client, s store.Store, cfg *Config, logger *slog.Logger) *Bridge {
	return &Bridge{
		forge:       f,
		discord:     d,
		store:       s,
		cfg:         cfg,
		logger:      logger,
		translators: make(map[string]*Translator),
		cancelFns:   make(map[string]context.CancelFunc),
	}
}

// Run starts the bridge event loop. Blocks until ctx is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	// Resume active sessions from store
	if err := b.resumeSessions(ctx); err != nil {
		b.logger.Error("failed to resume sessions", "error", err)
	}

	events, err := b.discord.SubscribeEvents(ctx)
	if err != nil {
		return fmt.Errorf("subscribe discord events: %w", err)
	}

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				return nil
			}
			if err := b.OnDiscordEvent(ctx, evt); err != nil {
				b.logger.Error("discord event error", "type", evt.Type, "error", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// OnDiscordEvent handles a Discord event.
func (b *Bridge) OnDiscordEvent(ctx context.Context, evt discord.Event) error {
	// Never respond to our own messages
	if evt.UserID == b.discord.BotUserID() || evt.BotUser {
		return nil
	}

	switch evt.Type {
	case discord.EventThreadCreate:
		return b.onThreadCreate(ctx, evt)
	case discord.EventMessageCreate:
		return b.onMessageCreate(ctx, evt)
	case discord.EventReactionAdd:
		return b.onReactionAdd(ctx, evt)
	case discord.EventThreadUpdate:
		return b.onThreadUpdate(ctx, evt)
	case discord.EventReconnect:
		return b.onReconnect(ctx)
	default:
		return nil
	}
}

func (b *Bridge) onThreadCreate(ctx context.Context, evt discord.Event) error {
	// Only handle threads in configured channels
	if !b.cfg.IsForgeChannel(evt.ChannelID) {
		return nil
	}

	// Check idempotency — thread may already have a session
	existing, err := b.store.GetSessionByThread(ctx, evt.ThreadID)
	if err != nil {
		return fmt.Errorf("check existing session: %w", err)
	}
	if existing != nil {
		b.logger.Info("thread already has session, ignoring duplicate THREAD_CREATE",
			"thread", evt.ThreadID, "session", existing.ForgeSessionID)
		return nil
	}

	// Check user authorization
	if !b.cfg.IsUserAllowed(evt.ChannelID, evt.UserID) {
		b.logger.Info("user not allowed", "user", evt.UserID, "channel", evt.ChannelID)
		return nil
	}

	cc := b.cfg.GetChannelConfig(evt.ChannelID)
	if cc == nil {
		return nil
	}

	// Create Forge session
	metadata := map[string]any{
		"source":                  "discord",
		"discord.guildId":        evt.GuildID,
		"discord.channelId":     evt.ChannelID,
		"discord.threadId":      evt.ThreadID,
		"discord.userId":        evt.UserID,
		"discord.username":      evt.Username,
	}

	sessionID, err := b.forge.CreateSession(ctx, cc.RepoPath, metadata)
	if err != nil {
		b.logger.Error("failed to create forge session", "error", err)
		// Post retry message to thread
		_, _ = b.discord.PostMessage(ctx, evt.ThreadID,
			"⏳ Forge is unreachable. Will retry…")
		return fmt.Errorf("create forge session: %w", err)
	}

	// Store mapping
	now := time.Now()
	if err := b.store.UpsertSession(ctx, store.SessionRecord{
		DiscordThreadID: evt.ThreadID,
		ForgeSessionID:  sessionID,
		Status:          store.StatusActive,
		CreatedAt:       now,
		LastEventAt:     now,
	}); err != nil {
		return fmt.Errorf("store session: %w", err)
	}

	b.logger.Info("session created",
		"thread", evt.ThreadID, "session", sessionID)

	// Start SSE relay
	b.startSSERelay(ctx, evt.ThreadID, sessionID, "")

	// Update bot status
	b.updateActiveCount(1)

	return nil
}

func (b *Bridge) onMessageCreate(ctx context.Context, evt discord.Event) error {
	if evt.Content == "" {
		return nil
	}

	// Look up session for this thread
	rec, err := b.store.GetSessionByThread(ctx, evt.ThreadID)
	if err != nil {
		return err
	}

	if rec == nil {
		// Not a managed thread. If it's a forge channel, this could be the
		// starter message for a forum thread. The THREAD_CREATE event handles
		// that — the message might arrive first, which is fine.
		return nil
	}

	if rec.Status != store.StatusActive {
		return nil
	}

	// Check if this is the first message (starter) — if so, also send to Forge
	// Forward to Forge
	if err := b.forge.SendMessage(ctx, rec.ForgeSessionID, evt.Content); err != nil {
		b.logger.Error("failed to send message to forge",
			"session", rec.ForgeSessionID, "error", err)
		return err
	}

	return nil
}

func (b *Bridge) onReactionAdd(ctx context.Context, evt discord.Event) error {
	rec, err := b.store.GetSessionByThread(ctx, evt.ThreadID)
	if err != nil || rec == nil || rec.Status != store.StatusActive {
		return err
	}

	switch evt.Emoji {
	case "⏸", "⏸️":
		return b.forge.Interrupt(ctx, rec.ForgeSessionID)

	case "🔁":
		// TODO: Re-send prior human message. For v1, log and ignore.
		b.logger.Info("retry reaction received", "thread", evt.ThreadID)
		return nil

	case "🛑":
		// Only on starter message
		_ = b.forge.Interrupt(ctx, rec.ForgeSessionID)
		_ = b.discord.ArchiveThread(ctx, evt.ThreadID)
		rec.Status = store.StatusDropped
		return b.store.UpsertSession(ctx, *rec)
	}

	return nil
}

func (b *Bridge) onThreadUpdate(ctx context.Context, evt discord.Event) error {
	if !evt.ThreadArchived {
		return nil
	}

	rec, err := b.store.GetSessionByThread(ctx, evt.ThreadID)
	if err != nil || rec == nil || rec.Status != store.StatusActive {
		return err
	}

	// Thread archived — interrupt and drop
	_ = b.forge.Interrupt(ctx, rec.ForgeSessionID)
	rec.Status = store.StatusDropped
	b.cancelRelay(evt.ThreadID)
	b.updateActiveCount(-1)
	return b.store.UpsertSession(ctx, *rec)
}

func (b *Bridge) onReconnect(ctx context.Context) error {
	// Post warning on active threads
	b.mu.Lock()
	threads := make([]string, 0, len(b.translators))
	for tid := range b.translators {
		threads = append(threads, tid)
	}
	b.mu.Unlock()

	for _, tid := range threads {
		_ = b.discord.AddReaction(ctx, tid, "", "⚠️")
	}
	return nil
}

// OnForgeEvent handles a Forge SSE event for a specific thread.
func (b *Bridge) OnForgeEvent(ctx context.Context, threadID string, evt types.OutboundEvent) error {
	rec, err := b.store.GetSessionByThread(ctx, threadID)
	if err != nil || rec == nil {
		return err
	}

	// Idempotency check
	if evt.ID != "" {
		seen, err := b.store.EventSeen(ctx, rec.ForgeSessionID, evt.ID)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}
	}

	// Translate
	b.mu.Lock()
	tr, ok := b.translators[threadID]
	b.mu.Unlock()
	if !ok {
		return nil
	}

	actions := tr.Translate(evt)

	// Execute actions
	for _, action := range actions {
		msgID, err := b.executeAction(ctx, action)
		if err != nil {
			b.logger.Error("failed to execute discord action",
				"action", action.Type, "thread", threadID, "error", err)
			continue
		}

		// Track bot message IDs
		if action.Type == ActionPost && msgID != "" {
			tr.SetLastBotMsgID(msgID)
			// Track tool embed messages
			if action.Embed != nil && action.Embed.Footer != nil &&
				action.Embed.Footer.Text == "running…" {
				tr.toolMsgID = msgID
			}
		}
	}

	// Record event for idempotency
	if evt.ID != "" {
		lastMsgID := ""
		b.mu.Lock()
		if t, ok := b.translators[threadID]; ok {
			lastMsgID = t.lastBotMsgID
		}
		b.mu.Unlock()
		_ = b.store.RecordEvent(ctx, rec.ForgeSessionID, evt.ID, lastMsgID)
	}

	// Handle session completion
	if evt.Type == "done" {
		rec.Status = store.StatusComplete
		_ = b.store.UpsertSession(ctx, *rec)
		b.cancelRelay(threadID)
		b.updateActiveCount(-1)
	}

	return nil
}

func (b *Bridge) executeAction(ctx context.Context, action DiscordAction) (string, error) {
	switch action.Type {
	case ActionPost:
		var opts []discord.PostOption
		if action.Embed != nil {
			opts = append(opts, discord.WithEmbed(action.Embed))
		}
		if action.Pin {
			opts = append(opts, discord.WithPin())
		}
		return b.discord.PostMessage(ctx, action.ThreadID, action.Content, opts...)

	case ActionEdit:
		return "", b.discord.EditMessage(ctx, action.ThreadID, action.MessageID, action.Content)

	case ActionEditEmbed:
		return "", b.discord.EditMessageEmbed(ctx, action.ThreadID, action.MessageID, action.Embed)

	case ActionReact:
		return "", b.discord.AddReaction(ctx, action.ThreadID, action.MessageID, action.Emoji)

	case ActionRemoveReact:
		return "", b.discord.RemoveReaction(ctx, action.ThreadID, action.MessageID, action.Emoji)

	case ActionPin:
		return "", b.discord.PinMessage(ctx, action.ThreadID, action.MessageID)

	default:
		return "", fmt.Errorf("unknown action type: %d", action.Type)
	}
}

// startSSERelay begins streaming Forge events for a session.
func (b *Bridge) startSSERelay(ctx context.Context, threadID, sessionID, lastEventID string) {
	subCtx, cancel := context.WithCancel(ctx)

	b.mu.Lock()
	// Create translator
	tr := NewTranslator(threadID, "", sessionID,
		b.cfg.ShowThinking, b.cfg.RevealSessionID)
	b.translators[threadID] = tr
	b.cancelFns[threadID] = cancel
	b.mu.Unlock()

	go func() {
		defer cancel()

		events, err := b.forge.SubscribeEvents(subCtx, sessionID)
		if err != nil {
			b.logger.Error("failed to subscribe to forge events",
				"session", sessionID, "error", err)
			return
		}

		for {
			select {
			case evt, ok := <-events:
				if !ok {
					// Stream ended — try to reconnect with backoff
					b.handleSSEDisconnect(ctx, threadID, sessionID)
					return
				}
				if err := b.OnForgeEvent(subCtx, threadID, evt); err != nil {
					b.logger.Error("forge event error",
						"session", sessionID, "type", evt.Type, "error", err)
				}
			case <-subCtx.Done():
				return
			}
		}
	}()
}

func (b *Bridge) handleSSEDisconnect(ctx context.Context, threadID, sessionID string) {
	rec, err := b.store.GetSessionByThread(ctx, threadID)
	if err != nil || rec == nil || rec.Status != store.StatusActive {
		return
	}

	b.logger.Warn("SSE disconnected, attempting reconnect",
		"session", sessionID, "thread", threadID)

	// Post warning reaction
	b.mu.Lock()
	tr, ok := b.translators[threadID]
	b.mu.Unlock()
	if ok && tr.lastBotMsgID != "" {
		_ = b.discord.AddReaction(ctx, threadID, tr.lastBotMsgID, "⚠️")
	}

	// Reconnect with backoff
	backoff := time.Second
	for i := 0; i < 5; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		b.startSSERelay(ctx, threadID, sessionID, rec.LastEventID)
		return
	}
}

func (b *Bridge) cancelRelay(threadID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cancel, ok := b.cancelFns[threadID]; ok {
		cancel()
		delete(b.cancelFns, threadID)
	}
	delete(b.translators, threadID)
}

func (b *Bridge) resumeSessions(ctx context.Context) error {
	sessions, err := b.store.ListActiveSessions(ctx)
	if err != nil {
		return err
	}

	for _, rec := range sessions {
		b.logger.Info("resuming session",
			"thread", rec.DiscordThreadID, "session", rec.ForgeSessionID)
		b.startSSERelay(ctx, rec.DiscordThreadID, rec.ForgeSessionID, rec.LastEventID)
		b.activeCount++
	}

	b.updateStatus()
	return nil
}

func (b *Bridge) updateActiveCount(delta int) {
	b.mu.Lock()
	b.activeCount += delta
	b.mu.Unlock()
	b.updateStatus()
}

func (b *Bridge) updateStatus() {
	b.mu.Lock()
	count := b.activeCount
	b.mu.Unlock()
	status := fmt.Sprintf("Watching: %d sessions", count)
	_ = b.discord.UpdateStatus(context.Background(), status)
}

// ActiveSessionCount returns the number of active sessions.
func (b *Bridge) ActiveSessionCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeCount
}
