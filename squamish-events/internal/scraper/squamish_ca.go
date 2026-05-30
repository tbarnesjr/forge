package scraper

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/models"
)

// SquamishCAScraper scrapes events from the District of Squamish website.
type SquamishCAScraper struct{}

func (s *SquamishCAScraper) Scrape(ctx context.Context, source models.Source) ([]models.Event, error) {
	url := source.URL
	if url == "" {
		url = "https://www.squamish.ca/events-ede7fdf5-d5eb-4be3-b0f6-8a9f56be8621"
	}

	body, err := fetchURL(ctx, url)
	if err != nil {
		slog.Warn("Squamish.ca scraper: failed to fetch page", "url", url, "error", err)
		return nil, nil
	}

	return parseSquamishCAEvents(string(body), source, url), nil
}

// parseSquamishCAEvents extracts events from the District of Squamish events page.
func parseSquamishCAEvents(html string, source models.Source, baseURL string) []models.Event {
	var events []models.Event

	// The District site typically lists events in cards or list items
	// with links to individual event pages

	// Pattern: event links with titles
	linkRe := regexp.MustCompile(`<a[^>]+href=["']([^"']*(?:/events?[/-]|/calendar/|/whats-happening/)[^"']*)["'][^>]*>([\s\S]*?)</a>`)
	matches := linkRe.FindAllStringSubmatch(html, -1)

	// Also try heading-based patterns
	headingRe := regexp.MustCompile(`<h[2-4][^>]*>\s*<a[^>]+href=["']([^"']+)["'][^>]*>([^<]+)</a>`)
	headingMatches := headingRe.FindAllStringSubmatch(html, -1)
	matches = append(matches, headingMatches...)

	// Date patterns common on municipal sites
	dateRe := regexp.MustCompile(`(?:(?:January|February|March|April|May|June|July|August|September|October|November|December)\s+\d{1,2},?\s+\d{4}|\d{4}-\d{2}-\d{2}|(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)[a-z]*,?\s+\w+\s+\d{1,2})`)
	timeRe := regexp.MustCompile(`(\d{1,2}:\d{2}\s*(?:am|pm|AM|PM))`)

	seen := make(map[string]bool)

	for _, match := range matches {
		eventURL := match[1]
		titleHTML := match[2]

		// Strip HTML tags from title
		title := stripTags(titleHTML)
		if title == "" || seen[eventURL] {
			continue
		}
		seen[eventURL] = true

		// Make URL absolute
		if strings.HasPrefix(eventURL, "/") {
			eventURL = "https://www.squamish.ca" + eventURL
		}

		// Find date near this link
		startTime := time.Time{}
		linkIdx := strings.Index(html, match[0])
		if linkIdx >= 0 {
			windowStart := max(0, linkIdx-500)
			windowEnd := min(len(html), linkIdx+1000)
			window := html[windowStart:windowEnd]

			if dm := dateRe.FindString(window); dm != "" {
				startTime = parseDateFuzzy(dm)

				// Try to add time if available
				if tm := timeRe.FindStringSubmatch(window); len(tm) > 1 {
					if t, err := time.Parse("3:04 PM", strings.ToUpper(strings.TrimSpace(tm[1]))); err == nil {
						startTime = time.Date(
							startTime.Year(), startTime.Month(), startTime.Day(),
							t.Hour(), t.Minute(), 0, 0, startTime.Location(),
						)
					}
				}
			}
		}

		// Try to find location
		location := ""
		if linkIdx >= 0 {
			location = extractNearbyText(html, linkIdx, `(?:location|venue|place|where)[^>]*>([^<]+)<`)
		}
		if location == "" {
			location = "Squamish, BC"
		}

		events = append(events, models.Event{
			SourceID:   source.ID,
			ExternalID: hashExternalID(eventURL),
			Title:      title,
			URL:        eventURL,
			StartTime:  startTime,
			Location:   location,
			Category:   "community",
		})
	}

	// Fallback: try JSON-LD
	if len(events) == 0 {
		events = extractJSONLDEvents(html, source)
	}

	if len(events) == 0 {
		slog.Info("Squamish.ca scraper: no events found", "url", baseURL)
	}

	return events
}

// stripTags removes HTML tags from a string.
func stripTags(s string) string {
	re := regexp.MustCompile(`<[^>]+>`)
	result := re.ReplaceAllString(s, "")
	// Collapse whitespace
	result = regexp.MustCompile(`\s+`).ReplaceAllString(result, " ")
	return strings.TrimSpace(result)
}
