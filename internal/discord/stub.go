package discord

import (
	"context"
	"fmt"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// StubClient is a test double for Client.
type StubClient struct {
	mu       sync.Mutex
	Messages []StubMessage
	Edits    []StubEdit
	Embeds   []StubEmbedEdit
	Reactions []StubReaction
	Removed   []StubReaction
	Archives  []string
	Pins      []StubPin
	Statuses  []string

	botID    string
	eventCh  chan Event
	msgIDSeq int

	// PostError, if set, is returned by PostMessage.
	PostError error
}

// StubMessage records a PostMessage call.
type StubMessage struct {
	ThreadID string
	Content  string
	Embed    *discordgo.MessageEmbed
	Pin      bool
}

// StubEdit records an EditMessage call.
type StubEdit struct {
	ThreadID  string
	MessageID string
	Content   string
}

// StubEmbedEdit records an EditMessageEmbed call.
type StubEmbedEdit struct {
	ThreadID  string
	MessageID string
	Embed     *discordgo.MessageEmbed
}

// StubReaction records a reaction add/remove.
type StubReaction struct {
	ThreadID  string
	MessageID string
	Emoji     string
}

// StubPin records a pin call.
type StubPin struct {
	ThreadID  string
	MessageID string
}

// NewStubClient returns a stub ready for use in tests.
func NewStubClient(botID string) *StubClient {
	return &StubClient{
		botID:   botID,
		eventCh: make(chan Event, 100),
	}
}

func (s *StubClient) BotUserID() string { return s.botID }
func (s *StubClient) Close() error      { return nil }

func (s *StubClient) PostMessage(_ context.Context, threadID, content string, opts ...PostOption) (string, error) {
	if s.PostError != nil {
		return "", s.PostError
	}

	cfg := &postConfig{}
	for _, o := range opts {
		o(cfg)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgIDSeq++
	msg := StubMessage{
		ThreadID: threadID,
		Content:  content,
		Embed:    cfg.embed,
		Pin:      cfg.pin,
	}
	s.Messages = append(s.Messages, msg)
	return fmt.Sprintf("msg-%d", s.msgIDSeq), nil
}

func (s *StubClient) EditMessage(_ context.Context, threadID, messageID, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Edits = append(s.Edits, StubEdit{ThreadID: threadID, MessageID: messageID, Content: content})
	return nil
}

func (s *StubClient) EditMessageEmbed(_ context.Context, threadID, messageID string, embed *discordgo.MessageEmbed) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Embeds = append(s.Embeds, StubEmbedEdit{ThreadID: threadID, MessageID: messageID, Embed: embed})
	return nil
}

func (s *StubClient) AddReaction(_ context.Context, threadID, messageID, emoji string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Reactions = append(s.Reactions, StubReaction{ThreadID: threadID, MessageID: messageID, Emoji: emoji})
	return nil
}

func (s *StubClient) RemoveReaction(_ context.Context, threadID, messageID, emoji string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Removed = append(s.Removed, StubReaction{ThreadID: threadID, MessageID: messageID, Emoji: emoji})
	return nil
}

func (s *StubClient) ArchiveThread(_ context.Context, threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Archives = append(s.Archives, threadID)
	return nil
}

func (s *StubClient) PinMessage(_ context.Context, threadID, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Pins = append(s.Pins, StubPin{ThreadID: threadID, MessageID: messageID})
	return nil
}

func (s *StubClient) UpdateStatus(_ context.Context, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Statuses = append(s.Statuses, status)
	return nil
}

func (s *StubClient) SubscribeEvents(_ context.Context) (<-chan Event, error) {
	return s.eventCh, nil
}

// Send injects an event into the stub's event channel (for tests).
func (s *StubClient) Send(evt Event) {
	s.eventCh <- evt
}

// GetMessages returns a snapshot of posted messages.
func (s *StubClient) GetMessages() []StubMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StubMessage, len(s.Messages))
	copy(out, s.Messages)
	return out
}

// GetReactions returns a snapshot of added reactions.
func (s *StubClient) GetReactions() []StubReaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StubReaction, len(s.Reactions))
	copy(out, s.Reactions)
	return out
}

// GetArchives returns a snapshot of archived thread IDs.
func (s *StubClient) GetArchives() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.Archives))
	copy(out, s.Archives)
	return out
}
