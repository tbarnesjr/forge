package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/db"
	"github.com/tbarnesjr/squamish-events/internal/models"
	"github.com/tbarnesjr/squamish-events/internal/scraper"
)

// Scheduler runs scrapers at per-source intervals.
type Scheduler struct {
	db     *db.DB
	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new Scheduler.
func New(database *db.DB) *Scheduler {
	return &Scheduler{db: database}
}

// Start begins running scrapers on their configured intervals.
// It launches a goroutine per enabled source.
func (s *Scheduler) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	sources, err := s.db.GetEnabledSources(ctx)
	if err != nil {
		slog.Error("scheduler: failed to load sources", "error", err)
		return
	}

	for _, src := range sources {
		s.wg.Add(1)
		go s.runSource(ctx, src)
	}

	slog.Info("scheduler: started", "sources", len(sources))
}

// RunNow triggers an immediate scrape of all enabled sources.
func (s *Scheduler) RunNow(ctx context.Context) {
	sources, err := s.db.GetEnabledSources(ctx)
	if err != nil {
		slog.Error("scheduler: failed to load sources for immediate scrape", "error", err)
		return
	}

	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func(src models.Source) {
			defer wg.Done()
			s.scrapeSource(ctx, src)
		}(src)
	}
	wg.Wait()
}

// Stop gracefully shuts down the scheduler.
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *Scheduler) runSource(ctx context.Context, src models.Source) {
	defer s.wg.Done()

	interval := src.ScrapeInterval
	if interval < time.Minute {
		interval = time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scrapeSource(ctx, src)
		}
	}
}

func (s *Scheduler) scrapeSource(ctx context.Context, src models.Source) {
	sc := scraper.New(src.Type)
	if sc == nil {
		slog.Warn("scheduler: unknown scraper type", "type", src.Type, "source", src.Name)
		return
	}

	slog.Info("scheduler: scraping", "source", src.Name, "type", src.Type)

	events, err := sc.Scrape(ctx, src)
	if err != nil {
		// Scraper failures log but never crash the scheduler
		slog.Error("scheduler: scrape failed", "source", src.Name, "error", err)
		return
	}

	if len(events) == 0 {
		slog.Info("scheduler: no events found", "source", src.Name)
		// Don't delete existing events — just skip
		return
	}

	// Use a mutex to serialize writes (SQLite single writer)
	s.mu.Lock()
	defer s.mu.Unlock()

	upserted := 0
	for _, event := range events {
		event.SourceID = src.ID
		if err := s.db.UpsertEvent(ctx, event); err != nil {
			slog.Error("scheduler: upsert failed", "source", src.Name, "event", event.Title, "error", err)
			continue
		}
		upserted++
	}

	now := time.Now()
	if err := s.db.UpdateSourceLastScraped(ctx, src.ID, now); err != nil {
		slog.Error("scheduler: failed to update last_scraped_at", "source", src.Name, "error", err)
	}

	slog.Info("scheduler: scrape complete", "source", src.Name, "found", len(events), "upserted", upserted)
}
