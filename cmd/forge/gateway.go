package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/google/uuid"
	"github.com/jelmersnoeck/forge/internal/envutil"
	"github.com/jelmersnoeck/forge/internal/server/backend"
	"github.com/jelmersnoeck/forge/internal/server/gateway"
)

func runGateway(args []string) int {
	fs := flag.NewFlagSet("forge gateway", flag.ExitOnError)
	daemon := fs.Bool("daemon", false, "run in background and write PID file")
	pidFile := fs.String("pid-file", "", "path to PID file (default: $SESSIONS_DIR/forge.pid)")
	logFile := fs.String("log-file", "", "path to log file (default: $SESSIONS_DIR/forge.log)")
	_ = fs.Parse(args[1:])

	envutil.LoadEnv(".")

	port := envInt("GATEWAY_PORT", 3000)
	host := envStr("GATEWAY_HOST", "0.0.0.0")
	workspaceDir := envStr("WORKSPACE_DIR", "/tmp/forge/workspace")
	sessionsDir := envStr("SESSIONS_DIR", defaultSessionsDir)
	forgeBin := envStr("FORGE_BIN", "forge")
	gatewayToken := os.Getenv("FORGE_GATEWAY_TOKEN")

	// Cost DB path: default to ~/.forge/costs.db if it exists
	costDBPath := os.Getenv("FORGE_COST_DB")
	if costDBPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			candidate := filepath.Join(home, ".forge", "costs.db")
			if _, err := os.Stat(candidate); err == nil {
				costDBPath = candidate
			}
		}
	}

	// UI mount path: default "/ui", overridable, disable with FORGE_UI_DISABLE=1
	uiPath := envStr("FORGE_UI_PATH", "/ui")
	if os.Getenv("FORGE_UI_DISABLE") == "1" {
		uiPath = ""
	}

	// Handle daemon mode by re-executing in background
	if *daemon && os.Getenv("FORGE_DAEMON_CHILD") != "1" {
		if *pidFile == "" {
			*pidFile = filepath.Join(sessionsDir, "forge.pid")
		}
		if *logFile == "" {
			*logFile = filepath.Join(sessionsDir, "forge.log")
		}
		daemonize(*pidFile, *logFile)
		return 0
	}

	// Restore flags if we're the daemon child process
	if os.Getenv("FORGE_DAEMON_CHILD") == "1" {
		if pf := os.Getenv("FORGE_PID_FILE"); pf != "" {
			pidFile = &pf
		}
	}

	_ = os.MkdirAll(workspaceDir, 0o755)
	_ = os.MkdirAll(sessionsDir, 0o755)

	// Write PID file if we're in daemon mode
	if pidFile != nil && *pidFile != "" {
		if err := writePIDFile(*pidFile); err != nil {
			log.Fatalf("failed to write PID file: %v", err)
		}
		defer func() { _ = os.Remove(*pidFile) }()
	}

	gatewayID := uuid.New().String()[:8]
	be := backend.NewTmux(forgeBin, gatewayID, workspaceDir)

	// Clean up agent sessions on shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %s, stopping agents...", sig)
		_ = be.Close()
		if pidFile != nil && *pidFile != "" {
			_ = os.Remove(*pidFile)
		}
		os.Exit(0)
	}()

	log.Println("forge gateway starting...")
	log.Printf("  gateway id: %s", gatewayID)
	log.Printf("  workspace: %s", workspaceDir)
	log.Printf("  sessions:  %s", sessionsDir)
	log.Printf("  forge bin: %s", forgeBin)

	cfg := gateway.Config{
		Port:         port,
		Host:         host,
		WorkspaceDir: workspaceDir,
		SessionsDir:  sessionsDir,
		Backend:      be,
		CostDBPath:   costDBPath,
		Token:        gatewayToken,
		UIPath:       uiPath,
	}

	if err := gateway.Start(cfg); err != nil {
		_ = be.Close()
		log.Printf("fatal: %v", err)
		return 1
	}

	return 0
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// daemonize forks the process into the background and exits the parent.
func daemonize(pidFile, logFile string) {
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("failed to get executable path: %v", err)
	}

	// Ensure parent directories exist
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil {
		log.Fatalf("failed to create PID directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		log.Fatalf("failed to create log directory: %v", err)
	}

	// Open log file for child process
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Fatalf("failed to open log file: %v", err)
	}
	defer func() { _ = lf.Close() }()

	// Re-execute with FORGE_DAEMON_CHILD=1 to skip daemonization
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"FORGE_DAEMON_CHILD=1",
		"FORGE_PID_FILE="+pidFile,
	)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start daemon: %v", err)
	}

	fmt.Printf("forge gateway started in background (PID: %d)\n", cmd.Process.Pid)
	fmt.Printf("  log file: %s\n", logFile)
	fmt.Printf("  pid file: %s\n", pidFile)
	fmt.Printf("\nTo stop:\n  kill $(cat %s)\n", pidFile)

	// Parent exits, child continues
	os.Exit(0)
}

// writePIDFile writes the current process ID to a file.
func writePIDFile(path string) error {
	pid := os.Getpid()
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
}
