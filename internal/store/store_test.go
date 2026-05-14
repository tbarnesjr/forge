package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testDB(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNew_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bridge.db")
	s, err := New(dbPath)
	require.NoError(t, err)
	defer s.Close()

	_, err = os.Stat(dbPath)
	require.NoError(t, err)
}

func TestUpsertSession_InsertAndUpdate(t *testing.T) {
	s := testDB(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	rec := SessionRecord{
		DiscordThreadID: "thread-1",
		ForgeSessionID:  "session-abc",
		Status:          StatusActive,
		CreatedAt:       now,
		LastEventAt:     now,
		LastEventID:     "",
	}
	require.NoError(t, s.UpsertSession(ctx, rec))

	got, err := s.GetSessionByThread(ctx, "thread-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "session-abc", got.ForgeSessionID)
	require.Equal(t, StatusActive, got.Status)

	// Update
	rec.Status = StatusComplete
	rec.LastEventID = "evt-42"
	require.NoError(t, s.UpsertSession(ctx, rec))

	got, err = s.GetSessionByThread(ctx, "thread-1")
	require.NoError(t, err)
	require.Equal(t, StatusComplete, got.Status)
	require.Equal(t, "evt-42", got.LastEventID)
}

func TestGetSessionByThread_NotFound(t *testing.T) {
	s := testDB(t)
	got, err := s.GetSessionByThread(context.Background(), "nonexistent")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestListActiveSessions(t *testing.T) {
	s := testDB(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.UpsertSession(ctx, SessionRecord{
		DiscordThreadID: "t1", ForgeSessionID: "s1", Status: StatusActive,
		CreatedAt: now, LastEventAt: now,
	}))
	require.NoError(t, s.UpsertSession(ctx, SessionRecord{
		DiscordThreadID: "t2", ForgeSessionID: "s2", Status: StatusComplete,
		CreatedAt: now, LastEventAt: now,
	}))
	require.NoError(t, s.UpsertSession(ctx, SessionRecord{
		DiscordThreadID: "t3", ForgeSessionID: "s3", Status: StatusActive,
		CreatedAt: now, LastEventAt: now,
	}))

	list, err := s.ListActiveSessions(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestEventIdempotency(t *testing.T) {
	s := testDB(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, s.UpsertSession(ctx, SessionRecord{
		DiscordThreadID: "t1", ForgeSessionID: "s1", Status: StatusActive,
		CreatedAt: now, LastEventAt: now,
	}))

	// Record an event
	require.NoError(t, s.RecordEvent(ctx, "s1", "evt-1", "msg-1"))

	seen, err := s.EventSeen(ctx, "s1", "evt-1")
	require.NoError(t, err)
	require.True(t, seen)

	seen, err = s.EventSeen(ctx, "s1", "evt-2")
	require.NoError(t, err)
	require.False(t, seen)

	// Duplicate insert should not error (INSERT OR IGNORE)
	require.NoError(t, s.RecordEvent(ctx, "s1", "evt-1", "msg-1-dup"))
}

func TestPendingQueue(t *testing.T) {
	s := testDB(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	// Enqueue two items
	require.NoError(t, s.EnqueuePending(ctx, PendingMessage{
		DiscordMessageID: "dm-1", ThreadID: "t1", Body: "hello",
		RetryCount: 0, NextRetryAt: now.Add(-time.Minute),
	}))
	require.NoError(t, s.EnqueuePending(ctx, PendingMessage{
		DiscordMessageID: "dm-2", ThreadID: "t1", Body: "world",
		RetryCount: 0, NextRetryAt: now.Add(time.Hour), // future
	}))

	// Only dm-1 is due
	items, err := s.DequeueDuePending(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "dm-1", items[0].DiscordMessageID)

	// dm-1 is gone
	items, err = s.DequeueDuePending(ctx, now, 10)
	require.NoError(t, err)
	require.Empty(t, items)
}
