---
id: architect-role
status: draft
---
# Rename spec-creator phase to architect

## Description

The SWE pipeline phase currently called "spec-creator" is misnamed. The role it
plays is **architect**; the spec is merely the artifact an architect produces,
just as the coder produces code and the reviewer produces findings. This spec
renames the phase everywhere (Go symbols, log lines, comments, CLI text, tests)
and sharpens the architect role description in the phase prompt. Output format
and pipeline behaviour are unchanged.

Related: `spec-dedup-and-evolution` (the dedup instructions inside the prompt
are preserved; this spec just reframes who is doing the dedup — an architect,
not a stenographer).

## Context

Files that require changes:

| File | What changes |
|---|---|
| `internal/agent/phase/phase.go` | `SpecCreator()` → `Architect()`, function comment, package doc |
| `internal/agent/phase/prompts.go` | `specCreatorPrompt` → `architectPrompt`, prompt prose, `PromptForPhase` mapping |
| `internal/agent/phase/orchestrator.go` | `runSpecCreator` → `runArchitect`, log messages, comments, `shouldIdeate` comment |
| `internal/agent/phase/phase_test.go` | Test names/labels: `TestSpecCreatorAndPlanner_EditAllowed` → `TestArchitectAndPlanner_EditAllowed`, `TestSpecCreatorPrompt_ContainsDedupInstructions` → `TestArchitectPrompt_ContainsDedupInstructions`, table-driven label `"spec creator"` → `"architect"` |
| `internal/agent/phase/session_continuity_test.go` | `TestRunSpecCreator_ReturnsHistoryID` → `TestRunArchitect_ReturnsHistoryID`, call site + assertion message |
| `internal/agent/worker.go` | `phase.SpecCreator()` → `phase.Architect()` in `case "spec":` |
| `cmd/forge/cli.go` | Error message string: `"spec creator writes specs"` → `"architect writes specs"` |

Files explicitly **not** changed:

| File / Symbol | Reason |
|---|---|
| `OrchestratorOpts.SpecPath` | Named after the file path, not the phase |
| `Result.SpecPath` | Same — describes the output artifact path |
| `OrchestratorResult.SpecHistoryID` | Describes what the history contains (a spec), not who wrote it |
| `Phase.Name` value `"spec"` | This is the **phase name** used as a key in `PromptForPhase`, session storage, and `emitPhaseStart`/`emitPhaseComplete`. Changing it would break session resumption for in-flight sessions. Stays `"spec"` in v1. |
| Spec file format (`.forge/specs/*.md`) | Out of scope |
| `--mode spec` CLI flag value | The mode value routes through `worker.go` and maps to a `Phase.Name`. Renaming the flag is a user-facing breaking change; out of scope for v1. |

## Behavior

1. `phase.Architect()` returns a `Phase` with `Name: "spec"`, identical
   `DisallowedTools`, and `MaxTurns: 200` — same configuration as today's
   `SpecCreator()`.
2. `phase.SpecCreator` is removed (not aliased). Any code calling
   `SpecCreator()` must be updated in the same commit.
3. `runArchitect` replaces `runSpecCreator` with identical logic; only the
   function name, log format strings, and error wrapping text change.
4. The `architectPrompt` constant replaces `specCreatorPrompt`. The opening
   paragraph is updated from "senior technical product manager and software
   architect" to frame the role as **architect** (see Prompt Sharpening below).
5. All test functions referencing the old names compile and pass after rename.
6. The CLI error message for `--mode spec --spec <path>` reads:
   `"cannot use --spec with --mode spec (architect writes specs, it doesn't consume them)"`.
7. `PromptForPhase("spec")` returns `architectPrompt`.

### Prompt Sharpening (minimal, v1)

The `architectPrompt` is updated to reframe the role. Minimal changes:

- Opening line: "You are a **software architect**." (drop "senior technical
  product manager and").
- Add a short paragraph after the opening that describes the architect's
  responsibilities beyond template-filling: deciding whether the request maps
  to one spec or two, whether it needs a spec at all, naming and categorising,
  linking to related specs, and pushing back on layering violations.
- Existing sections (Exploration Strategy, Quality Gates, Anti-Patterns, Spec
  Deduplication) are preserved verbatim.
- No new sections added to the prompt in this spec.

Deeper prompt rework (structured decision trees, explicit "reject this request"
flows, multi-spec splitting logic) is deferred to a follow-up spec.

## Constraints

1. **No behavioural change.** The phase produces the same output, uses the same
   tools, and runs at the same point in the pipeline. Tests assert identical
   outcomes, not just compilation.
2. **No new phase name.** `Phase.Name` stays `"spec"`. Do not introduce
   `"architect"` as a phase name — it would break `PromptForPhase`, session
   resumption, and event streams.
3. **No `--mode` flag change.** `--mode spec` continues to work. Do not add
   `--mode architect` or alias it.
4. **Single commit.** All renames land atomically — no intermediate state where
   `SpecCreator()` is called but doesn't exist.
5. **No spec format changes.** The markdown template, frontmatter schema, and
   section names in `.forge/specs/` files are unchanged.
6. **Prompt changes are additive-only in v1.** Existing prompt sections must
   not be deleted or restructured. Only the opening framing paragraph changes,
   plus one new paragraph describing architect responsibilities.

## Interfaces

```go
// phase.go — public API change

// Architect returns the architect phase configuration.
// The architect explores the codebase, analyses the request, and produces
// a feature specification at .forge/specs/<id>.md.
func Architect() Phase {
    return Phase{
        Name: "spec",
        DisallowedTools: []string{
            "Agent", "AgentGet", "AgentList", "AgentStop",
            "TaskCreate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
            "QueueImmediate", "QueueOnComplete",
            "UseMCPTool",
        },
        MaxTurns: 200,
    }
}

// SpecCreator is REMOVED — no alias, no deprecation wrapper.
```

```go
// orchestrator.go — private method rename

func (o *Orchestrator) runArchitect(ctx context.Context, opts OrchestratorOpts) (Result, error)
// Body identical to current runSpecCreator, with updated log format strings:
//   "[orchestrator:%s] architect phase Send failed ..."
//   fmt.Errorf("architect phase: %w", err)
```

```go
// prompts.go — constant rename

const architectPrompt = `You are a software architect.
...`
// PromptForPhase("spec") returns architectPrompt
```

## Edge Cases

1. **In-flight sessions resumed after deploy.** `Phase.Name` is still `"spec"`,
   so session JSONL and history IDs remain valid. No migration needed.

2. **Grep-based tooling outside the repo.** External scripts that `grep` for
   `SpecCreator` or `runSpecCreator` will break. This is acceptable — internal
   API, no stability guarantee. AGENTS.md should be updated to reflect the new
   naming.

3. **Ideation pipeline path.** The `shouldIdeate` function's comment references
   "single-agent spec creator". Update the comment to "single-agent architect"
   but do not change logic.

4. **Planner phase overlap.** `Planner()` is a separate phase used in the
   ideation pipeline. It is NOT renamed — it plays a different role. The test
   `TestArchitectAndPlanner_EditAllowed` continues to test both.

5. **AGENTS.md references.** The project AGENTS.md mentions "spec-creator" in
   the phase list (`classify → spec-creator → coder → reviewer → finalize`).
   Update to `classify → architect → coder → reviewer → finalize`.

## Tests

Renamed tests (logic unchanged):

| Old name | New name |
|---|---|
| `TestSpecCreatorAndPlanner_EditAllowed` | `TestArchitectAndPlanner_EditAllowed` |
| `TestSpecCreatorPrompt_ContainsDedupInstructions` | `TestArchitectPrompt_ContainsDedupInstructions` |
| `TestRunSpecCreator_ReturnsHistoryID` | `TestRunArchitect_ReturnsHistoryID` |
| Table label `"spec creator"` in `TestPhaseConfig` | `"architect"` |

All existing assertions remain. No new test cases required — this is a rename
with no logic change.

## Rollout

1. Single PR, single commit. All renames are mechanical.
2. After merge, run `just test` to confirm no stale references.
3. Update AGENTS.md in the same commit (phase list in Architecture section).
4. No feature flag, no gradual rollout — zero behavioural change.

## Out of Scope

- Changing the spec file format or adding new spec template sections.
- Changing when the architect phase runs in the pipeline.
- Adding `--mode architect` as a CLI flag (the mode refers to the output
  artifact type, not the role).
- Deep prompt rework (decision trees, reject-request flows, multi-spec
  splitting). That's a follow-up spec.
- Building a "product tier" above architect (e.g., product-manager phase that
  decides whether to invoke architect at all).
- Renaming `Phase.Name` from `"spec"` to `"architect"` — breaks session
  resumption and event streams.
- Renaming `SpecPath`, `SpecHistoryID`, or `--spec` flag — these name the
  artifact, not the role.
