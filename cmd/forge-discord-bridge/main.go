// Command forge-discord-bridge connects Discord threads to Forge sessions.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jelmersnoeck/forge/internal/bridge"
	"github.com/jelmersnoeck/forge/internal/discord"
	"github.com/jelmersnoeck/forge/internal/forge"
	"github.com/jelmersnoeck/forge/internal/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(getenv("BRIDGE_LOG_LEVEL", "info")),
	}))

	// Required env
	discordToken := requireEnv("DISCORD_BOT_TOKEN")
	guildID := requireEnv("DISCORD_GUILD_ID")
	forgeURL := requireEnv("FORGE_GATEWAY_URL")

	// Optional env
	dbPath := getenv("BRIDGE_DB_PATH", "/data/bridge.db")
	listenAddr := getenv("BRIDGE_LISTEN_ADDR", ":8080")
	channelsPath := getenv("BRIDGE_CHANNELS_PATH", "/config/channels.json")
	showThinking := getenv("BRIDGE_SHOW_THINKING", "false") == "true"
	revealSession := getenv("BRIDGE_REVEAL_SESSION_ID", "false") == "true"
	adminToken := os.Getenv("BRIDGE_ADMIN_TOKEN")

	// Init store
	st, err := store.New(dbPath)
	if err != nil {
		logger.Error("failed to open store", "path", dbPath, "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// Init config
	cfg := &bridge.Config{
		GuildID:         guildID,
		ForgeGatewayURL: forgeURL,
		ShowThinking:    showThinking,
		RevealSessionID: revealSession,
		AdminToken:      adminToken,
	}

	if err := cfg.LoadChannels(channelsPath); err != nil {
		logger.Error("failed to load channels config", "path", channelsPath, "error", err)
		os.Exit(1)
	}
	cfg.WatchConfig(channelsPath)

	// Init clients
	dc, err := discord.NewLiveClient(discordToken, guildID, logger)
	if err != nil {
		logger.Error("failed to connect to discord", "error", err)
		os.Exit(1)
	}
	defer dc.Close()

	fc := forge.NewHTTPClient(forgeURL, logger)

	// Init bridge
	b := bridge.New(fc, dc, st, cfg, logger)

	// Admin HTTP server
	admin := bridge.NewAdminServer(b, adminToken)

	go func() {
		logger.Info("admin server listening", "addr", listenAddr)
		if err := http.ListenAndServe(listenAddr, admin.Handler()); err != nil {
			logger.Error("admin server error", "error", err)
		}
	}()

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("shutting down")
		cancel()
	}()

	admin.SetReady()
	logger.Info("bridge starting",
		"guild", guildID,
		"forge_url", forgeURL,
		"listen", listenAddr)

	if err := b.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("bridge error", "error", err)
		os.Exit(1)
	}
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required env var not set", "key", key)
		os.Exit(1)
	}
	return v
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
