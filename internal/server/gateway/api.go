package gateway

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jelmersnoeck/forge/internal/runtime/cost"
	"github.com/jelmersnoeck/forge/internal/runtime/session"
	"github.com/jelmersnoeck/forge/internal/server/bus"
)

// apiDeps holds dependencies for API handlers, initialized once.
type apiDeps struct {
	store   *session.Store
	tracker *cost.Tracker // nil if cost tracking is not available

	// cached cost summary
	cacheMu      sync.Mutex
	cachedSummary *costSummaryResponse
	cacheKey     string
	cacheExpiry  time.Time
}

// registerAPIRoutes mounts all /api/ endpoints on the given mux, wrapped with auth middleware.
func registerAPIRoutes(mux *http.ServeMux, cfg Config) {
	deps := &apiDeps{
		store: session.NewStore(cfg.SessionsDir),
	}

	// Open cost tracker in read-only mode if path is provided
	if cfg.CostDBPath != "" {
		t, err := cost.NewReadOnlyTracker(cfg.CostDBPath)
		if err != nil {
			log.Printf("[gateway] cost tracking unavailable: %v", err)
		} else {
			deps.tracker = t
		}
	}

	// Build an API sub-handler
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /api/sessions", deps.handleListSessions)
	apiMux.HandleFunc("GET /api/sessions/{sessionId}/history", deps.handleSessionHistory)
	apiMux.HandleFunc("GET /api/sessions/{sessionId}/costs", deps.handleSessionCosts)
	apiMux.HandleFunc("GET /api/costs/summary", deps.handleCostSummary)

	// Wrap with auth middleware
	authMiddleware := TokenAuthMiddleware(cfg.Token)
	wrapped := authMiddleware(apiMux)

	// Mount on main mux
	mux.Handle("/api/", wrapped)
}

// sessionResponse is a single session in the list response.
type sessionResponse struct {
	SessionID    string `json:"sessionId"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	CreatedAt    int64  `json:"createdAt"`
	LastActiveAt int64  `json:"lastActiveAt"`
	CWD          string `json:"cwd"`
}

func (d *apiDeps) handleListSessions(w http.ResponseWriter, r *http.Request) {
	// Parse query params
	statusFilter := r.URL.Query().Get("status")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	// 1. Active sessions from bus
	activeMetas := bus.ListSessions()
	activeSet := make(map[string]bool, len(activeMetas))

	var sessions []sessionResponse
	for _, meta := range activeMetas {
		activeSet[meta.SessionID] = true
		name := meta.SessionID
		if meta.Metadata != nil {
			if n, ok := meta.Metadata["name"].(string); ok && n != "" {
				name = n
			}
		}
		sessions = append(sessions, sessionResponse{
			SessionID:    meta.SessionID,
			Name:         name,
			Status:       "active",
			CreatedAt:    meta.CreatedAt,
			LastActiveAt: meta.LastActiveAt,
			CWD:          meta.CWD,
		})
	}

	// 2. Closed sessions from JSONL files not in active set
	fileSummaries, err := d.store.List()
	if err != nil {
		log.Printf("[gateway] list sessions error: %v", err)
	} else {
		for _, fs := range fileSummaries {
			if activeSet[fs.SessionID] {
				continue
			}
			sessions = append(sessions, sessionResponse{
				SessionID:    fs.SessionID,
				Name:         fs.SessionID,
				Status:       "closed",
				CreatedAt:    fs.FirstTS,
				LastActiveAt: fs.LastTS,
			})
		}
	}

	// Filter by status
	if statusFilter == "active" || statusFilter == "closed" {
		var filtered []sessionResponse
		for _, s := range sessions {
			if s.Status == statusFilter {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	// Sort by lastActiveAt descending
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastActiveAt > sessions[j].LastActiveAt
	})

	total := len(sessions)

	// Paginate
	if offset > len(sessions) {
		sessions = nil
	} else {
		sessions = sessions[offset:]
		if len(sessions) > limit {
			sessions = sessions[:limit]
		}
	}

	if sessions == nil {
		sessions = []sessionResponse{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": sessions,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

func (d *apiDeps) handleSessionHistory(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionId")

	messages, err := d.store.Load(sessionID)
	if err != nil {
		log.Printf("[gateway] load history error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load history"})
		return
	}

	// If no JSONL file and not in bus, return 404
	if len(messages) == 0 && bus.GetSession(sessionID) == nil && !d.store.Exists(sessionID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"messages": messages,
	})
}

func (d *apiDeps) handleSessionCosts(w http.ResponseWriter, r *http.Request) {
	if d.tracker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cost tracking not available"})
		return
	}

	sessionID := r.PathValue("sessionId")
	breakdown, err := d.tracker.GetSessionCost(sessionID)
	if err != nil {
		log.Printf("[gateway] session cost error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query costs"})
		return
	}

	firstCall := ""
	lastCall := ""
	if !breakdown.FirstCall.IsZero() {
		firstCall = breakdown.FirstCall.UTC().Format(time.RFC3339)
	}
	if !breakdown.LastCall.IsZero() {
		lastCall = breakdown.LastCall.UTC().Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId":           sessionID,
		"totalCost":           breakdown.TotalCost,
		"callCount":           breakdown.CallCount,
		"inputTokens":         breakdown.InputTokens,
		"outputTokens":        breakdown.OutputTokens,
		"cacheCreationTokens": breakdown.CacheCreationTokens,
		"cacheReadTokens":     breakdown.CacheReadTokens,
		"firstCall":           firstCall,
		"lastCall":            lastCall,
	})
}

type costSummaryResponse struct {
	TotalCost           float64                 `json:"totalCost"`
	SessionCount        int                     `json:"sessionCount"`
	CallCount           int                     `json:"callCount"`
	InputTokens         int                     `json:"inputTokens"`
	OutputTokens        int                     `json:"outputTokens"`
	CacheCreationTokens int                     `json:"cacheCreationTokens"`
	CacheReadTokens     int                     `json:"cacheReadTokens"`
	Daily               []dailySummaryResponse  `json:"daily"`
}

type dailySummaryResponse struct {
	Date                string  `json:"date"`
	TotalCost           float64 `json:"totalCost"`
	SessionCount        int     `json:"sessionCount"`
	CallCount           int     `json:"callCount"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
}

func (d *apiDeps) handleCostSummary(w http.ResponseWriter, r *http.Request) {
	if d.tracker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cost tracking not available"})
		return
	}

	now := time.Now()
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")

	var start, end time.Time
	if startStr != "" {
		var err error
		start, err = time.Parse("2006-01-02", startStr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid start date"})
			return
		}
	} else {
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	}

	if endStr != "" {
		var err error
		end, err = time.Parse("2006-01-02", endStr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid end date"})
			return
		}
		// end is inclusive: add one day
		end = end.AddDate(0, 0, 1)
	} else {
		end = start.AddDate(0, 1, 0)
	}

	// Check cache (5s TTL)
	cacheKey := start.Format("2006-01-02") + ":" + end.Format("2006-01-02")
	d.cacheMu.Lock()
	if d.cachedSummary != nil && d.cacheKey == cacheKey && time.Now().Before(d.cacheExpiry) {
		resp := d.cachedSummary
		d.cacheMu.Unlock()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	d.cacheMu.Unlock()

	dailies, err := d.tracker.GetDailySummaries(start, end)
	if err != nil {
		log.Printf("[gateway] cost summary error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query costs"})
		return
	}

	resp := &costSummaryResponse{
		Daily: make([]dailySummaryResponse, 0, len(dailies)),
	}

	for _, ds := range dailies {
		resp.TotalCost += ds.TotalCost
		resp.CallCount += ds.CallCount
		resp.InputTokens += ds.InputTokens
		resp.OutputTokens += ds.OutputTokens
		resp.CacheCreationTokens += ds.CacheCreationTokens
		resp.CacheReadTokens += ds.CacheReadTokens

		resp.Daily = append(resp.Daily, dailySummaryResponse{
			Date:         ds.Date.Format("2006-01-02"),
			TotalCost:    ds.TotalCost,
			SessionCount: ds.SessionCount,
			CallCount:    ds.CallCount,
			InputTokens:  ds.InputTokens,
			OutputTokens: ds.OutputTokens,
		})

		// Approximate total session count (daily summaries already count distinct)
		// Use max across days as lower bound; exact count needs separate query
		if ds.SessionCount > resp.SessionCount {
			resp.SessionCount = ds.SessionCount
		}
	}

	// TODO: SessionCount across days is approximate (max-per-day, not distinct across range).
	// A separate query would be needed for exact cross-day distinct count.

	// Cache result
	d.cacheMu.Lock()
	d.cachedSummary = resp
	d.cacheKey = cacheKey
	d.cacheExpiry = time.Now().Add(5 * time.Second)
	d.cacheMu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
