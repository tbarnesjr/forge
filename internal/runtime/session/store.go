// Package session provides JSONL-based session persistence.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jelmersnoeck/forge/internal/types"
)

// Store persists session messages to JSONL files.
type Store struct {
	baseDir string
	mu      sync.RWMutex
}

// NewStore creates a new session store.
func NewStore(baseDir string) *Store {
	return &Store{
		baseDir: baseDir,
	}
}

// Append writes a session message to the JSONL file for the given session.
func (s *Store) Append(sessionID string, msg types.SessionMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure base directory exists
	if err := os.MkdirAll(s.baseDir, 0755); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	path := filepath.Join(s.baseDir, sessionID+".jsonl")

	// Open file in append mode
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Write JSON line
	if err := json.NewEncoder(f).Encode(msg); err != nil {
		return fmt.Errorf("encode session message: %w", err)
	}

	return nil
}

// SessionSummary holds minimal metadata extracted from a JSONL file.
type SessionSummary struct {
	SessionID string `json:"sessionId"`
	FirstTS   int64  `json:"firstTimestamp"`
	LastTS    int64  `json:"lastTimestamp"`
}

// List enumerates JSONL files in the store directory, returning a summary for
// each. Only the first and last lines are read per file (for timestamps).
// Results are capped at 1000 most-recently-modified files.
func (s *Store) List() ([]SessionSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}

	// Collect .jsonl files with mod times for sorting
	type fileEntry struct {
		name    string
		modTime int64
	}
	var files []fileEntry
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileEntry{name: e.Name(), modTime: info.ModTime().UnixMilli()})
	}

	// Sort by mod time descending (most recent first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime > files[j].modTime
	})

	// Cap at 1000
	truncated := false
	if len(files) > 1000 {
		files = files[:1000]
		truncated = true
	}
	_ = truncated // caller can check len(result) == 1000

	var summaries []SessionSummary
	for _, fe := range files {
		sessionID := strings.TrimSuffix(fe.name, ".jsonl")
		first, last := s.readFirstLastTimestamp(filepath.Join(s.baseDir, fe.name))
		summaries = append(summaries, SessionSummary{
			SessionID: sessionID,
			FirstTS:   first,
			LastTS:    last,
		})
	}
	return summaries, nil
}

// Exists checks whether a JSONL file exists for the given session.
func (s *Store) Exists(sessionID string) bool {
	path := filepath.Join(s.baseDir, sessionID+".jsonl")
	_, err := os.Stat(path)
	return err == nil
}

// readFirstLastTimestamp reads only the first and last lines of a JSONL file
// to extract timestamps. Returns 0 for missing/empty/malformed files.
func (s *Store) readFirstLastTimestamp(path string) (first, last int64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	var firstLine, lastLine string
	for scanner.Scan() {
		line := scanner.Text()
		if firstLine == "" {
			firstLine = line
		}
		lastLine = line
	}

	if firstLine != "" {
		var msg struct {
			Timestamp int64 `json:"timestamp"`
		}
		if json.Unmarshal([]byte(firstLine), &msg) == nil {
			first = msg.Timestamp
		}
	}
	if lastLine != "" {
		var msg struct {
			Timestamp int64 `json:"timestamp"`
		}
		if json.Unmarshal([]byte(lastLine), &msg) == nil {
			last = msg.Timestamp
		}
	}
	return first, last
}

// Load reads all session messages from the JSONL file for the given session.
// Returns an empty slice if the session file does not exist.
func (s *Store) Load(sessionID string) ([]types.SessionMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := filepath.Join(s.baseDir, sessionID+".jsonl")

	// Check if file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []types.SessionMessage{}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open session file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var messages []types.SessionMessage
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		var msg types.SessionMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			return nil, fmt.Errorf("decode session message: %w", err)
		}
		messages = append(messages, msg)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan session file: %w", err)
	}

	return messages, nil
}
