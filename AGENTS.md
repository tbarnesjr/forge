# forge

Async coding agent — headless HTTP API with an Anthropic backend.

## Architecture

Forge uses a unified binary architecture with subcommands:

- **Unified Binary** (`cmd/forge/`) — single entry point with subcommands:
  - **`forge` (default)** — interactive REPL (spawns agent subprocess)
  - **`forge agent`** — run agent server (spawned by interactive mode or server mode)
  - **`forge gateway`** — session management gateway (spawns agents via `forge agent`)
  - **`forge stats`** — cost analytics (daily/monthly/session breakdowns)

## Cost Tracking

All API usage is tracked to `~/.forge/costs.db` (SQLite):
- Every API call records: timestamp, session_id, model, tokens, cost
- Query historical costs by day/week/month
- Per-session breakdowns with duration tracking
- Zero overhead — failures don't block sessions

Usage:
```bash
forge stats                    # current month summary + daily breakdown
forge stats --month 2026-04    # specific month
forge stats --week             # current week
forge stats --sessions         # per-session breakdown
forge stats --daily --sessions # both views
```

## MCP Integration

### MCP Client (connecting to remote MCP servers)

The agent can connect to remote MCP servers over HTTP (Streamable HTTP transport),
discover their tools, and expose them to the LLM alongside built-in tools.
Tools from MCP servers are namespaced as `mcp__<serverName>__<toolName>`.

Configuration in `~/.forge/mcp.json` (user-level) or `.forge/mcp.json` (project-level):
```json
{
  "mcpServers": {
    "my-server": {
      "url": "https://example.com/mcp",
      "auth": "oauth"
    },
    "simple-server": {
      "url": "https://simple.example.com/mcp",
      "headers": { "Authorization": "Bearer sk-..." }
    }
  }
}
```

Authentication modes:
- `"auth": "oauth"` — Full OAuth 2.1 with Dynamic Client Registration (RFC 7591),
  PKCE authorization, and automatic token refresh. Tokens stored at `~/.forge/mcp-tokens.json`.
- `"headers"` — Static headers (API keys, pre-configured bearer tokens).

**Lazy tool loading:** MCP tool schemas are NOT sent to the LLM on every API call.
Instead, a single `UseMCPTool` gateway tool (~300 tokens) lets the model discover
and invoke MCP tools on demand via `list_servers` → `list_tools` → `call`. This
avoids the ~15K+ token overhead per MCP server that would otherwise bloat every
request's tool definitions.

Key files:
- `internal/mcp/client.go` — JSON-RPC 2.0 over Streamable HTTP, SSE support
- `internal/mcp/oauth.go` — OAuth 2.1 + DCR + PKCE flow
- `internal/mcp/bridge.go` — connects to MCP servers, caches tool catalogs in Store
- `internal/mcp/store.go` — holds MCP clients + tool catalogs for lazy access
- `internal/mcp/config.go` — config loading (user + project merge)
- `internal/mcp/token_store.go` — persistent OAuth token storage
- `internal/tools/mcp_gateway.go` — UseMCPTool: single gateway tool for lazy MCP access

## Spec-Driven Development

Forge is spec-driven. The agent writes a spec before implementing any feature.
Specs live in `.forge/specs/` (configurable via `.forge/config.json` `specsDir`).
Use `forge --spec path/to/spec.md` to implement an existing spec directly.

Key files:
- `internal/spec/spec.go` — parser and loader
- `internal/config/config.go` — forge config loader
- `internal/runtime/prompt/prompt.go` — spec instructions in system prompt

## Configuration

All forge configuration lives under `.forge/`:
- `.forge/settings.json` — project settings (model, permissions, env)
- `.forge/settings.local.json` — local overrides (gitignored)
- `.forge/rules/` — additional rule files (`.md`)
- `.forge/skills/` — skill definitions (`SKILL.md` with frontmatter)
- `.forge/agents/` — agent definitions (`.md` with frontmatter)
- `.forge/specs/` — feature specifications
- `.forge/learnings/` — actionable gotchas and discoveries from agent sessions
- `.forge/config.json` — forge-level config (specsDir, quality commands, etc.)

For backward compatibility, `.claude/` is also checked as a fallback if `.forge/` doesn't exist.

User-level config: `~/.forge/` (settings, rules, skills).

## Repository layout

```
cmd/
  forge/           unified binary (cli + gateway + agent + stats)
internal/
  mcp/             MCP client (Go) — connects to remote MCP servers
    client.go      JSON-RPC over Streamable HTTP transport
    oauth.go       OAuth 2.1 + DCR + PKCE
    bridge.go      bridges MCP tools into Forge's tool registry
    config.go      config loading (~/.forge/mcp.json)
    token_store.go persistent OAuth token storage
  config/          forge-level configuration (.forge/config.json), user config (~/.forge/config.toml)
  attribution/     commit trailer hooks and PR body attribution
  envutil/         shared .env file loading
  spec/            feature specification loader + parser
  types/           shared contracts (messages, events, tools, context, tasks)
  tools/           tool registry + implementations
  agent/           agent HTTP server, single-session hub, worker
  runtime/
    provider/      LLMProvider interface + Anthropic implementation
    context/       ContextLoader (AGENTS.md, specs, skills, agents, rules)
    prompt/        system prompt assembly
    session/       JSONL session persistence
    loop/          ConversationLoop (agentic loop)
    cost/          cost calculation + SQLite tracker
    task/          background task & sub-agent management
  server/
    bus/           in-memory event pub/sub + session metadata
    backend/       Backend interface + tmux implementation
    gateway/       HTTP routes, SSE streaming, agent message forwarding
```

Go module: `github.com/jelmersnoeck/forge`

## Build & run

```bash
just build              # build unified forge binary
just build-all          # build all binaries
just dev                # build + run interactive CLI
just dev-gateway         # build + run gateway (foreground)
just dev-gateway-daemon  # build + run gateway daemon
just stop-gateway        # stop daemon gateway
just tail-gateway        # tail daemon gateway logs
just gateway-status      # show daemon status (PID, alive/dead, log path)
just test               # go test ./...
just vet                # go vet ./...
just clean              # remove binaries
```

## Running

### Interactive mode (default)
```bash
export ANTHROPIC_API_KEY=sk-...
forge                    # spawns local agent, ephemeral session
forge --skip-worktree    # skip worktree creation, run in current directory
```

**Git Worktree Isolation**: By default, when running in interactive mode from a git repository (and not already in a worktree), Forge automatically:
- Creates a temporary worktree in `/tmp/forge/worktrees/<session-id>`
- Creates a branch `jelmer/<session-id>` from your current HEAD
- Runs the agent in that isolated worktree
- Cleans up the worktree and branch on exit

This ensures each Forge session has its own isolated workspace, preventing conflicts when running multiple sessions or when your main working tree is in use.

Use `--skip-worktree` to disable this behavior and run directly in your current directory.

### Gateway mode (persistent sessions)
```bash
cp .env.example .env        # set ANTHROPIC_API_KEY
just dev-gateway             # local dev foreground (reads .env, builds agent first)
just dev-gateway-daemon      # local dev daemon mode

# In another terminal
forge --gateway http://localhost:3000
forge --gateway http://localhost:3000 --resume <session-id>

# Manual daemon control:
forge gateway -daemon                           # default: /tmp/forge/sessions/forge.{pid,log}
forge gateway -daemon -pid-file /path/to/file   # custom paths
kill $(cat /tmp/forge/sessions/forge.pid)      # stop
```

### Cost analytics
```bash
forge stats                    # current month
forge stats --month 2026-04    # specific month
forge stats --week             # current week
forge stats --sessions         # per-session breakdown
```

## How it works

### Interactive Mode (default)
1. CLI spawns agent as subprocess: `forge agent --port 0`
2. Agent emits `{"port": 12345}` to stdout
3. CLI parses port, connects directly via HTTP
4. CLI sends messages to agent's `/messages` endpoint
5. CLI subscribes to agent's `/events` SSE stream
6. Agent runs ConversationLoop, executes tools, talks to Anthropic
7. On CLI exit, agent subprocess is terminated (ephemeral session)

### Gateway Mode (persistent)
1. CLI sends HTTP requests to the gateway (`--gateway` flag)
2. Gateway creates sessions and manages metadata via an in-memory bus
3. On first message, gateway spawns agent in tmux: `forge agent --port X`
4. Gateway forwards messages to the agent's HTTP API
5. Gateway relays agent SSE events back to CLI subscribers via the bus
6. Agent runs ConversationLoop, executes tools, talks to Anthropic
7. Sessions persist as JSONL for resume

## Key files

- `internal/agent/server.go` — agent HTTP server (health, messages, events)
- `internal/agent/hub.go` — single-session message queue + event pub/sub
- `internal/agent/worker.go` — conversation loop runner
- `internal/runtime/loop/loop.go` — agentic conversation loop
- `internal/runtime/context/loader.go` — AGENTS.md, skills, agents, rules discovery
- `internal/runtime/prompt/prompt.go` — system prompt assembly
- `internal/runtime/provider/anthropic.go` — Anthropic Messages API streaming
- `internal/runtime/task/manager.go` — background task & sub-agent manager
- `internal/tools/registry.go` — tool registry + NewDefaultRegistry()
- `internal/tools/*.go` — tool implementations (Read, Write, Edit, Bash, Grep, Glob, WebSearch, Reflect, TaskCreate, TaskGet, TaskList, TaskStop, TaskOutput, Agent, AgentGet, AgentList, AgentStop, UseMCPTool)
- `internal/server/backend/backend.go` — Backend interface
- `internal/server/backend/tmux.go` — tmux backend implementation
- `internal/server/bus/bus.go` — in-memory event pub/sub + session metadata
- `internal/server/gateway/gateway.go` — HTTP routes, SSE streaming, agent proxy
- `internal/types/types.go` — shared contracts
- `internal/types/task.go` — task & sub-agent types

## Self-Improvement Loop

Forge includes a built-in learning mechanism for capturing actionable gotchas:

1. **AGENTS.md Support**: The agent loads `AGENTS.md` files from:
   - `~/AGENTS.md` (user-level instructions)
   - `$PROJECT/AGENTS.md` or `$PROJECT/.forge/AGENTS.md` (project-level)
   - Parent directories (inheritable)
   - `$PROJECT/AGENTS.local.md` (session-specific)

2. **Reflect Tool**: Agents use the `Reflect` tool to save non-obvious discoveries:
   ```json
   {
     "summary": "Short filename label",
     "learnings": [
       "DuckDuckGo API returns HTTP 202 for bot-detected requests — use server-side search instead",
       "git rebase in worktrees requires --onto with explicit SHAs; interactive mode hangs"
     ]
   }
   ```
   Only call Reflect when the session uncovered something genuinely useful for
   future sessions. Routine work with no surprises needs no reflection.

3. **Automatic Loading**: Learnings from `.forge/learnings/` are injected into
   the system prompt so future sessions benefit from past discoveries.

## API endpoints

### Gateway — gateway mode only

```
POST   /sessions                      create session (accepts metadata)
GET    /sessions/{sessionId}          get session info
POST   /sessions/{sessionId}/messages send message (forwards to agent)
GET    /sessions/{sessionId}/events   SSE stream of OutboundEvents (relayed from agent)
```

### Agent (per-session) — both modes

```
GET    /health                        health check
POST   /messages                      receive message (from CLI or gateway)
GET    /events                        SSE stream of OutboundEvents
POST   /interrupt                     interrupt current work
```

## Testing

- Tests use `testing` + `testify/require`
- Table-driven: `tests := map[string]struct{...}`, loop var `tc`
- All tests use real filesystem (`t.TempDir()`), real exec — no mocks
- Grep tests require `rg` (ripgrep) on PATH
- Backend integration tests require `tmux` on PATH and a built `forge-agent` binary

## Environment variables

- `ANTHROPIC_API_KEY` — required by the agent (not the server)
- `GATEWAY_PORT` — gateway listen port (default: 3000)
- `GATEWAY_HOST` — gateway listen host (default: 0.0.0.0)
- `WORKSPACE_DIR` — default working directory (default: /tmp/forge/workspace)
- `SESSIONS_DIR` — JSONL session storage (default: /tmp/forge/sessions)
- `FORGE_BIN` — path to forge binary (default: forge)
- `FORGE_SESSION_ID` — set by the agent in bash tool env for commit hook attribution
- `FORGE_COAUTHOR` — set by the agent in bash tool env for Co-authored-by trailer
- `FORGE_GENERATED_BY` — set by the agent in bash tool env for Generated-by trailer

## Gotchas

- `~/.forge/settings.json` may contain model aliases like `opus[1m]` that the
  Anthropic API doesn't understand. Agent filters these — only values
  starting with `claude-` are passed through.
- Gateway loads `.env` from project root at startup (custom loader in
  `cmd/forge/gateway.go`). Explicit env vars take precedence.
- Anthropic API requires `tool_result` blocks immediately after `tool_use` in
  the message history. The ConversationLoop persists these to session JSONL so
  resume reconstructs valid history.
- The gateway spawns agents using the unified `forge agent` subcommand. The
  binary path can be customized via `FORGE_BIN` env var.

## Test data

Use TV show Community references for fake data (Troy Barnes, Greendale
Community College, etc.).

## Git commands

- **NEVER use git commands with the -i flag** (like `git rebase -i`, `git add -i`, `git commit --interactive`) since they require interactive input which is not supported
- The Bash tool sets `GIT_EDITOR=true` to prevent git from opening editors, so commands like `git rebase` or `git commit --amend` will work non-interactively
- Always use `-m` flag for `git commit` to provide messages inline
- For complex rebases, use explicit commands like `git rebase --onto` with SHA arguments rather than interactive mode

## Conventions

- Go, standard library where possible
- `internal/` for all packages (no public API yet)
- Platform-agnostic API: `source` is a free-form string, `metadata` is opaque
- Adapters are external HTTP clients — the gateway has no platform-specific code
- Configuration in `.forge/` directory (fallback: `.claude/` for backward compat)

# Agent Learnings

Actionable gotchas discovered during development sessions.

- When referencing `httptest.Server.URL` inside its own handler closure, the variable isn't assigned yet — assign the handler after creating the server, or use a pointer indirection
- Race condition pattern: when a Stop function sets status to 'killed' but a goroutine's deferred completion handler overwrites it to 'failed', check `IsTerminal` before overwriting
- DuckDuckGo Instant Answer API is a knowledge graph, not a search engine — returns HTTP 202 for bot-detected requests and empty results for real queries
- The Anthropic SDK (v1.27.1+) supports `web_search_20250305` and `web_search_20260209` server tools — use the `20260209` variant
- For server-side tools, prefer the sub-call pattern (client tool wrapping a server tool) over injecting server tools into the main conversation loop — keeps history clean, allows cheaper models
- Check Claude Code's source at `~/Projects/claude-code/` when implementing features that interact with external APIs — they likely solved it already
- MCP tool namespacing as `mcp__server__tool` prevents collisions with built-in tools
- Sub-agents share the parent's ContextBundle and LLM provider unchanged — keep this in mind for isolation and rate-limiting concerns
- The task manager uses package-level `SetTaskManager` — not ideal, but changing to ToolContext injection is a larger refactor
