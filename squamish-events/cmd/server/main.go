package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tbarnesjr/squamish-events/internal/api"
	"github.com/tbarnesjr/squamish-events/internal/db"
	"github.com/tbarnesjr/squamish-events/internal/models"
	"github.com/tbarnesjr/squamish-events/internal/scheduler"
	"github.com/tbarnesjr/squamish-events/web"
)

func main() {
	port := envOr("PORT", "8080")
	dbPath := envOr("DB_PATH", "squamish-events.db")
	scrapeOnStartup := strings.EqualFold(os.Getenv("SCRAPE_ON_STARTUP"), "true")

	// Open database
	database, err := db.New(dbPath)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	// Seed default sources if none exist
	seedDefaultSources(database)

	// Create scheduler
	sched := scheduler.New(database)

	// Scrape on startup if configured
	if scrapeOnStartup {
		slog.Info("SCRAPE_ON_STARTUP enabled, running immediate scrape")
		sched.RunNow(context.Background())
	}

	// Start scheduler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)
	defer sched.Stop()

	// Set up HTTP server
	mux := http.NewServeMux()

	// API routes
	handler := api.New(database)
	handler.RegisterRoutes(mux)

	// Static files (embedded frontend)
	staticFS, err := fs.Sub(web.StaticFiles, "static")
	if err != nil {
		slog.Error("failed to create sub filesystem", "error", err)
		os.Exit(1)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("GET /", fileServer)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("server starting", "port", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-done
	slog.Info("shutting down")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	server.Shutdown(shutCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func seedDefaultSources(database *db.DB) {
	sources, err := database.ListSources(context.Background())
	if err != nil {
		slog.Error("failed to list sources for seeding", "error", err)
		return
	}
	if len(sources) > 0 {
		return
	}

	defaults := []struct {
		name     string
		typ      string
		url      string
		interval time.Duration
	}{
		{"SORCA", "sorca", "https://www.sorca.ca/events", 6 * time.Hour},
		{"Squamish Meetup Groups", "meetup", "https://www.meetup.com/find/?location=ca--bc--Squamish&source=EVENTS", 4 * time.Hour},
		{"Eventbrite Squamish", "eventbrite", "https://www.eventbrite.ca/d/canada--squamish/events/", 4 * time.Hour},
		{"District of Squamish", "squamish_ca", "https://squamish.ca/events-ede1c98", 6 * time.Hour},
	}

	for _, d := range defaults {
		src := models.Source{
			Name:           d.name,
			Type:           d.typ,
			URL:            d.url,
			ScrapeInterval: d.interval,
			Enabled:        true,
			Config:         "{}",
		}
		id, err := database.CreateSource(context.Background(), src)
		if err != nil {
			slog.Error("failed to seed source", "name", d.name, "error", err)
		} else {
			slog.Info("seeded source", "name", d.name, "id", id)
		}
	}

	fmt.Println("Default sources seeded. Add iCal sources via the admin API.")
}
