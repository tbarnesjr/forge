# Squamish Events

Local event aggregator for Squamish, BC. Scrapes events from multiple sources
and presents them in a single searchable interface.

## Sources

- **SORCA** — Squamish Off-Road Cycling Association events
- **Meetup** — Local Meetup groups
- **Eventbrite** — Eventbrite listings for Squamish
- **District of Squamish** — squamish.ca official events
- **iCal feeds** — Any .ics calendar feed

## Quick Start

```bash
# Run locally
go run ./cmd/server

# With scrape on startup
SCRAPE_ON_STARTUP=true go run ./cmd/server

# Docker (arm64)
docker compose up -d
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP server port |
| `DB_PATH` | `squamish-events.db` | SQLite database path |
| `SCRAPE_ON_STARTUP` | `false` | Run scrapers immediately on boot |
| `ADMIN_TOKEN` | _(none)_ | Bearer token for admin API endpoints |

## API

### Public

- `GET /api/events` — list events (query: `search`, `category`, `source`, `from`, `to`, `limit`, `offset`)
- `GET /api/events/:id` — get event details
- `GET /api/sources` — list scraper sources
- `GET /api/categories` — list event categories
- `POST /api/submissions` — submit a community event

### Admin (requires `Authorization: Bearer ADMIN_TOKEN`)

- `GET /api/admin/submissions` — list pending submissions
- `POST /api/admin/submissions/:id/approve` — approve a submission
- `POST /api/admin/submissions/:id/reject` — reject a submission
- `POST /api/admin/sources` — add a new scraper source

## Adding iCal Sources

```bash
curl -X POST http://localhost:8080/api/admin/sources \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Community Calendar",
    "type": "ical",
    "url": "https://example.com/calendar.ics",
    "scrape_interval": 21600,
    "enabled": true,
    "config": "{}"
  }'
```

## Development

```bash
go build -o squamish-events ./cmd/server
./squamish-events
```

## Architecture

- Single Go binary with embedded frontend (via `go:embed`)
- SQLite database with WAL mode for concurrent reads
- Built-in scheduler runs scrapers at configurable intervals
- Vanilla JS frontend with Pico CSS
- Deduplication via `(source_id, external_id)` unique constraint
