package scraper

import (
	"bufio"
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/models"
)

// ICalScraper parses iCal/ICS feeds.
type ICalScraper struct{}

func (s *ICalScraper) Scrape(ctx context.Context, source models.Source) ([]models.Event, error) {
	body, err := fetchURL(ctx, source.URL)
	if err != nil {
		slog.Error("ical: fetch failed", "url", source.URL, "error", err)
		return nil, nil
	}

	events := parseICalEvents(string(body), source.ID)
	slog.Info("ical: parsed events", "source", source.Name, "count", len(events))
	return events, nil
}

func parseICalEvents(data string, sourceID int64) []models.Event {
	var events []models.Event
	scanner := bufio.NewScanner(strings.NewReader(data))

	var inEvent bool
	var props map[string]string

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")

		if line == "BEGIN:VEVENT" {
			inEvent = true
			props = make(map[string]string)
			continue
		}

		if line == "END:VEVENT" && inEvent {
			inEvent = false
			event, ok := icalPropsToEvent(props, sourceID)
			if ok {
				events = append(events, event)
			}
			continue
		}

		if inEvent {
			// Handle property lines like DTSTART;VALUE=DATE:20260101
			// or DTSTART:20260101T120000Z
			key, value := parseICalLine(line)
			if key != "" {
				props[key] = value
			}
		}
	}

	return events
}

func parseICalLine(line string) (string, string) {
	// Split on first colon
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", ""
	}

	keyPart := line[:idx]
	value := line[idx+1:]

	// Strip parameters (e.g., DTSTART;VALUE=DATE → DTSTART)
	if semi := strings.Index(keyPart, ";"); semi >= 0 {
		keyPart = keyPart[:semi]
	}

	return strings.ToUpper(keyPart), value
}

func icalPropsToEvent(props map[string]string, sourceID int64) (models.Event, bool) {
	summary := props["SUMMARY"]
	uid := props["UID"]
	if summary == "" {
		slog.Warn("ical: skipping event without SUMMARY")
		return models.Event{}, false
	}

	startTime, err := parseICalTime(props["DTSTART"])
	if err != nil {
		slog.Warn("ical: skipping event with bad DTSTART", "summary", summary, "error", err)
		return models.Event{}, false
	}

	var endTime time.Time
	if v, ok := props["DTEND"]; ok {
		t, err := parseICalTime(v)
		if err == nil {
			endTime = t
		}
	}

	externalID := uid
	if externalID == "" {
		externalID = hashExternalID(summary + props["DTSTART"])
	}

	// Unescape iCal text
	description := unescapeICal(props["DESCRIPTION"])
	location := unescapeICal(props["LOCATION"])
	url := props["URL"]

	return models.Event{
		SourceID:    sourceID,
		ExternalID:  externalID,
		Title:       unescapeICal(summary),
		Description: description,
		Location:    location,
		URL:         url,
		StartTime:   startTime,
		EndTime:     endTime,
		Category:    "Community",
	}, true
}

func parseICalTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)

	// Try datetime with Z: 20260101T120000Z
	if t, err := time.Parse("20060102T150405Z", s); err == nil {
		return t, nil
	}
	// Try datetime without Z (local): 20260101T120000
	if t, err := time.Parse("20060102T150405", s); err == nil {
		return t, nil
	}
	// Try date only: 20260101
	if t, err := time.Parse("20060102", s); err == nil {
		return t, nil
	}

	return time.Time{}, &time.ParseError{Value: s, Message: "unrecognized iCal date format"}
}

func unescapeICal(s string) string {
	s = strings.ReplaceAll(s, "\\n", "\n")
	s = strings.ReplaceAll(s, "\\,", ",")
	s = strings.ReplaceAll(s, "\\;", ";")
	s = strings.ReplaceAll(s, "\\\\", "\\")
	return s
}
