package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
)

// AdminServer provides the health/ready/metrics HTTP API.
type AdminServer struct {
	bridge      *Bridge
	adminToken  string
	ready       atomic.Bool
}

// NewAdminServer creates the admin HTTP handler.
func NewAdminServer(b *Bridge, adminToken string) *AdminServer {
	return &AdminServer{
		bridge:     b,
		adminToken: adminToken,
	}
}

// SetReady marks the bridge as ready (after initial reconciliation).
func (a *AdminServer) SetReady() {
	a.ready.Store(true)
}

// Handler returns the HTTP mux for the admin API.
func (a *AdminServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealthz)
	mux.HandleFunc("GET /readyz", a.handleReadyz)
	mux.HandleFunc("GET /metrics", a.handleMetrics)

	if a.adminToken != "" {
		mux.HandleFunc("GET /sessions", a.requireAuth(a.handleListSessions))
		mux.HandleFunc("POST /sessions/{threadId}/interrupt", a.requireAuth(a.handleInterrupt))
	}

	return mux
}

func (a *AdminServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	// TODO: Check Discord WS connected and Forge reachable
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *AdminServer) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !a.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

func (a *AdminServer) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP bridge_active_sessions Number of active bridge sessions\n")
	fmt.Fprintf(w, "# TYPE bridge_active_sessions gauge\n")
	fmt.Fprintf(w, "bridge_active_sessions %d\n", a.bridge.ActiveSessionCount())
}

func (a *AdminServer) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := a.bridge.store.ListActiveSessions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sessions)
}

func (a *AdminServer) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadId")
	rec, err := a.bridge.store.GetSessionByThread(r.Context(), threadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rec == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := a.bridge.forge.Interrupt(r.Context(), rec.ForgeSessionID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *AdminServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		expected := "Bearer " + a.adminToken
		if token != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
