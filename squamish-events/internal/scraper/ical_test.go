package scraper

import (
	"testing"
	"time"
)

func TestParseICalEvents(t *testing.T) {
	ical := `BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:evt-001@example.com
SUMMARY:Troy Barnes Birthday Bash
DTSTART:20260715T180000Z
DTEND:20260715T220000Z
LOCATION:Greendale Community College
DESCRIPTION:Celebrate Troy's birthday with the study group
URL:https://example.com/events/troy-birthday
END:VEVENT
BEGIN:VEVENT
UID:evt-002@example.com
SUMMARY:Paintball Tournament
DTSTART:20260801
DESCRIPTION:Annual paintball championship
END:VEVENT
BEGIN:VEVENT
SUMMARY:
DTSTART:20260901T100000Z
END:VEVENT
END:VCALENDAR`

	events := parseICalEvents(ical, 42)

	// Should skip the event with empty SUMMARY
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// First event
	e := events[0]
	if e.Title != "Troy Barnes Birthday Bash" {
		t.Errorf("title: got %q", e.Title)
	}
	if e.Location != "Greendale Community College" {
		t.Errorf("location: got %q", e.Location)
	}
	if e.ExternalID != "evt-001@example.com" {
		t.Errorf("external_id: got %q", e.ExternalID)
	}
	if e.SourceID != 42 {
		t.Errorf("source_id: got %d", e.SourceID)
	}
	expected := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	if !e.StartTime.Equal(expected) {
		t.Errorf("start_time: got %v, want %v", e.StartTime, expected)
	}

	// Second event (date-only)
	e2 := events[1]
	if e2.Title != "Paintball Tournament" {
		t.Errorf("title: got %q", e2.Title)
	}
	expectedDate := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !e2.StartTime.Equal(expectedDate) {
		t.Errorf("start_time: got %v, want %v", e2.StartTime, expectedDate)
	}
}

func TestParseICalTime(t *testing.T) {
	tests := map[string]struct {
		input string
		want  time.Time
	}{
		"datetime_utc":   {"20260715T180000Z", time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)},
		"datetime_local": {"20260715T180000", time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)},
		"date_only":      {"20260715", time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseICalTime(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnescapeICal(t *testing.T) {
	input := `Hello\, world\; test`
	got := unescapeICal(input)
	want := "Hello, world; test"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Test newline
	got2 := unescapeICal(`Line 1\nLine 2`)
	want2 := "Line 1\nLine 2"
	if got2 != want2 {
		t.Errorf("newline: got %q, want %q", got2, want2)
	}
}
