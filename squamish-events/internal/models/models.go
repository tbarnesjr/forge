package models

import (
	"context"
	"time"
)

type Event struct {
	ID          int64     `json:"id"`
	SourceID    int64     `json:"source_id"`
	ExternalID  string    `json:"external_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Location    string    `json:"location"`
	URL         string    `json:"url"`
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	Category    string    `json:"category"`
	ImageURL    string    `json:"image_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// For API responses — join with sources table
	SourceName string `json:"source_name,omitempty"`
	SourceType string `json:"source_type,omitempty"`
}

type Source struct {
	ID             int64         `json:"id"`
	Name           string        `json:"name"`
	Type           string        `json:"type"` // sorca, meetup, eventbrite, squamish_ca, ical
	URL            string        `json:"url"`
	ScrapeInterval time.Duration `json:"scrape_interval"`
	LastScrapedAt  *time.Time    `json:"last_scraped_at"`
	Enabled        bool          `json:"enabled"`
	Config         string        `json:"config"` // JSON blob for scraper-specific config
}

type Submission struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Location     string    `json:"location"`
	URL          string    `json:"url"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
	Category     string    `json:"category"`
	ContactEmail string    `json:"contact_email"`
	Status       string    `json:"status"` // pending, approved, rejected
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Scraper defines the interface for event source scrapers.
type Scraper interface {
	Scrape(ctx context.Context, source Source) ([]Event, error)
}
