package scraper

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/models"
)

// New returns a Scraper implementation for the given type.
// Returns nil if the type is unknown.
func New(scraperType string) models.Scraper {
	switch scraperType {
	case "sorca":
		return &SORCAScraper{}
	case "meetup":
		return &MeetupScraper{}
	case "eventbrite":
		return &EventbriteScraper{}
	case "squamish_ca":
		return &SquamishCAScraper{}
	case "ical":
		return &ICalScraper{}
	default:
		return nil
	}
}

// fetchURL fetches a URL with a 30-second timeout, context cancellation,
// and a reasonable User-Agent header.
func fetchURL(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", "SquamishEvents/1.0 (+https://squamishevents.ca)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: status %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", url, err)
	}
	return body, nil
}

// hashExternalID generates a deterministic external ID from a string
// (typically a URL or unique identifier from the source).
func hashExternalID(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:8])
}
