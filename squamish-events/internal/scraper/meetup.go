package scraper

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/models"
)

// MeetupScraper scrapes events from Meetup group pages.
type MeetupScraper struct{}

func (s *MeetupScraper) Scrape(ctx context.Context, source models.Source) ([]models.Event, error) {
	url := source.URL
	if url == "" {
		slog.Warn("Meetup scraper: no URL configured", "source", source.Name)
		return nil, nil
	}

	// Ensure we're hitting the events page
	if !strings.HasSuffix(url, "/events") && !strings.HasSuffix(url, "/events/") {
		url = strings.TrimRight(url, "/") + "/events/"
	}

	body, err := fetchURL(ctx, url)
	if err != nil {
		slog.Warn("Meetup scraper: failed to fetch page", "url", url, "error", err)
		return nil, nil
	}

	return parseMeetupEvents(string(body), source), nil
}

// parseMeetupEvents extracts events from a Meetup group events page.
// Meetup renders client-side, but the initial HTML contains structured data
// and some event metadata in script tags and meta elements.
func parseMeetupEvents(html string, source models.Source) []models.Event {
	var events []models.Event

	// Meetup pages contain event cards with links like /group-name/events/12345/
	// Try to find event links and titles
	eventLinkRe := regexp.MustCompile(`<a[^>]+href=["'](https?://(?:www\.)?meetup\.com/[^/]+/events/(\d+)/?)["'][^>]*>([^<]+)</a>`)
	matches := eventLinkRe.FindAllStringSubmatch(html, -1)

	// Also try relative event links
	if len(matches) == 0 {
		eventLinkRe = regexp.MustCompile(`<a[^>]+href=["'](/[^/]+/events/(\d+)/?)["'][^>]*>([^<]+)</a>`)
		matches = eventLinkRe.FindAllStringSubmatch(html, -1)
	}

	// Date pattern for Meetup: "Sat, Jun 15, 2025, 10:00 AM PDT" or similar
	dateRe := regexp.MustCompile(`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun),\s+([A-Z][a-z]{2}\s+\d{1,2},\s+\d{4}),?\s+(\d{1,2}:\d{2}\s+[AP]M)`)

	// Also look for datetime attributes
	datetimeRe := regexp.MustCompile(`(?:datetime|data-event-date)=["'](\d{4}-\d{2}-\d{2}T\d{2}:\d{2})`)

	seen := make(map[string]bool)

	for _, match := range matches {
		eventURL := match[1]
		eventID := match[2]
		title := strings.TrimSpace(match[3])

		if title == "" || seen[eventID] {
			continue
		}
		seen[eventID] = true

		// Make URL absolute if relative
		if strings.HasPrefix(eventURL, "/") {
			eventURL = "https://www.meetup.com" + eventURL
		}

		// Try to find a date near this event in the HTML
		startTime := time.Time{}
		linkIdx := strings.Index(html, match[0])
		if linkIdx >= 0 {
			windowStart := max(0, linkIdx-1000)
			windowEnd := min(len(html), linkIdx+1000)
			window := html[windowStart:windowEnd]

			if dtMatch := datetimeRe.FindStringSubmatch(window); len(dtMatch) > 1 {
				if t, err := time.Parse("2006-01-02T15:04", dtMatch[1]); err == nil {
					startTime = t
				}
			} else if dateMatch := dateRe.FindStringSubmatch(window); len(dateMatch) > 2 {
				dateStr := dateMatch[1] + " " + dateMatch[2]
				if t, err := time.Parse("Jan 2, 2006 3:04 PM", dateStr); err == nil {
					startTime = t
				}
			}
		}

		// Try to find location — often in a nearby element
		location := extractNearbyText(html, linkIdx, `<address[^>]*>([^<]+)</address>`)
		if location == "" {
			location = extractNearbyText(html, linkIdx, `location["']?\s*:\s*["']([^"']+)`)
		}

		events = append(events, models.Event{
			SourceID:   source.ID,
			ExternalID: eventID,
			Title:      title,
			URL:        eventURL,
			StartTime:  startTime,
			Location:   location,
			Category:   "community",
		})
	}

	// Fallback: try to extract from JSON-LD structured data
	if len(events) == 0 {
		events = extractJSONLDEvents(html, source)
	}

	if len(events) == 0 {
		slog.Info("Meetup scraper: no events found", "url", source.URL)
	}

	return events
}

// extractNearbyText looks for a pattern near a given position in the HTML.
func extractNearbyText(html string, pos int, pattern string) string {
	if pos < 0 {
		return ""
	}
	windowStart := max(0, pos-1000)
	windowEnd := min(len(html), pos+1000)
	window := html[windowStart:windowEnd]

	re := regexp.MustCompile(pattern)
	if m := re.FindStringSubmatch(window); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// extractJSONLDEvents tries to find event data in JSON-LD script tags.
// Many sites include structured data that's easier to parse than HTML.
func extractJSONLDEvents(html string, source models.Source) []models.Event {
	// Look for JSON-LD blocks with Event type
	scriptRe := regexp.MustCompile(`<script[^>]+type=["']application/ld\+json["'][^>]*>([\s\S]*?)</script>`)
	matches := scriptRe.FindAllStringSubmatch(html, -1)

	var events []models.Event
	nameRe := regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)
	urlRe := regexp.MustCompile(`"url"\s*:\s*"([^"]+)"`)
	startRe := regexp.MustCompile(`"startDate"\s*:\s*"([^"]+)"`)
	locRe := regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)

	for _, m := range matches {
		jsonStr := m[1]
		if !strings.Contains(jsonStr, `"Event"`) {
			continue
		}

		nameMatch := nameRe.FindStringSubmatch(jsonStr)
		urlMatch := urlRe.FindStringSubmatch(jsonStr)
		startMatch := startRe.FindStringSubmatch(jsonStr)

		if nameMatch == nil {
			continue
		}

		title := nameMatch[1]
		eventURL := ""
		if urlMatch != nil {
			eventURL = urlMatch[1]
		}

		startTime := time.Time{}
		if startMatch != nil {
			// Try common ISO formats
			for _, f := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
				if t, err := time.Parse(f, startMatch[1]); err == nil {
					startTime = t
					break
				}
			}
		}

		// Location is often nested, take a simple approach
		location := ""
		locIdx := strings.Index(jsonStr, `"location"`)
		if locIdx >= 0 {
			locWindow := jsonStr[locIdx:min(len(jsonStr), locIdx+300)]
			if lm := locRe.FindStringSubmatch(locWindow); len(lm) > 1 {
				// Skip if it's the event name repeated
				if lm[1] != title {
					location = lm[1]
				}
			}
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

	return events
}
