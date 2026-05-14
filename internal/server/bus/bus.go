// Package bus provides an in-memory message queue and event pub/sub for
// session-based communication between the gateway and workers.
//
//	pushMessage  → gateway enqueues work
//	pullMessage  → worker blocks until a message arrives
//	publishEvent → worker emits output
//	subscribe    → SSE handler receives output
//
// Uses a waiter list: if a worker is already waiting when a message
// arrives, we resolve its channel directly — no polling, no timers.
package bus

import (
	"sync"

	"github.com/jelmersnoeck/forge/internal/types"
)

var (
	sessions = struct {
		sync.RWMutex
		m map[string]*types.SessionMeta
	}{m: make(map[string]*types.SessionMeta)}

	queues = struct {
		sync.Mutex
		m map[string][]types.InboundMessage
	}{m: make(map[string][]types.InboundMessage)}

	waiters = struct {
		sync.Mutex
		m map[string][]chan types.InboundMessage
	}{m: make(map[string][]chan types.InboundMessage)}

	subs = struct {
		sync.RWMutex
		m map[string][]chan types.OutboundEvent
	}{m: make(map[string][]chan types.OutboundEvent)}
)

// GetSession returns session metadata, or nil if not found.
func GetSession(sessionID string) *types.SessionMeta {
	sessions.RLock()
	defer sessions.RUnlock()
	meta, ok := sessions.m[sessionID]
	if !ok {
		return nil
	}
	cp := *meta
	return &cp
}

// SetSession stores session metadata.
func SetSession(meta *types.SessionMeta) {
	sessions.Lock()
	defer sessions.Unlock()
	sessions.m[meta.SessionID] = meta
}

// PushMessage enqueues a message for a session's worker.
func PushMessage(sessionID string, msg types.InboundMessage) {
	waiters.Lock()
	if ws, ok := waiters.m[sessionID]; ok && len(ws) > 0 {
		ch := ws[0]
		waiters.m[sessionID] = ws[1:]
		waiters.Unlock()
		ch <- msg
		return
	}
	waiters.Unlock()

	queues.Lock()
	defer queues.Unlock()
	queues.m[sessionID] = append(queues.m[sessionID], msg)
}

// PullMessage blocks until a message is available for the session.
func PullMessage(sessionID string) types.InboundMessage {
	queues.Lock()
	if q, ok := queues.m[sessionID]; ok && len(q) > 0 {
		msg := q[0]
		queues.m[sessionID] = q[1:]
		queues.Unlock()
		return msg
	}
	queues.Unlock()

	ch := make(chan types.InboundMessage, 1)
	waiters.Lock()
	waiters.m[sessionID] = append(waiters.m[sessionID], ch)
	waiters.Unlock()

	return <-ch
}

// PublishEvent sends an event to all subscribers for a session.
func PublishEvent(sessionID string, event types.OutboundEvent) {
	subs.RLock()
	defer subs.RUnlock()
	for _, ch := range subs.m[sessionID] {
		select {
		case ch <- event:
		default:
			// subscriber too slow, drop event
		}
	}
}

// ListSessions returns a snapshot of all active session metadata.
// The returned slice is safe to use without holding the lock.
func ListSessions() []*types.SessionMeta {
	sessions.RLock()
	defer sessions.RUnlock()
	result := make([]*types.SessionMeta, 0, len(sessions.m))
	for _, meta := range sessions.m {
		cp := *meta
		result = append(result, &cp)
	}
	return result
}

// Subscribe returns a channel of events for a session and an unsubscribe function.
func Subscribe(sessionID string) (<-chan types.OutboundEvent, func()) {
	ch := make(chan types.OutboundEvent, 64)

	subs.Lock()
	subs.m[sessionID] = append(subs.m[sessionID], ch)
	subs.Unlock()

	unsub := func() {
		subs.Lock()
		defer subs.Unlock()
		list := subs.m[sessionID]
		for i, c := range list {
			if c == ch {
				subs.m[sessionID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		close(ch)
	}

	return ch, unsub
}
