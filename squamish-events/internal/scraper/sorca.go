package scraper

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/models"
)

// SORCAScraper scrapes events from the Squamish Off-Road Cycling Association website.
type SORCAScraper struct{}

func (s *SORCAScraper) Scrape(ctx context.Context, source models.Source) ([]models.Event, error) {
	url := source.URL
	if url == "" {
		url = "https://www.sorca.ca/events"
	}

	body, err := fetchURL(ctx, url)
	if err != nil {
		slog.Warn("SORCA scraper: failed to fetch page", "url", url, "error", err)
		return nil, nil
	}

	return parseSORCAEvents(string(body), source), nil
}

// parseSORCAEvents extracts event data from SORCA HTML.
// Uses string-based extraction to find event blocks with titles, dates, and links.
func parseSORCAEvents(html string, source models.Source) []models.Event {
	var events []models.Event

	// Look for event listing blocks — SORCA typically uses article or div elements
	// with event data. We search for common patterns: links with dates nearby.
	//
	// Pattern 1: <a href="/events/...">Title</a> with nearby date text
	linkRe := regexp.MustCompile(`<a[^>]+href=["']([^"']*(?:/events?/|/calendar/)[^"']*)["'][^>]*>([^<]+)</a>`)
	matches := linkRe.FindAllStringSubmatch(html, -1)

	// Pattern for dates like "January 15, 2025" or "Jan 15, 2025" or "2025-01-15"
	dateRe := regexp.MustCompile(`(?:(?:January|February|March|April|May|June|July|August|September|October|November|December|Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2},?\s+\d{4}|\d{4}-\d{2}-\d{2})`)

	for _, match := range matches {
		eventURL := match[1]
		title := strings.TrimSpace(match[2])

		if title == "" {
			continue
		}

		// Make URL absolute if relative
		if strings.HasPrefix(eventURL, "/") {
			eventURL = "https://www.sorca.ca" + eventURL
		}

		// Try to find a date near this link in the HTML
		linkIdx := strings.Index(html, match[0])
		startTime := time.Time{}
		if linkIdx >= 0 {
			// Search in a window around the link
			windowStart := max(0, linkIdx-500)
			windowEnd := min(len(html), linkIdx+500)
			window := html[windowStart:windowEnd]

			if dateMatch := dateRe.FindString(window); dateMatch != "" {
				startTime = parseDateFuzzy(dateMatch)
			}
		}

		events = append(events, models.Event{
			SourceID:   source.ID,
			ExternalID: hashExternalID(eventURL),
			Title:      title,
			URL:        eventURL,
			StartTime:  startTime,
			Category:   "sports",
		})
	}

	if len(events) == 0 {
		slog.Info("SORCA scraper: no events found", "url", source.URL)
	}

	return events
}

// parseDateFuzzy attempts to parse various date formats.
func parseDateFuzzy(s string) time.Time {
	s = strings.TrimSpace(s)

	formats := []string{
		"January 2, 2006",
		"January 2 2006",
		"Jan 2, 2006",
		"Jan 2 2006",
		"2006-01-02",
	}

	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
