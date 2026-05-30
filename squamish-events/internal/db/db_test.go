package db

import (
	"context"
	"testing"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/models"
)

func TestDBBasicOperations(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	database, err := New(dbPath)
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	// Create a source
	src := models.Source{
		Name:           "Test Source",
		Type:           "ical",
		URL:            "https://example.com/cal.ics",
		ScrapeInterval: time.Hour,
		Enabled:        true,
		Config:         "{}",
	}
	srcID, err := database.CreateSource(ctx, src)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if srcID <= 0 {
		t.Fatal("expected positive source ID")
	}

	// List sources
	sources, err := database.ListSources(ctx)
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(sources))
	}
	if sources[0].Name != "Test Source" {
		t.Fatalf("expected 'Test Source', got %q", sources[0].Name)
	}

	// Upsert event
	event := models.Event{
		SourceID:   srcID,
		ExternalID: "evt-001",
		Title:      "Troy Barnes Community Day",
		Location:   "Greendale Community College",
		StartTime:  time.Now().Add(24 * time.Hour),
		Category:   "Community",
	}
	if err := database.UpsertEvent(ctx, event); err != nil {
		t.Fatalf("upsert event: %v", err)
	}

	// List events
	events, err := database.ListEvents(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Title != "Troy Barnes Community Day" {
		t.Fatalf("expected 'Troy Barnes Community Day', got %q", events[0].Title)
	}
	if events[0].SourceName != "Test Source" {
		t.Fatalf("expected source name 'Test Source', got %q", events[0].SourceName)
	}

	// Upsert same event (should update, not duplicate)
	event.Title = "Troy Barnes Community Day (Updated)"
	if err := database.UpsertEvent(ctx, event); err != nil {
		t.Fatalf("upsert event again: %v", err)
	}
	events, err = database.ListEvents(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("list events after upsert: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event after upsert, got %d", len(events))
	}
	if events[0].Title != "Troy Barnes Community Day (Updated)" {
		t.Fatalf("expected updated title, got %q", events[0].Title)
	}

	// Create submission
	sub := models.Submission{
		Title:        "Abed's Film Festival",
		Description:  "A celebration of cinema at Greendale",
		StartTime:    time.Now().Add(48 * time.Hour),
		ContactEmail: "abed@greendale.edu",
		Status:       "pending",
	}
	subID, err := database.CreateSubmission(ctx, sub)
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}
	if subID <= 0 {
		t.Fatal("expected positive submission ID")
	}

	// List submissions
	subs, err := database.ListSubmissions(ctx, "pending")
	if err != nil {
		t.Fatalf("list submissions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 submission, got %d", len(subs))
	}

	// Update submission status
	if err := database.UpdateSubmissionStatus(ctx, subID, "approved"); err != nil {
		t.Fatalf("update submission status: %v", err)
	}
	subs, err = database.ListSubmissions(ctx, "pending")
	if err != nil {
		t.Fatalf("list pending after approve: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("expected 0 pending submissions, got %d", len(subs))
	}
}

func TestEventFilterSearch(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	database, err := New(dbPath)
	if err != nil {
		t.Fatalf("failed to create db: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	srcID, _ := database.CreateSource(ctx, models.Source{
		Name: "Test", Type: "ical", URL: "https://example.com", Enabled: true, Config: "{}",
	})

	database.UpsertEvent(ctx, models.Event{
		SourceID: srcID, ExternalID: "1", Title: "Mountain Biking Race",
		StartTime: time.Now().Add(24 * time.Hour), Category: "Sports",
	})
	database.UpsertEvent(ctx, models.Event{
		SourceID: srcID, ExternalID: "2", Title: "Art Gallery Opening",
		StartTime: time.Now().Add(48 * time.Hour), Category: "Arts",
	})

	// Search
	events, _ := database.ListEvents(ctx, EventFilter{Search: "biking"})
	if len(events) != 1 {
		t.Fatalf("search 'biking': expected 1, got %d", len(events))
	}

	// Category filter
	events, _ = database.ListEvents(ctx, EventFilter{Category: "Arts"})
	if len(events) != 1 {
		t.Fatalf("category 'Arts': expected 1, got %d", len(events))
	}
}
