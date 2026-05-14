// Package phase defines composable agent phases (architect, coder, reviewer)
// and the software-engineer orchestrator that chains them.
package phase

import (
	"github.com/jelmersnoeck/forge/internal/review"
)

// Phase configures a conversation loop for a specific purpose.
type Phase struct {
	Name            string   // e.g. "spec", "code", "review", "investigate", "qa"
	AllowedTools    []string // tools this phase can use (empty = all)
	DisallowedTools []string // tools this phase cannot use
	Model           string   // model override (empty = default)
	MaxTurns        int      // 0 = unlimited
}

// Result is the output of a completed phase.
type Result struct {
	Phase        string                       // phase name
	SpecPath     string                       // path to spec file (architect output)
	Diff         string                       // git diff (coder output)
	Findings     []review.Finding             // raw review findings (reviewer output)
	Consolidated []review.ConsolidatedFinding // deduplicated findings (post-consolidation)
	HistoryID    string                       // conversation historyID for session resumption
}

// Architect returns the architect phase configuration.
// The architect explores the codebase, analyses the request, and produces
// a feature specification at .forge/specs/<id>.md.
func Architect() Phase {
	return Phase{
		Name: "spec",
		// Architect can read, explore, write new specs, and edit existing
		// specs (for deduplication). No sub-agents.
		DisallowedTools: []string{
			"Agent", "AgentGet", "AgentList", "AgentStop",
			"TaskCreate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
			"QueueImmediate", "QueueOnComplete",
			"UseMCPTool",
		},
		MaxTurns: 200,
	}
}

// Coder returns the coder phase configuration.
func Coder() Phase {
	return Phase{
		Name:     "code",
		MaxTurns: 0, // unlimited
	}
}

// Investigate returns the investigation phase configuration.
// Full exploration with tool access — no specs, no PRs, no sub-agents.
func Investigate() Phase {
	return Phase{
		Name: "investigate",
		DisallowedTools: []string{
			"PRCreate",
			"Agent", "AgentGet", "AgentList", "AgentStop",
			"TaskCreate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
			"QueueImmediate", "QueueOnComplete",
			"UseMCPTool",
			"Reflect",
		},
		MaxTurns: 200,
	}
}

// QA returns the Q&A phase configuration.
// Read-only exploration — no file mutations, no agents.
func QA() Phase {
	return Phase{
		Name: "qa",
		DisallowedTools: []string{
			"Write", "Edit",
			"Agent", "AgentGet", "AgentList", "AgentStop",
			"TaskCreate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
			"QueueImmediate", "QueueOnComplete",
			"UseMCPTool",
			"Reflect",
		},
		MaxTurns: 200,
	}
}

// Ideator returns the ideation agent phase configuration.
// Read-only exploration — produces JSON candidates, no file writes.
func Ideator() Phase {
	return Phase{
		Name: "ideate",
		DisallowedTools: []string{
			"Write", "Edit", "PRCreate",
			"Agent", "AgentGet", "AgentList", "AgentStop",
			"TaskCreate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
			"QueueImmediate", "QueueOnComplete",
			"UseMCPTool",
			"Reflect",
		},
		MaxTurns: 100,
	}
}

// Clarifier returns the clarification agent phase configuration.
// Read-only — verifies file paths, no writes.
func Clarifier() Phase {
	return Phase{
		Name: "clarify",
		DisallowedTools: []string{
			"Write", "Edit", "PRCreate",
			"Agent", "AgentGet", "AgentList", "AgentStop",
			"TaskCreate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
			"QueueImmediate", "QueueOnComplete",
			"UseMCPTool",
			"Reflect",
		},
		MaxTurns: 50,
	}
}

// Planner returns the planning agent phase configuration.
// Can read, write new specs, and edit existing specs.
func Planner() Phase {
	return Phase{
		Name: "plan",
		DisallowedTools: []string{
			"PRCreate",
			"Agent", "AgentGet", "AgentList", "AgentStop",
			"TaskCreate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
			"QueueImmediate", "QueueOnComplete",
			"UseMCPTool",
		},
		MaxTurns: 150,
	}
}

// Reviewer returns the reviewer phase configuration.
func Reviewer() Phase {
	return Phase{
		Name: "review",
		DisallowedTools: []string{
			"Write", "Edit",
			"Agent", "AgentGet", "AgentList", "AgentStop",
			"TaskCreate", "TaskGet", "TaskList", "TaskStop", "TaskOutput",
			"QueueImmediate", "QueueOnComplete",
			"UseMCPTool",
			"Reflect",
		},
		MaxTurns: 100,
	}
}
