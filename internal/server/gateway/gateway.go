// Package gateway provides the HTTP API for the forge gateway.
//
//	POST   /sessions                     create session
//	GET    /sessions/:sessionId          get session info
//	POST   /sessions/:sessionId/messages send message
//	GET    /sessions/:sessionId/events   SSE stream
package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jelmersnoeck/forge/internal/server/backend"
	"github.com/jelmersnoeck/forge/internal/server/bus"
	"github.com/jelmersnoeck/forge/internal/types"
)

// Config holds gateway settings.
type Config struct {
	Port         int
	Host         string
	WorkspaceDir string
	SessionsDir  string
	Backend      backend.Backend
	CostDBPath   string // path to costs.db (optional; cost endpoints return 503 if empty)
	Token        string // optional bearer token for /api/ routes
	UIPath       string // mount path for UI (default "/ui"), empty to disable
}

// Start launches the HTTP server.
func Start(cfg Config) error {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /sessions", handleCreateSession)
	mux.HandleFunc("GET /sessions/{sessionId}", handleGetSession)
	mux.HandleFunc("POST /sessions/{sessionId}/messages", handleSendMessage(cfg))
	mux.HandleFunc("POST /sessions/{sessionId}/review", handleReview(cfg))
	mux.HandleFunc("POST /sessions/{sessionId}/interrupt", handleInterrupt(cfg))
	mux.HandleFunc("GET /sessions/{sessionId}/events", handleEvents)

	// Register read-only API endpoints (with auth middleware)
	registerAPIRoutes(mux, cfg)

	// Register embedded UI if not disabled
	if cfg.UIPath != "" {
		RegisterUI(mux, cfg.UIPath)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	log.Printf("[gateway] listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}

func handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CWD      string         `json:"cwd"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		body.Metadata = make(map[string]any)
	}

	sessionID := uuid.New().String()
	now := time.Now().UnixMilli()
	meta := &types.SessionMeta{
		SessionID:    sessionID,
		CWD:          body.CWD,
		Metadata:     body.Metadata,
		CreatedAt:    now,
		LastActiveAt: now,
	}
	bus.SetSession(meta)

	log.Printf("[gateway] session created id=%s cwd=%s", sessionID, body.CWD)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sessionId": sessionID,
		"metadata":  body.Metadata,
	})
}

func handleGetSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionId")
	meta := bus.GetSession(sessionID)
	if meta == nil {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meta)
}

// relays tracks active SSE relay goroutines per session so we don't
// start duplicates.
var (
	relays   = make(map[string]struct{})
	relaysMu sync.Mutex
)

// startRelay connects to an agent's /events SSE endpoint and republishes
// events via the server bus so CLI clients see them.
func startRelay(sessionID, agentAddr string) {
	relaysMu.Lock()
	if _, ok := relays[sessionID]; ok {
		relaysMu.Unlock()
		return
	}
	relays[sessionID] = struct{}{}
	relaysMu.Unlock()

	go func() {
		defer func() {
			relaysMu.Lock()
			delete(relays, sessionID)
			relaysMu.Unlock()
		}()

		url := fmt.Sprintf("http://%s/events", agentAddr)
		resp, err := http.Get(url)
		if err != nil {
			log.Printf("[relay:%s] connect error: %v", sessionID, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			// SSE data lines start with "data: "
			if !isDataLine(line) {
				continue
			}
			jsonData := line[6:] // strip "data: " prefix

			var event types.OutboundEvent
			if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
				continue
			}
			bus.PublishEvent(sessionID, event)
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[relay:%s] read error: %v", sessionID, err)
		}
	}()
}

func isDataLine(line string) bool {
	return len(line) > 6 && line[:6] == "data: "
}

func handleSendMessage(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("sessionId")

		var body struct {
			Text     string         `json:"text"`
			User     string         `json:"user"`
			Source   string         `json:"source"`
			Metadata map[string]any `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if body.Text == "" {
			http.Error(w, `{"error":"text is required"}`, http.StatusBadRequest)
			return
		}
		if body.User == "" {
			body.User = "anonymous"
		}
		if body.Source == "" {
			body.Source = "api"
		}

		// Ensure the agent is running via the backend.
		cwd := cfg.WorkspaceDir
		if meta := bus.GetSession(sessionID); meta != nil && meta.CWD != "" {
			cwd = meta.CWD
		}

		alreadyRunning := cfg.Backend.AgentAddress(sessionID) != ""
		agentAddr, err := cfg.Backend.EnsureAgent(r.Context(), sessionID, backend.AgentOptions{
			CWD:         cwd,
			SessionsDir: cfg.SessionsDir,
		})
		if err != nil {
			log.Printf("[gateway] backend.EnsureAgent error: %v", err)
			http.Error(w, fmt.Sprintf(`{"error":"failed to start agent: %v"}`, err), http.StatusInternalServerError)
			return
		}

		if alreadyRunning {
			log.Printf("[gateway] session resumed id=%s agent=%s", sessionID, agentAddr)
		} else {
			log.Printf("[gateway] agent started id=%s agent=%s cwd=%s", sessionID, agentAddr, cwd)
		}

		// Start SSE relay from agent → bus (idempotent).
		startRelay(sessionID, agentAddr)

		// Forward the message to the agent.
		msgBody, _ := json.Marshal(map[string]string{
			"text":   body.Text,
			"user":   body.User,
			"source": body.Source,
		})
		agentURL := fmt.Sprintf("http://%s/messages", agentAddr)
		resp, err := http.Post(agentURL, "application/json", bytes.NewReader(msgBody))
		if err != nil {
			log.Printf("[gateway] forward to agent error: %v", err)
			http.Error(w, `{"error":"failed to forward message to agent"}`, http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		// Parse the agent's response to get the actual status
		var agentResp map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&agentResp); err != nil {
			log.Printf("[gateway] failed to decode agent response: %v", err)
			agentResp = map[string]string{"status": "queued"} // fallback
		}

		// Update session metadata.
		meta := bus.GetSession(sessionID)
		if meta != nil {
			meta.LastActiveAt = time.Now().UnixMilli()
			bus.SetSession(meta)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(agentResp)
	}
}

func handleReview(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("sessionId")

		// Ensure agent is running.
		cwd := cfg.WorkspaceDir
		if meta := bus.GetSession(sessionID); meta != nil && meta.CWD != "" {
			cwd = meta.CWD
		}

		agentAddr, err := cfg.Backend.EnsureAgent(r.Context(), sessionID, backend.AgentOptions{
			CWD:         cwd,
			SessionsDir: cfg.SessionsDir,
		})
		if err != nil {
			log.Printf("[gateway] backend.EnsureAgent error: %v", err)
			http.Error(w, fmt.Sprintf(`{"error":"failed to start agent: %v"}`, err), http.StatusInternalServerError)
			return
		}

		// Start SSE relay from agent -> bus (idempotent).
		startRelay(sessionID, agentAddr)

		// Forward review request to agent.
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		if len(body) == 0 {
			body = []byte("{}")
		}

		agentURL := fmt.Sprintf("http://%s/review", agentAddr)
		resp, err := http.Post(agentURL, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("[gateway] forward review to agent error: %v", err)
			http.Error(w, `{"error":"failed to forward review to agent"}`, http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		log.Printf("[gateway] review started id=%s", sessionID)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "review_started"})
	}
}

func handleInterrupt(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("sessionId")

		// Check if agent is running
		agentAddr := cfg.Backend.AgentAddress(sessionID)
		if agentAddr == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "no active agent"})
			return
		}

		// Forward interrupt to agent
		agentURL := fmt.Sprintf("http://%s/interrupt", agentAddr)
		resp, err := http.Post(agentURL, "application/json", bytes.NewReader([]byte("{}")))
		if err != nil {
			log.Printf("[gateway] forward interrupt to agent error: %v", err)
			http.Error(w, `{"error":"failed to forward interrupt to agent"}`, http.StatusBadGateway)
			return
		}
		_ = resp.Body.Close()

		log.Printf("[gateway] interrupt sent to agent id=%s", sessionID)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "interrupted"})
	}
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionId")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	events, unsub := bus.Subscribe(sessionID)
	defer unsub()

	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "id: %s\ndata: %s\n\n", event.ID, data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
