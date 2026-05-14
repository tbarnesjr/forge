// Package discord wraps discordgo for the bridge's needs.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// EventType identifies a Discord event relevant to the bridge.
type EventType string

const (
	EventThreadCreate    EventType = "thread_create"
	EventMessageCreate   EventType = "message_create"
	EventReactionAdd     EventType = "reaction_add"
	EventThreadUpdate    EventType = "thread_update"
	EventGuildReady      EventType = "guild_ready"
	EventDisconnect      EventType = "disconnect"
	EventReconnect       EventType = "reconnect"
)

// Event is a normalized Discord event for the bridge.
type Event struct {
	Type      EventType
	GuildID   string
	ChannelID string // parent channel for threads
	ThreadID  string
	MessageID string
	UserID    string
	Username  string
	Content   string
	BotUser   bool

	// Reaction-specific
	Emoji string

	// Thread-specific
	ThreadArchived bool
}

// PostOption configures a message post.
type PostOption func(*postConfig)

type postConfig struct {
	embed *discordgo.MessageEmbed
	pin   bool
}

// WithEmbed adds an embed to the message.
func WithEmbed(embed *discordgo.MessageEmbed) PostOption {
	return func(c *postConfig) { c.embed = embed }
}

// WithPin pins the message after posting.
func WithPin() PostOption {
	return func(c *postConfig) { c.pin = true }
}

// Client is the Discord operations interface used by the bridge.
type Client interface {
	PostMessage(ctx context.Context, threadID, content string, opts ...PostOption) (messageID string, err error)
	EditMessage(ctx context.Context, threadID, messageID, content string) error
	EditMessageEmbed(ctx context.Context, threadID, messageID string, embed *discordgo.MessageEmbed) error
	AddReaction(ctx context.Context, threadID, messageID, emoji string) error
	RemoveReaction(ctx context.Context, threadID, messageID, emoji string) error
	ArchiveThread(ctx context.Context, threadID string) error
	PinMessage(ctx context.Context, threadID, messageID string) error
	UpdateStatus(ctx context.Context, status string) error
	SubscribeEvents(ctx context.Context) (<-chan Event, error)
	BotUserID() string
	Close() error
}

// LiveClient implements Client with a real Discord connection.
type LiveClient struct {
	session  *discordgo.Session
	guildID  string
	botID    string
	logger   *slog.Logger

	mu       sync.Mutex
	eventChs []chan Event
	closed   bool
}

// NewLiveClient opens a Discord gateway connection.
func NewLiveClient(token, guildID string, logger *slog.Logger) (*LiveClient, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}

	s.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildMessageReactions |
		discordgo.IntentsGuilds

	c := &LiveClient{
		session: s,
		guildID: guildID,
		logger:  logger,
	}

	s.AddHandler(c.onReady)
	s.AddHandler(c.onThreadCreate)
	s.AddHandler(c.onMessageCreate)
	s.AddHandler(c.onReactionAdd)
	s.AddHandler(c.onThreadUpdate)
	s.AddHandler(c.onDisconnect)
	s.AddHandler(c.onResumed)

	if err := s.Open(); err != nil {
		return nil, fmt.Errorf("open discord ws: %w", err)
	}

	return c, nil
}

func (c *LiveClient) BotUserID() string { return c.botID }

func (c *LiveClient) Close() error {
	c.mu.Lock()
	c.closed = true
	for _, ch := range c.eventChs {
		close(ch)
	}
	c.eventChs = nil
	c.mu.Unlock()
	return c.session.Close()
}

func (c *LiveClient) emit(evt Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	for _, ch := range c.eventChs {
		select {
		case ch <- evt:
		default:
			c.logger.Warn("dropping discord event, channel full", "type", evt.Type)
		}
	}
}

func (c *LiveClient) SubscribeEvents(_ context.Context) (<-chan Event, error) {
	ch := make(chan Event, 100)
	c.mu.Lock()
	c.eventChs = append(c.eventChs, ch)
	c.mu.Unlock()
	return ch, nil
}

func (c *LiveClient) PostMessage(_ context.Context, threadID, content string, opts ...PostOption) (string, error) {
	cfg := &postConfig{}
	for _, o := range opts {
		o(cfg)
	}

	msg := &discordgo.MessageSend{Content: content}
	if cfg.embed != nil {
		msg.Embeds = []*discordgo.MessageEmbed{cfg.embed}
	}

	m, err := c.session.ChannelMessageSendComplex(threadID, msg)
	if err != nil {
		return "", err
	}

	if cfg.pin {
		_ = c.session.ChannelMessagePin(threadID, m.ID)
	}

	return m.ID, nil
}

func (c *LiveClient) EditMessage(_ context.Context, threadID, messageID, content string) error {
	_, err := c.session.ChannelMessageEdit(threadID, messageID, content)
	return err
}

func (c *LiveClient) EditMessageEmbed(_ context.Context, threadID, messageID string, embed *discordgo.MessageEmbed) error {
	edit := &discordgo.MessageEdit{
		Channel: threadID,
		ID:      messageID,
		Embeds:  &[]*discordgo.MessageEmbed{embed},
	}
	_, err := c.session.ChannelMessageEditComplex(edit)
	return err
}

func (c *LiveClient) AddReaction(_ context.Context, threadID, messageID, emoji string) error {
	return c.session.MessageReactionAdd(threadID, messageID, emoji)
}

func (c *LiveClient) RemoveReaction(_ context.Context, threadID, messageID, emoji string) error {
	return c.session.MessageReactionRemove(threadID, messageID, emoji, c.botID)
}

func (c *LiveClient) ArchiveThread(_ context.Context, threadID string) error {
	archived := true
	_, err := c.session.ChannelEdit(threadID, &discordgo.ChannelEdit{
		Archived: &archived,
	})
	return err
}

func (c *LiveClient) PinMessage(_ context.Context, threadID, messageID string) error {
	return c.session.ChannelMessagePin(threadID, messageID)
}

func (c *LiveClient) UpdateStatus(_ context.Context, status string) error {
	return c.session.UpdateCustomStatus(status)
}

// ── Discord event handlers ─────────────────────────────────────

func (c *LiveClient) onReady(_ *discordgo.Session, r *discordgo.Ready) {
	c.botID = r.User.ID
	c.logger.Info("discord ready", "bot_id", c.botID, "username", r.User.Username)
	c.emit(Event{Type: EventGuildReady})
}

func (c *LiveClient) onThreadCreate(_ *discordgo.Session, t *discordgo.ThreadCreate) {
	if t.GuildID != c.guildID {
		return
	}
	c.emit(Event{
		Type:      EventThreadCreate,
		GuildID:   t.GuildID,
		ChannelID: t.ParentID,
		ThreadID:  t.ID,
		UserID:    t.OwnerID,
	})
}

func (c *LiveClient) onMessageCreate(_ *discordgo.Session, m *discordgo.MessageCreate) {
	if m.GuildID != c.guildID {
		return
	}
	if m.Author == nil {
		return
	}

	evt := Event{
		Type:      EventMessageCreate,
		GuildID:   m.GuildID,
		ThreadID:  m.ChannelID,
		MessageID: m.ID,
		UserID:    m.Author.ID,
		Username:  m.Author.Username,
		Content:   m.Content,
		BotUser:   m.Author.Bot,
	}

	// Resolve parent channel for thread messages
	ch, err := c.session.Channel(m.ChannelID)
	if err == nil && ch.IsThread() {
		evt.ChannelID = ch.ParentID
	}

	c.emit(evt)
}

func (c *LiveClient) onReactionAdd(_ *discordgo.Session, r *discordgo.MessageReactionAdd) {
	if r.GuildID != c.guildID {
		return
	}
	c.emit(Event{
		Type:      EventReactionAdd,
		GuildID:   r.GuildID,
		ThreadID:  r.ChannelID,
		MessageID: r.MessageID,
		UserID:    r.UserID,
		Emoji:     r.Emoji.Name,
	})
}

func (c *LiveClient) onThreadUpdate(_ *discordgo.Session, t *discordgo.ThreadUpdate) {
	if t.GuildID != c.guildID {
		return
	}
	c.emit(Event{
		Type:           EventThreadUpdate,
		GuildID:        t.GuildID,
		ChannelID:      t.ParentID,
		ThreadID:       t.ID,
		ThreadArchived: t.ThreadMetadata != nil && t.ThreadMetadata.Archived,
	})
}

func (c *LiveClient) onDisconnect(_ *discordgo.Session, _ *discordgo.Disconnect) {
	c.logger.Warn("discord disconnected")
	c.emit(Event{Type: EventDisconnect})
}

func (c *LiveClient) onResumed(_ *discordgo.Session, _ *discordgo.Resumed) {
	c.logger.Info("discord reconnected")
	c.emit(Event{Type: EventReconnect})
}
