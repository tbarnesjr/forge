package review

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jelmersnoeck/forge/internal/config"
)

// DeterministicReviewer extends Reviewer with a Run method for non-LLM reviewers
// that execute commands directly and produce findings.
type DeterministicReviewer interface {
	Reviewer
	Run(ctx context.Context, cwd string) []Finding
}

// ProjectQualityReviewer auto-detects and runs the project's lint/test/format
// commands, surfacing failures as review findings.
type ProjectQualityReviewer struct{}

func (ProjectQualityReviewer) Name() string        { return "project-quality" }
func (ProjectQualityReviewer) SystemPrompt() string { return "" }

// Run detects quality commands and executes them sequentially.
func (p ProjectQualityReviewer) Run(ctx context.Context, cwd string) []Finding {
	cmds := DetectQualityCommands(cwd)
	if cmds.IsEmpty() {
		log.Printf("[project-quality] no quality commands detected")
		return nil
	}

	timeout := defaultCommandTimeout
	cfg, err := config.Load(cwd)
	if err == nil && cfg.Quality != nil && cfg.Quality.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Quality.Timeout); err == nil {
			timeout = d
		}
	}

	var findings []Finding

	// Run each category sequentially.
	for _, entry := range []struct {
		category string
		commands []string
	}{
		{"lint", cmds.Lint},
		{"test", cmds.Test},
		{"format", cmds.Format},
	} {
		for _, cmd := range entry.commands {
			f := runQualityCommand(ctx, cwd, cmd, entry.category, timeout)
			findings = append(findings, f...)
		}
	}

	return findings
}

const defaultCommandTimeout = 5 * time.Minute

// runQualityCommand executes a single quality command and returns findings.
func runQualityCommand(ctx context.Context, cwd, cmd, category string, timeout time.Duration) []Finding {
	// Check if command needs secrets/network.
	if commandNeedsSecrets(cmd) {
		return []Finding{{
			Reviewer:    "project-quality",
			Provider:    "local",
			Severity:    SeveritySuggestion,
			Description: fmt.Sprintf("Skipped command `%s` — appears to require secrets or network access", cmd),
		}}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Execute via shell for proper argument handling.
	proc := exec.CommandContext(cmdCtx, "sh", "-c", cmd)
	proc.Dir = cwd

	// Kill entire process group on timeout.
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	proc.Cancel = func() error {
		return syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
	}
	proc.WaitDelay = 3 * time.Second

	var stdout, stderr bytes.Buffer
	proc.Stdout = &stdout
	proc.Stderr = &stderr

	err := proc.Run()

	// Exit code 0 → no findings.
	if err == nil {
		return nil
	}

	// Check for timeout.
	if cmdCtx.Err() == context.DeadlineExceeded {
		return []Finding{{
			Reviewer:    "project-quality",
			Provider:    "local",
			Severity:    SeverityWarning,
			Description: fmt.Sprintf("Command `%s` timed out after %s", cmd, timeout),
		}}
	}

	// Parse output.
	combined := stdout.String() + stderr.String()
	combined = strings.TrimSpace(combined)

	// Non-zero exit with no output — emit a generic finding.
	if combined == "" {
		return []Finding{{
			Reviewer:    "project-quality",
			Provider:    "local",
			Severity:    severityForCategory(category),
			Description: fmt.Sprintf("Command `%s` failed with exit code (no output)", cmd),
		}}
	}

	return parseCommandOutput(combined, category)
}

// ansiRe matches ANSI escape codes.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// stripANSI removes ANSI escape codes from text.
func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// fileLineRe matches "file:line:" or "file:line:col:" patterns.
var fileLineRe = regexp.MustCompile(`^([^\s:]+):(\d+)(?::\d+)?:\s*(.+)$`)

// parseCommandOutput parses command stdout+stderr into findings.
func parseCommandOutput(output, category string) []Finding {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil
	}

	output = stripANSI(output)
	severity := severityForCategory(category)

	// Try to parse file:line references.
	lines := strings.Split(output, "\n")
	var findings []Finding

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		m := fileLineRe.FindStringSubmatch(line)
		if m != nil {
			lineNum, _ := strconv.Atoi(m[2])
			findings = append(findings, Finding{
				Reviewer:    "project-quality",
				Provider:    "local",
				Severity:    severity,
				File:        m[1],
				StartLine:   lineNum,
				Description: m[3],
			})
		}
	}

	if len(findings) > 0 {
		return findings
	}

	// No parseable locations — emit single finding with full output.
	return []Finding{{
		Reviewer:    "project-quality",
		Provider:    "local",
		Severity:    severity,
		Description: output,
	}}
}

// severityForCategory maps command categories to finding severities.
func severityForCategory(category string) Severity {
	switch category {
	case "test":
		return SeverityCritical
	case "lint":
		return SeverityWarning
	case "format":
		return SeveritySuggestion
	default:
		return SeverityWarning
	}
}

// secretPatterns are substrings that suggest a command needs secrets or network.
var secretPatterns = []string{
	"$SECRET_",
	"$AWS_",
	"$DOCKER_",
	"docker push",
	"docker login",
	"npm publish",
}

// commandNeedsSecrets returns true if a command appears to need secrets or
// network access that cannot be verified.
func commandNeedsSecrets(cmd string) bool {
	for _, pat := range secretPatterns {
		if strings.Contains(cmd, pat) {
			return true
		}
	}

	// Check for curl to external URLs (not localhost).
	if strings.Contains(cmd, "curl") {
		if strings.Contains(cmd, "localhost") || strings.Contains(cmd, "127.0.0.1") {
			return false
		}
		if strings.Contains(cmd, "http://") || strings.Contains(cmd, "https://") {
			return true
		}
	}

	return false
}
