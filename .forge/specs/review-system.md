---
id: review-system
status: active
---
# Umbrella spec: review system — roster, architecture, and extensibility

## Description
Single source of truth for the Forge review system: what reviewers exist, what
each does, how they run, and how to add a new one. Consolidates six prior specs
into one reference document and introduces a new **project-quality** reviewer
that auto-detects and runs the project's own lint/test/format commands.

Related specs folded into this document (originals preserved in place):

| Spec ID | Status | What it added |
|---|---|---|
| `automated-review` | active | Foundation — multi-agent parallel review, orchestrator, `Reviewer` interface, OpenAI provider, `/review` command, slash-command autocomplete |
| `review-dedup` | implemented | LLM-based consolidation step + deterministic dedup fallback (`ConsolidatedFinding`, `ConsolidatedResults`) |
| `review-loop-hardening` | implemented | Raised cycle cap to 10 (later 5), infinite reset on criticals, removed `praise` severity |
| `review-net-new-findings` | implemented | Incremental diffs for cycles 1+, severity-gated loop (only critical+warning are actionable), reduced `maxReviewCycles` to 5 |
| `simplification-reviewer` | implemented | Added `SimplificationReviewer`, sharpened `MaintainabilityReviewer` prompt to avoid overlap |
| `post-review-squash` | implemented | Deterministic squash of branch commits after review loop, before PR creation |

## Context
Files that define and orchestrate the review system today:

- `internal/review/review.go` — core types: `Severity`, `Finding`, `ReviewResult`, `ConsolidatedFinding`, `Source`, `ConsolidatedResults`, `Reviewer` interface
- `internal/review/reviewers.go` — six reviewer structs, `DefaultReviewers()`, `DefaultReviewersWithSpec()`, `findingJSONFormat`
- `internal/review/orchestrator.go` — `Orchestrator`, `ReviewRequest`, `Run()`, `consolidate()`, `buildUserMessage()`, `parseFindings()`, helpers
- `internal/review/consolidate.go` — `Consolidate()`, prompt construction, fallback, formatting helpers
- `internal/review/dedup.go` — deterministic dedup: `DedupRawFindings()`, similarity matching
- `internal/review/diff.go` — `GetDiff()`, `GetIncrementalDiff()`, `GetHeadSHA()`, `detectBaseBranch()`
- `internal/agent/phase/orchestrator.go` — `runSWEPipeline()` review→fix loop (cycles, severity gating, squash)
- `internal/agent/worker.go` — `/review` command handler (manual one-shot review)
- `internal/tools/squash.go` — `SquashBranchCommits()` called post-review
- `internal/config/config.go` — `ForgeConfig` (currently only `specsDir`; gains `Quality` block)
- `.forge/config.json` — project-level config (gains `quality` section)

## Behavior

### 1. Reviewer roster

There are two categories of reviewer:

**LLM-based reviewers** — run in parallel, each via `LLMProvider.Chat()`, receive only the git diff (read-only, no tools):

| # | Name | Struct | Purpose | Flags | Does NOT flag | Originated from |
|---|---|---|---|---|---|---|
| 1 | `security` | `SecurityReviewer` | Vulnerabilities, injection, auth, secrets | Injection (SQL/cmd/path), auth bypass, leaked secrets, SSRF, unsafe deserialization, race conditions with security impact | Performance, style, naming | `automated-review` |
| 2 | `code-quality` | `CodeQualityReviewer` | Correctness, robustness, test coverage | Logic errors, nil deref, race conditions, resource leaks, missing error handling, test gaps, API contract violations | Style, naming, documentation (→ maintainability), complexity (→ simplification) | `automated-review` |
| 3 | `simplification` | `SimplificationReviewer` | Unnecessary complexity, over-engineering | Deep nesting, unnecessary abstractions, verbose code with simpler equivalents, over-engineering, dead branches, boolean logic | Naming (→ maintainability), correctness (→ code-quality), security | `simplification-reviewer` |
| 4 | `maintainability` | `MaintainabilityReviewer` | Structural/architectural health | Naming, DRY violations, dead code, inconsistent patterns, poor separation of concerns, magic numbers, misleading docs | Complexity/readability (→ simplification), correctness (→ code-quality) | `automated-review`, sharpened by `simplification-reviewer` |
| 5 | `operational` | `OperationalReviewer` | Production readiness | Missing/unhelpful error messages, logging gaps, missing observability, hardcoded config, deployment concerns, timeout/retry | Code logic, naming, complexity | `automated-review` |
| 6 | `spec-validation` | `SpecValidationReviewer` | Spec compliance (only when specs exist) | Implementation contradicts spec, missing required behavior, unhandled spec edge cases, code beyond spec | Issues unrelated to the active spec | `automated-review` |

**Deterministic reviewers** — run commands in a subprocess, parse exit codes/output into `Finding` structs:

| # | Name | Struct | Purpose | Originated from |
|---|---|---|---|---|
| 7 | `project-quality` | `ProjectQualityReviewer` | Auto-detect and run the project's lint/test/format commands, surfacing failures as findings | This spec (new) |

### 2. How reviewers are registered

```go
// internal/review/reviewers.go

func DefaultReviewers() []Reviewer {
    return []Reviewer{
        SecurityReviewer{},
        CodeQualityReviewer{},
        SimplificationReviewer{},
        MaintainabilityReviewer{},
        OperationalReviewer{},
    }
}

func DefaultReviewersWithSpec() []Reviewer {
    return append(DefaultReviewers(), SpecValidationReviewer{})
}
```

The project-quality reviewer is NOT added to `DefaultReviewers()` because it
needs a working directory and command execution (the LLM-based orchestrator
provides neither). It runs as a separate step in the SWE pipeline's review
phase, before or in parallel with the LLM-based reviewers (see §4).

### 3. Severity levels and finding schema

Three severity levels: `critical`, `warning`, `suggestion`. No others.

```go
type Finding struct {
    Reviewer    string   `json:"reviewer"`
    Provider    string   `json:"provider"`
    Severity    Severity `json:"severity"`
    File        string   `json:"file,omitempty"`
    StartLine   int      `json:"startLine,omitempty"`
    EndLine     int      `json:"endLine,omitempty"`
    Description string   `json:"description"`
}
```

The project-quality reviewer produces `Finding` values with `Provider: "local"`
and `Reviewer: "project-quality"`. Its findings flow into the same
consolidation pipeline as LLM findings.

### 4. Review flow in the SWE pipeline

```
runSWEPipeline
  ├── coder phase
  └── review→fix loop (maxReviewCycles = 5)
        ├── cycle 0: full branch diff (base...HEAD)
        ├── cycle 1+: incremental diff (prevSHA..HEAD)
        ├── run project-quality reviewer (deterministic, in-process)
        ├── run LLM reviewers in parallel (N reviewers × M providers)
        ├── consolidation (LLM dedup → deterministic dedup fallback)
        ├── severity gate: only critical+warning trigger fix cycle
        ├── feed findings to coder (resume)
        └── at cycle limit: reset if criticals remain, else exit
  ├── squash branch commits
  └── return
```

For manual `/review`: LLM reviewers only, full branch diff, all severities
shown. The project-quality reviewer does NOT run on manual `/review` — it is
SWE-pipeline only.

### 5. Project-quality reviewer (new)

**Proposed names** (pick one during implementation):
1. `project-quality` — clear, matches the "quality" config section
2. `project-checks` — emphasizes that it runs pre-existing check commands

**Purpose**: Detect the project's own quality commands (lint, test, format) and
run them. Surface failures as review findings so the coder fix-loop handles
them identically to LLM findings.

**Detection order** (highest priority wins per command category):

1. **`.forge/config.json`** → explicit `quality.lint` / `quality.test` /
   `quality.format` arrays. Author intent is authoritative.
   ```json
   {
     "quality": {
       "lint": ["golangci-lint run ./..."],
       "test": ["go test ./..."],
       "format": ["gofmt -l ."]
     }
   }
   ```

2. **`Makefile` / `justfile` / `Taskfile.yml`** → targets named `lint`,
   `test`, `check`, `ci`, `fmt`, `format`. Extract the commands from the
   recipe body.

3. **`.github/workflows/*.yml`** → parse `run:` steps under jobs whose name
   or key contains `lint`, `test`, `check`, `ci`. This is CI ground truth.

4. **Language manifests**:
   - `package.json` → `scripts.lint`, `scripts.test`, `scripts.format`
   - `Cargo.toml` → infer `cargo test`, `cargo clippy` if present
   - `pyproject.toml` → `[tool.pytest]` → `pytest`, `[tool.ruff]` → `ruff check .`
   - `go.mod` → `go test ./...`, `go vet ./...`

5. **`AGENTS.md` / `README.md`** → grep for `## Testing`, `## Linting`,
   `## Quality`, `## Build & run` headings and extract fenced code blocks
   whose content looks like shell commands.

If a higher-priority source provides a command for a category (e.g., lint),
lower-priority sources for that category are skipped. Different categories
can come from different sources.

If **nothing** is detected across all sources, the reviewer emits zero
findings and logs "no quality commands detected". No fallback to a hardcoded
toolchain. Never invent `golangci-lint`, `pytest`, etc.

**Execution**:
- Each detected command runs in a subprocess with a timeout (configurable,
  default 5 minutes per command).
- CWD is the project root (same as the review orchestrator's `CWD`).
- Environment inherits from the agent process (respects `PATH`, etc.).
- Commands run sequentially to avoid resource contention.
- Exit code 0 → no findings for that command.
- Exit code ≠ 0 → parse stdout+stderr into one or more `Finding` structs.

**Output parsing**:
- If the output contains file:line references (e.g., `foo.go:42: ...`),
  each line maps to a separate `Finding` with `File` and `StartLine` set.
- If the output does not contain parseable locations, emit a single finding
  with `File: ""` and the full output as `Description`.
- Severity mapping: test failures → `critical`; lint violations → `warning`;
  format violations → `suggestion`.

### 6. How to add a new reviewer

Minimum surface area:

1. **Define a struct** in `internal/review/reviewers.go` implementing `Reviewer`:
   ```go
   type MyReviewer struct{}
   func (MyReviewer) Name() string        { return "my-reviewer" }
   func (MyReviewer) SystemPrompt() string { return `...` }
   ```
   For LLM-based reviewers, the system prompt must end with the JSON format
   block (`findingJSONFormat`) and instruct the model to output `[]` when clean.

2. **Register** in `DefaultReviewers()` (or `DefaultReviewersWithSpec()` for
   spec-conditional reviewers). Order does not matter — all run in parallel.

3. **For deterministic (non-LLM) reviewers**: the struct must also implement a
   `Run(ctx context.Context, cwd string) []Finding` method. These do not use
   `SystemPrompt()` at all. The orchestrator calls `Run()` directly instead of
   dispatching to an LLM provider.

4. **Finding schema**: all findings must populate `Reviewer`, `Provider`
   (`"local"` for deterministic), `Severity`, and `Description`. `File`,
   `StartLine`, `EndLine` are optional.

5. **Tests**: add the reviewer name to `TestReviewersList` assertions in
   `internal/review/orchestrator_test.go`.

6. **Update this spec**: add the reviewer to the roster table in §1.

### 7. Review loop mechanics (consolidated from prior specs)

- **Cycle limit**: `maxReviewCycles = 5` (`internal/agent/phase/orchestrator.go`).
- **Severity gating**: only `critical` and `warning` findings trigger a fix
  cycle. `suggestion` and unknown severities do not.
- **Critical reset**: when the cycle limit is reached and criticals remain,
  the counter resets (infinite persistence on criticals).
- **Incremental diffs**: cycle 0 uses full branch diff; cycles 1+ use
  `prevSHA..HEAD`. If no new commits since the last review, the loop exits.
- **Coder fix prompt**: receives only critical+warning findings. Explicitly
  instructs "fix ONLY the listed issues."
- **Consolidation**: after all agents finish, LLM-based dedup runs against
  all available providers concurrently. Best result selected by severity-weighted
  score. Falls back to deterministic dedup (`DedupRawFindings`) on failure.
- **Post-review squash**: after the review loop exits, `SquashBranchCommits`
  squashes all branch commits into one before PR creation.

## Constraints
- LLM-based reviewers must not have tool access — they receive only text (diff + optional spec) and return JSON findings.
- The project-quality reviewer must not hardcode any toolchain. If detection finds nothing, it emits zero findings.
- The project-quality reviewer must not run commands that require secrets or network access it cannot verify. If a command appears to need credentials (e.g., `docker login`, references `$SECRET_*`), skip it with a note finding.
- The project-quality reviewer must not run on manual `/review` — SWE pipeline only.
- Adding a new LLM reviewer must not require changes outside `internal/review/reviewers.go` (struct + registration).
- Deterministic reviewers must produce `Finding` values compatible with the consolidation pipeline — same schema, same severity levels.
- The `Reviewer` interface must not change. Deterministic reviewers extend it with an additional `Run()` method (interface assertion, not interface change).
- Detection must not scan more than 50 files total across all sources.
- Per-command timeout default is 5 minutes. Configurable via `.forge/config.json` `quality.timeout`.

## Interfaces

```go
// internal/review/review.go — unchanged
type Reviewer interface {
    Name() string
    SystemPrompt() string
}

// internal/review/reviewers.go — new interface for deterministic reviewers
type DeterministicReviewer interface {
    Reviewer
    Run(ctx context.Context, cwd string) []Finding
}
```

```go
// internal/review/reviewers.go — new struct

type ProjectQualityReviewer struct{}

func (ProjectQualityReviewer) Name() string         { return "project-quality" }
func (ProjectQualityReviewer) SystemPrompt() string  { return "" } // unused
func (ProjectQualityReviewer) Run(ctx context.Context, cwd string) []Finding
```

```go
// internal/review/detect.go — new file

// QualityCommands holds detected commands by category.
type QualityCommands struct {
    Lint   []string // lint/check commands
    Test   []string // test commands
    Format []string // format/fmt commands
}

// DetectQualityCommands scans the project for quality commands.
// Returns an empty struct (not an error) if nothing is found.
func DetectQualityCommands(cwd string) QualityCommands
```

```go
// internal/config/config.go — extended

type ForgeConfig struct {
    SpecsDir string         `json:"specsDir,omitempty"`
    Quality  *QualityConfig `json:"quality,omitempty"`
}

type QualityConfig struct {
    Lint    []string `json:"lint,omitempty"`
    Test    []string `json:"test,omitempty"`
    Format  []string `json:"format,omitempty"`
    Timeout string   `json:"timeout,omitempty"` // e.g. "5m", parsed as time.Duration
}
```

## Edge Cases

1. **Detection finds commands from multiple sources** — precedence as defined
   in §5 detection order. Per-category: highest-priority source wins. If
   `.forge/config.json` specifies `lint` but not `test`, `lint` comes from
   config and `test` falls through to Makefile/CI/etc.

2. **CI YAML uses matrix/strategy** — the detector extracts `run:` steps from
   all matrix combinations. Deduplicate identical commands. If a step uses
   `${{ matrix.os }}` or similar templating that cannot be resolved statically,
   skip that step with a log message.

3. **Command requires secrets or network** — if a detected command references
   `$SECRET_*`, `$AWS_*`, `$DOCKER_*` env vars or includes `docker push`,
   `npm publish`, `curl` to external URLs, skip it. Emit a `suggestion`-level
   finding: "Skipped command `X` — appears to require secrets or network access."

4. **Command exceeds timeout** — kill the process group (same as bash tool).
   Emit a `warning`-level finding: "Command `X` timed out after 5m."

5. **Command produces no parseable output** — emit a single finding with the
   raw stderr/stdout as description and severity based on exit code (non-zero →
   `warning`).

6. **`.forge/config.json` quality section exists but all arrays are empty** —
   treated as "author explicitly wants no quality checks." Emit zero findings,
   do not fall through to other detection sources.

7. **Project has no recognizable build system** (no Makefile, no package.json,
   no go.mod, etc.) — `DetectQualityCommands` returns empty `QualityCommands`.
   Reviewer emits zero findings.

8. **Multiple justfile/Makefile targets match** (e.g., both `lint` and `check`)
   — include all matching targets as separate commands in the appropriate
   category.

9. **Reviewer runs during incremental review cycle** — the project-quality
   reviewer always runs the full command suite (it is not incremental). Its
   findings are still subject to consolidation/dedup with prior cycles.

10. **Command output contains ANSI escape codes** — strip ANSI before parsing
    file:line references.
