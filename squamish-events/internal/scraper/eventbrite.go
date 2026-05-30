package scraper

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/models"
)

// EventbriteScraper scrapes events from Eventbrite search pages.
type EventbriteScraper struct{}

func (s *EventbriteScraper) Scrape(ctx context.Context, source models.Source) ([]models.Event, error) {
	url := source.URL
	if url == "" {
		url = "https://www.eventbrite.ca/d/canada--squamish/all-events/"
	}

	body, err := fetchURL(ctx, url)
	if err != nil {
		slog.Warn("Eventbrite scraper: failed to fetch page", "url", url, "error", err)
		return nil, nil
	}

	return parseEventbriteEvents(string(body), source), nil
}

// parseEventbriteEvents extracts events from an Eventbrite search results page.
func parseEventbriteEvents(html string, source models.Source) []models.Event {
	var events []models.Event

	// Eventbrite event cards contain links like /e/event-name-tickets-123456789
	eventLinkRe := regexp.MustCompile(`<a[^>]+href=["'](https?://(?:www\.)?eventbrite\.[a-z]+/e/([^"'?]+))["'][^>]*>`)
	matches := eventLinkRe.FindAllStringSubmatch(html, -1)

	// Also try data-attributes for event IDs
	eventIDRe := regexp.MustCompile(`data-event-id=["'](\d+)["']`)

	// Title patterns — Eventbrite uses various heading elements
	titleRe := regexp.MustCompile(`<(?:h[1-4]|div)[^>]+class=["'][^"']*(?:event-card__title|Typography_heading)[^"']*["'][^>]*>([^<]+)<`)

	seen := make(map[string]bool)

	for _, match := range matches {
		eventURL := match[1]
		slug := match[2]

		// Extract event ID from the slug (usually the last number)
		idRe := regexp.MustCompile(`(\d{8,})`)
		idMatch := idRe.FindString(slug)
		if idMatch == "" {
			idMatch = slug
		}

		if seen[idMatch] {
			continue
		}
		seen[idMatch] = true

		// Search for title near this link
		linkIdx := strings.Index(html, match[0])
		title := ""
		startTime := time.Time{}
		location := ""

		if linkIdx >= 0 {
			windowStart := max(0, linkIdx-500)
			windowEnd := min(len(html), linkIdx+2000)
			window := html[windowStart:windowEnd]

			// Try title from heading near the link
			if tm := titleRe.FindStringSubmatch(window); len(tm) > 1 {
				title = strings.TrimSpace(tm[1])
			}

			// If no title from class, try the link text itself
			if title == "" {
				textRe := regexp.MustCompile(`>([^<]{5,})<`)
				afterLink := html[linkIdx:min(len(html), linkIdx+500)]
				if tm := textRe.FindStringSubmatch(afterLink); len(tm) > 1 {
					candidate := strings.TrimSpace(tm[1])
					// Skip navigation-like text
					if len(candidate) > 10 && !strings.Contains(strings.ToLower(candidate), "view") {
						title = candidate
					}
				}
			}

			// Try to find date — Eventbrite uses various formats
			datePatterns := []struct {
				re     string
				format string
			}{
				{`datetime=["'](\d{4}-\d{2}-\d{2}T\d{2}:\d{2})`, "2006-01-02T15:04"},
				{`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun),\s+([A-Z][a-z]{2}\s+\d{1,2})\s+·`, "Jan 2"},
				{`(\w+\s+\d{1,2},\s+\d{4})\s+`, "January 2, 2006"},
			}
			for _, dp := range datePatterns {
				re := regexp.MustCompile(dp.re)
				if dm := re.FindStringSubmatch(window); len(dm) > 1 {
					if t, err := time.Parse(dp.format, dm[1]); err == nil {
						startTime = t
						// If no year was parsed, assume current year
						if startTime.Year() == 0 {
							startTime = startTime.AddDate(time.Now().Year(), 0, 0)
						}
						break
					}
				}
			}

			// Try location
			locRe := regexp.MustCompile(`(?:location|venue)[^>]*>([^<]+)<`)
			if lm := locRe.FindStringSubmatch(window); len(lm) > 1 {
				location = strings.TrimSpace(lm[1])
			}
		}

		if title == "" {
			// Use slug as fallback title
			title = strings.ReplaceAll(strings.TrimSuffix(slug, "-tickets-"+idMatch), "-", " ")
			title = strings.Title(title)
		}

		events = append(events, models.Event{
			SourceID:   source.ID,
			ExternalID: idMatch,
			Title:      title,
			URL:        eventURL,
			StartTime:  startTime,
			Location:   location,
			Category:   "event",
		})
	}

	// Fallback: try JSON-LD structured data
	if len(events) == 0 {
		events = extractJSONLDEvents(html, source)
	}

	// Try data-event-id attributes as last resort
	if len(events) == 0 {
		idMatches := eventIDRe.FindAllStringSubmatch(html, -1)
		for _, m := range idMatches {
			eid := m[1]
			if seen[eid] {
				continue
			}
			seen[eid] = true

			events = append(events, models.Event{
				SourceID:   source.ID,
				ExternalID: eid,
				Title:      "Eventbrite Event " + eid,
				URL:        "https://www.eventbrite.com/e/" + eid,
				Category:   "event",
			})
		}
	}

	if len(events) == 0 {
		slog.Info("Eventbrite scraper: no events found", "url", source.URL)
	}

	return events
}
