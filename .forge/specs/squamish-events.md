---
id: squamish-events
status: implemented
---
# Squamish BC local event aggregator web app

## Description
Standalone Go web app that aggregates local events from multiple Squamish BC sources
(SORCA, Meetup, Eventbrite, District of Squamish, iCal feeds) into a single searchable
interface. SQLite backend, vanilla JS frontend, containerized for arm64 Mac mini deployment.
Built as a subdirectory in the forge worktree (`squamish-events/`) rather than a separate repo.

## Context
Files created in `squamish-events/` directory:
- `cmd/server/main.go` — entry point with source seeding, scheduler, HTTP server
- `internal/models/models.go` — Event, Source, Submission types + Scraper interface
- `internal/db/db.go` — SQLite layer (WAL, busy timeout, CRUD, upsert)
- `internal/db/db_test.go` — DB integration tests
- `internal/api/api.go` — HTTP handler with admin auth middleware
- `internal/scheduler/scheduler.go` — per-source interval scheduler
- `internal/scraper/scraper.go` — registry + fetchURL helper
- `internal/scraper/sorca.go` — SORCA HTML scraper
- `internal/scraper/meetup.go` — Meetup page scraper
- `internal/scraper/eventbrite.go` — Eventbrite scraper
- `internal/scraper/squamish_ca.go` — District of Squamish scraper
- `internal/scraper/ical.go` — Generic iCal/ICS feed parser
- `internal/scraper/ical_test.go` — iCal parser tests
- `web/embed.go` — embed.FS for static files
- `web/static/index.html` — main page
- `web/static/app.js` — vanilla JS frontend logic
- `web/static/style.css` — custom styles
- `Dockerfile` — multi-stage build, alpine runtime
- `docker-compose.yml` — arm64 with dashboard labels
- `README.md` — documentation

## Behavior
- Go HTTP server serves API + embedded frontend on configurable port (PORT env, default 8080)
- Built-in scheduler runs scrapers at per-source intervals (goroutine per source)
- SCRAPE_ON_STARTUP=true triggers immediate scrape of all enabled sources on boot
- 5 scraper types: sorca, meetup, eventbrite, squamish_ca, ical
- Scrapers use external_id+source_id for dedup (INSERT ON CONFLICT DO UPDATE)
- Default sources seeded on first run (SORCA, Meetup, Eventbrite, District of Squamish)
- User event submissions go to `submissions` table with status=pending
- Admin endpoints gated by Bearer ADMIN_TOKEN env var
- Admin can approve (copies to events via "Community Submissions" source) or reject submissions
- Frontend: search, category/date/source filters, calendar view, submit modal
- Dockerized for linux/arm64 with dashboard labels (homepage, watchtower)
- Categories served as static list from /api/categories endpoint

## Constraints
- No CGO — uses modernc.org/sqlite v1.34.5
- No JS frameworks — vanilla JS + Pico CSS CDN
- No Facebook/Instagram scrapers
- Scraper failures log via slog + return nil/empty, never crash scheduler
- Single binary with embedded frontend via Go embed (web package)

## Interfaces
```go
// internal/models/models.go
type Scraper interface {
    Scrape(ctx context.Context, source Source) ([]Event, error)
}

// internal/db/db.go
type EventFilter struct {
    Search   string
    Category string
    Source   string
    From     *time.Time
    To       *time.Time
    Limit    int
    Offset   int
}
```

API endpoints:
- `GET /api/events` — list events with filters
- `GET /api/events/{id}` — get event details
- `GET /api/sources` — list sources
- `GET /api/categories` — list categories
- `POST /api/submissions` — submit community event
- `GET /api/admin/submissions` — list submissions (admin)
- `POST /api/admin/submissions/{id}/approve` — approve (admin)
- `POST /api/admin/submissions/{id}/reject` — reject (admin)
- `POST /api/admin/sources` — add source (admin)

## Edge Cases
- Scraper target site returns 403/429 → fetchURL returns error, scraper logs + returns nil
- Duplicate events across sources → dedup by (source_id, external_id) UNIQUE constraint
- SQLite concurrent writes → WAL mode + 5s busy timeout; scheduler mutex on writes
- Empty scrape result → don't delete existing events, just skip (no purge logic)
- Malformed iCal feed → parseICalEvents skips entries with missing SUMMARY or bad DTSTART, logs warnings
- Approved submission → creates event under auto-created "Community Submissions" source
