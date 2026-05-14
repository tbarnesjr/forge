package forge

import (
	"context"
	"fmt"
	"sync"

	"github.com/jelmersnoeck/forge/internal/types"
)

// StubClient is a test double for Client.
type StubClient struct {
	mu       sync.Mutex
	Sessions []StubSession
	Messages []StubMsg
	Interrupts []string

	sessionSeq int
	eventChans map[string]chan types.OutboundEvent

	// CreateError, if set, is returned by CreateSession.
	CreateError error
	// SendError, if set, is returned by SendMessage.
	SendError error
	// HealthyResult controls Healthy return value (default true).
	HealthyResult bool
}

// StubSession records a CreateSession call.
type StubSession struct {
	CWD      string
	Metadata map[string]any
}

// StubMsg records a SendMessage call.
type StubMsg struct {
	SessionID string
	Text      string
}

// NewStubClient returns a stub ready for use in tests.
func NewStubClient() *StubClient {
	return &StubClient{
		eventChans:    make(map[string]chan types.OutboundEvent),
		HealthyResult: true,
	}
}

func (s *StubClient) CreateSession(_ context.Context, cwd string, metadata map[string]any) (string, error) {
	if s.CreateError != nil {
		return "", s.CreateError
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionSeq++
	s.Sessions = append(s.Sessions, StubSession{CWD: cwd, Metadata: metadata})
	return fmt.Sprintf("session-%d", s.sessionSeq), nil
}

func (s *StubClient) SendMessage(_ context.Context, sessionID, text string) error {
	if s.SendError != nil {
		return s.SendError
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = append(s.Messages, StubMsg{SessionID: sessionID, Text: text})
	return nil
}

func (s *StubClient) Interrupt(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Interrupts = append(s.Interrupts, sessionID)
	return nil
}

func (s *StubClient) SubscribeEvents(_ context.Context, sessionID string) (<-chan types.OutboundEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan types.OutboundEvent, 64)
	s.eventChans[sessionID] = ch
	return ch, nil
}

func (s *StubClient) Healthy(_ context.Context) bool {
	return s.HealthyResult
}

// EmitEvent sends an event to the channel for a given session (for tests).
func (s *StubClient) EmitEvent(sessionID string, evt types.OutboundEvent) {
	s.mu.Lock()
	ch, ok := s.eventChans[sessionID]
	s.mu.Unlock()
	if ok {
		ch <- evt
	}
}

// CloseEvents closes the event channel for a session (simulates stream end).
func (s *StubClient) CloseEvents(sessionID string) {
	s.mu.Lock()
	ch, ok := s.eventChans[sessionID]
	s.mu.Unlock()
	if ok {
		close(ch)
	}
}

// GetSessions returns a snapshot of created sessions.
func (s *StubClient) GetSessions() []StubSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StubSession, len(s.Sessions))
	copy(out, s.Sessions)
	return out
}

// GetMessages returns a snapshot of sent messages.
func (s *StubClient) GetMessages() []StubMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StubMsg, len(s.Messages))
	copy(out, s.Messages)
	return out
}

// GetInterrupts returns a snapshot of interrupt calls.
func (s *StubClient) GetInterrupts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.Interrupts))
	copy(out, s.Interrupts)
	return out
}
