package phase

import (
	"os"
	"testing"

	"github.com/jelmersnoeck/forge/internal/types"
	"github.com/stretchr/testify/require"
)

func TestPhaseDefinitions(t *testing.T) {
	tests := map[string]struct {
		phase          Phase
		wantName       string
		wantMaxTurns   int
		wantDisallowed []string
		wantAllAllowed bool // true if no tool restrictions
	}{
		"architect": {
			phase:        Architect(),
			wantName:     "spec",
			wantMaxTurns: 200,
			wantDisallowed: []string{
				"Agent", "AgentGet", "AgentList", "AgentStop",
			},
		},
		"coder": {
			phase:          Coder(),
			wantName:       "code",
			wantMaxTurns:   0,
			wantAllAllowed: true,
		},
		"qa": {
			phase:        QA(),
			wantName:     "qa",
			wantMaxTurns: 200,
			wantDisallowed: []string{
				"Write", "Edit",
				"Agent", "AgentGet", "AgentList", "AgentStop",
				"TaskCreate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
				"QueueImmediate", "QueueOnComplete",
				"UseMCPTool",
				"Reflect",
			},
		},
		"reviewer": {
			phase:        Reviewer(),
			wantName:     "review",
			wantMaxTurns: 100,
			wantDisallowed: []string{
				"Write", "Edit",
			},
		},
		"ideator": {
			phase:        Ideator(),
			wantName:     "ideate",
			wantMaxTurns: 100,
			wantDisallowed: []string{
				"Write", "Edit", "PRCreate",
				"Agent", "AgentGet", "AgentList", "AgentStop",
			},
		},
		"clarifier": {
			phase:        Clarifier(),
			wantName:     "clarify",
			wantMaxTurns: 50,
			wantDisallowed: []string{
				"Write", "Edit", "PRCreate",
			},
		},
		"planner": {
			phase:        Planner(),
			wantName:     "plan",
			wantMaxTurns: 150,
			wantDisallowed: []string{
				"PRCreate",
				"Agent", "AgentGet", "AgentList", "AgentStop",
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			r.Equal(tc.wantName, tc.phase.Name)
			r.Equal(tc.wantMaxTurns, tc.phase.MaxTurns)

			if tc.wantAllAllowed {
				r.Empty(tc.phase.AllowedTools)
				r.Empty(tc.phase.DisallowedTools)
				return
			}

			for _, tool := range tc.wantDisallowed {
				r.Contains(tc.phase.DisallowedTools, tool,
					"expected %s to be disallowed", tool)
			}
		})
	}
}

func TestPromptForPhase(t *testing.T) {
	tests := map[string]struct {
		phaseName    string
		wantEmpty    bool
		wantContains string
	}{
		"spec": {
			phaseName:    "spec",
			wantContains: "software architect",
		},
		"code": {
			phaseName:    "code",
			wantContains: "expert software engineer",
		},
		"review": {
			phaseName:    "review",
			wantContains: "code review coordinator",
		},
		"qa": {
			phaseName:    "qa",
			wantContains: "answering questions",
		},
		"plan": {
			phaseName:    "plan",
			wantContains: "senior software architect",
		},
		"ideate returns empty": {
			phaseName: "ideate",
			wantEmpty: true,
		},
		"clarify returns empty": {
			phaseName: "clarify",
			wantEmpty: true,
		},
		"unknown defaults to coder": {
			phaseName:    "greendale",
			wantContains: "expert software engineer",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			prompt := PromptForPhase(tc.phaseName)

			switch tc.wantEmpty {
			case true:
				r.Empty(prompt)
			default:
				r.NotEmpty(prompt)
				r.Contains(prompt, tc.wantContains)
			}
		})
	}
}

func TestArchitectAndPlanner_EditAllowed(t *testing.T) {
	r := require.New(t)

	// Edit must be allowed for both architect and planner
	// so they can update existing specs in place.
	for _, phase := range []Phase{Architect(), Planner()} {
		r.NotContains(phase.DisallowedTools, "Edit",
			"phase %q must allow Edit for spec dedup", phase.Name)
	}
}

func TestArchitectPrompt_ContainsDedupInstructions(t *testing.T) {
	r := require.New(t)

	prompt := PromptForPhase("spec")
	r.Contains(prompt, "Spec Deduplication")
	r.Contains(prompt, "Existing Specs")
	r.Contains(prompt, "Edit tool")
	r.Contains(prompt, "superseded")
}

func TestPlannerPrompt_ContainsDedupInstructions(t *testing.T) {
	r := require.New(t)

	prompt := PromptForPhase("plan")
	r.Contains(prompt, "Spec Deduplication")
	r.Contains(prompt, "Existing Specs")
	r.Contains(prompt, "Edit tool")
}

func TestCoderPrompt_ContainsDocumentationSection(t *testing.T) {
	r := require.New(t)

	prompt := PromptForPhase("code")

	// Documentation section must be present in coder prompt
	r.Contains(prompt, "## Documentation")
	r.Contains(prompt, "git diff <default-branch>...HEAD --stat")
	r.Contains(prompt, "README.md, AGENTS.md, CONTRIBUTING.md")
	r.Contains(prompt, "No doc updates")
	r.Contains(prompt, "Read-only sessions")
	r.Contains(prompt, "specs or learnings")
	r.Contains(prompt, "test-only changes")
	r.Contains(prompt, `git commit -m "docs: update documentation"`)
}

func TestDocumentationSection_OnlyInCoderPhase(t *testing.T) {
	r := require.New(t)

	// Documentation section must NOT appear in non-coder phases
	nonCoderPhases := []string{"spec", "review", "qa", "plan"}
	for _, phase := range nonCoderPhases {
		prompt := PromptForPhase(phase)
		r.NotContains(prompt, "## Documentation",
			"phase %q should not contain Documentation section", phase)
	}

	// Empty phases obviously don't have it
	for _, phase := range []string{"ideate", "clarify"} {
		prompt := PromptForPhase(phase)
		r.Empty(prompt, "phase %q should be empty", phase)
	}

	// Unknown phase defaults to coder — should have it
	prompt := PromptForPhase("greendale")
	r.Contains(prompt, "## Documentation")
}

func TestInjectPhasePrompt(t *testing.T) {
	r := require.New(t)

	// Import types indirectly through the function.
	bundle := emptyBundle()

	// Inject spec phase prompt.
	injected := InjectPhasePrompt(bundle, "spec")

	// Original bundle should be unchanged.
	r.Empty(bundle.AgentsMD)

	// Injected bundle should have the phase entry.
	r.Len(injected.AgentsMD, 1)
	r.Equal("phase:spec", injected.AgentsMD[0].Path)
	r.Equal("phase", injected.AgentsMD[0].Level)
	r.Contains(injected.AgentsMD[0].Content, "software architect")
}

func TestFindLatestSpec(t *testing.T) {
	r := require.New(t)

	// Non-existent directory should return empty.
	path := findLatestSpec("/nonexistent/greendale/community/college")
	r.Empty(path)

	// Create a temp dir with specs.
	dir := t.TempDir()
	specsDir := dir + "/.forge/specs"
	require.NoError(t, mkdirAll(specsDir))

	// Write two spec files.
	require.NoError(t, writeFile(specsDir+"/first.md", "# First"))
	require.NoError(t, writeFile(specsDir+"/second.md", "# Second"))

	// Should find the latest one (second, since it was written last).
	path = findLatestSpec(dir)
	r.NotEmpty(path)
	r.Contains(path, ".md")
}

// helpers

func emptyBundle() types.ContextBundle {
	return types.ContextBundle{
		AgentDefinitions: make(map[string]types.AgentDefinition),
	}
}

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
