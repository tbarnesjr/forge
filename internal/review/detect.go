package review

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jelmersnoeck/forge/internal/config"
)

// QualityCommands holds detected commands by category.
type QualityCommands struct {
	Lint   []string // lint/check commands
	Test   []string // test commands
	Format []string // format/fmt commands
}

// IsEmpty returns true if no commands were detected.
func (q QualityCommands) IsEmpty() bool {
	return len(q.Lint) == 0 && len(q.Test) == 0 && len(q.Format) == 0
}

// DetectQualityCommands scans the project for quality commands.
// Returns an empty struct (not an error) if nothing is found.
//
// Detection order (highest priority wins per category):
//  1. .forge/config.json quality section
//  2. Makefile / justfile / Taskfile.yml
//  3. .github/workflows/*.yml
//  4. Language manifests (package.json, go.mod, etc.)
//  5. AGENTS.md / README.md (not implemented yet)
func DetectQualityCommands(cwd string) QualityCommands {
	var result QualityCommands

	// Source 1: .forge/config.json
	if detectFromForgeConfig(cwd, &result) {
		return result
	}

	// Source 2: Makefile / justfile / Taskfile.yml
	detectFromTaskRunners(cwd, &result)

	// Source 3: GitHub workflows
	detectFromGitHubWorkflows(cwd, &result)

	// Source 4: Language manifests
	detectFromManifests(cwd, &result)

	return result
}

// detectFromForgeConfig loads .forge/config.json and extracts quality commands.
// Returns true if the quality section exists (even if all arrays are empty —
// that means "author explicitly wants no quality checks").
func detectFromForgeConfig(cwd string, result *QualityCommands) bool {
	cfg, err := config.Load(cwd)
	if err != nil {
		return false
	}
	if cfg.Quality == nil {
		return false
	}

	// Quality section exists. Even if all arrays are empty, this is authoritative.
	result.Lint = cfg.Quality.Lint
	result.Test = cfg.Quality.Test
	result.Format = cfg.Quality.Format

	// If all arrays are explicitly provided (even empty), don't fall through.
	// If some categories are nil, they can still be filled from lower-priority sources.
	allPresent := cfg.Quality.Lint != nil && cfg.Quality.Test != nil && cfg.Quality.Format != nil
	return allPresent
}

// targetCategories maps task runner target names to quality categories.
var targetCategories = map[string]string{
	"lint":   "lint",
	"check":  "lint",
	"test":   "test",
	"ci":     "test",
	"fmt":    "format",
	"format": "format",
}

// detectFromTaskRunners extracts commands from Makefile, justfile, or Taskfile.yml.
func detectFromTaskRunners(cwd string, result *QualityCommands) {
	for _, name := range []string{"justfile", "Justfile", "Makefile", "makefile"} {
		path := filepath.Join(cwd, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		targets := parseTaskRunnerTargets(path, name)
		mergeTargets(result, targets)
		return // first found wins
	}
}

// parseTaskRunnerTargets parses a Makefile or justfile for matching targets.
func parseTaskRunnerTargets(path, kind string) map[string][]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	lines := strings.Split(string(data), "\n")
	targets := make(map[string][]string)
	var currentTarget string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect target header: "target:" (Makefile) or "target:" (justfile)
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && strings.Contains(trimmed, ":") && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, ".") {
			parts := strings.SplitN(trimmed, ":", 2)
			name := strings.TrimSpace(parts[0])
			// Skip if it has path separators (probably not a target)
			if !strings.Contains(name, "/") && !strings.Contains(name, "=") {
				currentTarget = name
			} else {
				currentTarget = ""
			}
			continue
		}

		// Recipe line (indented)
		if currentTarget != "" && (strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ")) {
			cmd := strings.TrimSpace(line)
			if cmd == "" || strings.HasPrefix(cmd, "#") || strings.HasPrefix(cmd, "@") {
				continue
			}
			// Strip leading @ from make recipes
			cmd = strings.TrimPrefix(cmd, "@")
			if cat, ok := targetCategories[currentTarget]; ok {
				targets[cat] = append(targets[cat], cmd)
			}
		} else if trimmed == "" {
			// Blank line may or may not end a target, depends on format
		} else if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			currentTarget = ""
		}
	}

	return targets
}

// mergeTargets adds discovered targets into result for categories that are still empty.
func mergeTargets(result *QualityCommands, targets map[string][]string) {
	if result.Lint == nil {
		if cmds, ok := targets["lint"]; ok && len(cmds) > 0 {
			result.Lint = cmds
		}
	}
	if result.Test == nil {
		if cmds, ok := targets["test"]; ok && len(cmds) > 0 {
			result.Test = cmds
		}
	}
	if result.Format == nil {
		if cmds, ok := targets["format"]; ok && len(cmds) > 0 {
			result.Format = cmds
		}
	}
}

// templateVarRe matches GitHub Actions template variables like ${{ matrix.os }}.
var templateVarRe = regexp.MustCompile(`\$\{\{[^}]+\}\}`)

// detectFromGitHubWorkflows extracts run commands from CI workflow files.
func detectFromGitHubWorkflows(cwd string, result *QualityCommands) {
	workflowDir := filepath.Join(cwd, ".github", "workflows")
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		return
	}

	// Limit to avoid scanning too many files (spec: max 50 files total).
	const maxWorkflowFiles = 10
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		count++
		if count > maxWorkflowFiles {
			break
		}
		path := filepath.Join(workflowDir, entry.Name())
		parseWorkflowFile(path, result)
	}
}

// parseWorkflowFile is a line-by-line YAML parser that extracts `run:` steps
// from jobs whose key contains lint, test, check, or ci.
func parseWorkflowFile(path string, result *QualityCommands) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var currentJobCategory string
	inSteps := false
	seenCmds := make(map[string]bool)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Detect job key: "  test:" or "  lint:" at 2-space indent under jobs
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 2 && strings.HasSuffix(trimmed, ":") {
			jobKey := strings.TrimSuffix(trimmed, ":")
			jobKey = strings.ToLower(jobKey)
			currentJobCategory = categorizeJobName(jobKey)
			inSteps = false
			continue
		}

		// Detect "name:" at job level to recategorize
		if indent == 4 && strings.HasPrefix(trimmed, "name:") {
			jobName := strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
			jobName = strings.Trim(jobName, "\"'")
			if cat := categorizeJobName(strings.ToLower(jobName)); cat != "" {
				currentJobCategory = cat
			}
		}

		// Detect steps section
		if indent == 4 && trimmed == "steps:" {
			inSteps = true
			continue
		}

		// Detect run: inside steps
		if inSteps && currentJobCategory != "" && strings.HasPrefix(trimmed, "- run:") {
			cmd := strings.TrimSpace(strings.TrimPrefix(trimmed, "- run:"))
			cmd = strings.Trim(cmd, "\"'")

			// Skip if it contains unresolvable template variables
			if templateVarRe.MatchString(cmd) {
				log.Printf("[detect] skipping workflow step with template variables: %s", cmd)
				continue
			}

			// Skip trivial commands
			if cmd == "" || strings.HasPrefix(cmd, "echo") || strings.HasPrefix(cmd, "mkdir") {
				continue
			}

			if seenCmds[cmd] {
				continue
			}
			seenCmds[cmd] = true

			switch currentJobCategory {
			case "lint":
				if result.Lint == nil {
					result.Lint = append(result.Lint, cmd)
				}
			case "test":
				if result.Test == nil {
					result.Test = append(result.Test, cmd)
				}
			case "format":
				if result.Format == nil {
					result.Format = append(result.Format, cmd)
				}
			}
		}
	}
}

// categorizeJobName determines the quality category for a job name.
func categorizeJobName(name string) string {
	// Check for exact matches and containment
	switch {
	case strings.Contains(name, "lint"):
		return "lint"
	case strings.Contains(name, "test"):
		return "test"
	case strings.Contains(name, "check"):
		return "lint"
	case strings.Contains(name, "format") || strings.Contains(name, "fmt"):
		return "format"
	case name == "ci":
		return "test"
	}
	return ""
}

// detectFromManifests checks language-specific files to infer quality commands.
func detectFromManifests(cwd string, result *QualityCommands) {
	// package.json
	detectFromPackageJSON(cwd, result)

	// go.mod
	detectFromGoMod(cwd, result)
}

// detectFromPackageJSON extracts lint/test/format from package.json scripts.
func detectFromPackageJSON(cwd string, result *QualityCommands) {
	path := filepath.Join(cwd, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return
	}

	if result.Lint == nil {
		if cmd, ok := pkg.Scripts["lint"]; ok && cmd != "" {
			result.Lint = []string{cmd}
		}
	}
	if result.Test == nil {
		if cmd, ok := pkg.Scripts["test"]; ok && cmd != "" {
			result.Test = []string{cmd}
		}
	}
	if result.Format == nil {
		if cmd, ok := pkg.Scripts["format"]; ok && cmd != "" {
			result.Format = []string{cmd}
		}
	}
}

// detectFromGoMod infers go test/vet if go.mod exists.
func detectFromGoMod(cwd string, result *QualityCommands) {
	path := filepath.Join(cwd, "go.mod")
	if _, err := os.Stat(path); err != nil {
		return
	}

	if result.Test == nil {
		result.Test = []string{"go test ./..."}
	}
}
