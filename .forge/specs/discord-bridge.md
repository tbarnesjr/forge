---
id: discord-bridge
status: draft
---
# Discord ↔ Forge bridge (Troy persona)

## Description
A standalone, containerized service that connects a Discord server to a Forge
gateway. Each Discord forum thread in a configured channel becomes one Forge
session; each Forge SSE event becomes a Discord message (or reaction); each
human reply in the thread becomes a Forge `POST /messages` call. The bot is
Troy — Forge wearing a persona — so the channel reads like a normal coding
conversation with a colleague.

The bridge does not run *inside* Forge. It is a separate process that talks
to Forge over its existing HTTP gateway API. This keeps Forge unaware of
Discord and keeps the bridge replaceable (Slack, Mattermost, etc. later).
It satisfies the build rule: agents run in containers (Docker).

The bridge lives in this repo under `cmd/forge-discord-bridge/` so it can
share types (`internal/types`) with the gateway without copy-paste. It
builds and releases as a separate binary / container image.

## Context
- **Forge gateway HTTP API** (already exists):
  - `POST /sessions` → `{sessionId, metadata}` to create a session
  - `POST /sessions/{id}/messages` → send a user message into a session
  - `POST /sessions/{id}/interrupt` → stop the agent mid-loop
  - `GET /sessions/{id}/events` → SSE stream of `OutboundEvent`s (type, content, toolName, usage, model)
- **OutboundEvent.Type** vocabulary (subset that matters for the bridge):
  `text`, `tool_use`, `done`, `error`, `interrupted`, `thinking`, `compact`,
  `retry`, `usage`, `intent_classified`, `ideation_start`, `clarification_question`,
  `planning_start`, `staleness_warning`, `phase_error`, `pr_url`, `pr_monitor`,
  `task_status`.
- **Discord forum channel model**: a parent channel with threads. The user
  starts a new thread = new task. Bot posts in the thread.
- **OpenClaw bot account `pelton`** is already in the guild "Study Room F"
  (id `1491267748632985700`); it's the same agent identity as me. The bridge
  will use a SEPARATE bot identity (`troy`) so the personas don't collide.
- **Build rule** (MEMORY.md): every agent I build runs in Docker.

## Behavior

### The bot identity
- Bridge runs under a dedicated Discord bot application named **Troy** (or
  `forge-bot` for non-personalized deploys).
- Default avatar/banner ships in the repo so a fresh deploy looks right.
- Status: `Watching: <N> sessions` (live count of active Forge sessions),
  updated on session start/done.
- Username/avatar overridable via env so self-hosted instances can re-brand
  (`BRIDGE_BOT_NAME`, `BRIDGE_BOT_AVATAR_URL`).

### Channel topology
- Bridge is configured for **one or more "Forge channels"** per guild:
  - `forge-channel:1504550234661978343` (e.g. `#forge` in our server)
- The Forge channel SHOULD be a **forum channel**. If it's a regular text
  channel, the bridge still works but creates threads off the trigger message
  instead.
- Inside the configured channel, **every new thread = a new Forge session**.
- Outside the configured channel, the bot ignores everything except direct
  pings (which get a helpful "use #forge to start a task" response).

### Starting a task
1. Human creates a Discord thread in `#forge` with a title + body
   (forum starter message). Title becomes the working name; body is the
   initial prompt.
2. Bridge detects new thread via Discord gateway event (`THREAD_CREATE`).
3. Bridge calls Forge `POST /sessions` with:
   ```json
   {
     "cwd": "<configured repo path for this Discord channel>",
     "metadata": {
       "source": "discord",
       "discord.guildId": "...",
       "discord.channelId": "...",
       "discord.threadId": "...",
       "discord.starterMessageId": "...",
       "discord.userId": "...",
       "discord.username": "...",
       "session.initiator.name": "<resolved git name>",
       "session.initiator.email": "<resolved git email>"
     }
   }
   ```
4. Bridge stores the mapping `discord.threadId ↔ forge.sessionId` in its
   own state (SQLite, see Persistence).
5. Bridge POSTs the starter-message body to `/sessions/{id}/messages`.
6. Bridge opens an SSE connection to `/sessions/{id}/events` and starts
   translating events back to thread messages.
7. Bridge reacts 🤖 to the starter message to confirm the handoff.

### Continuing a task
- Every subsequent message in the thread (from any non-bot user) is
  forwarded as a Forge user message via `POST /messages`.
- If the agent is currently mid-loop, the bridge does NOT auto-interrupt —
  it lets Forge's existing queueing/interrupt logic handle it. The human
  can react ⏸ to a recent bot message to send an explicit `/interrupt`.
- If the human edits a prior message, the bridge does NOT replay it. Edits
  on already-sent messages are out of scope for v1.

### Translating Forge events to Discord
- **`text`** — buffered and posted as a single Discord message when the
  agent yields. Two flush conditions:
  1. `done` event arrives → flush whatever's buffered.
  2. Buffer ≥ 1800 chars (Discord limit 2000; leave headroom) → flush in
     chunks, split on sentence boundaries when possible.
- **`tool_use`** — collapsed into a Discord embed with the tool name,
  truncated args (200 chars), and a "running…" indicator. When the tool
  result arrives (the next non-tool event), the embed is *edited* to
  "✓ done" or "✗ failed". The embed never grows — long output stays in
  Forge's logs and is linkable from the embed.
- **`thinking`** — surfaced as a 💭 reaction on the most recent bot
  message, removed when the next `text` event arrives. Optional, gated by
  `BRIDGE_SHOW_THINKING` env (default off — too noisy).
- **`intent_classified`** / **`planning_start`** / **`ideation_start`** —
  collapsed into a single "phase header" message at most once per phase
  transition (e.g. `🧭 Intent: feature → planning`). Suppress per-phase
  noise; the user wants progress, not a play-by-play.
- **`clarification_question`** — posted as a normal message with the
  question; the bot WAITS for a reply (no auto-progress) since the next
  human message becomes the answer.
- **`staleness_warning`** — surfaced as a ⚠️ reaction, no message.
- **`phase_error`** / **`error`** — posted as a message prefixed `❌` with
  the error content. Pinned to the thread.
- **`interrupted`** — posted as `⏸ Interrupted by user.`
- **`retry`** — silent. Logged in the bridge but not surfaced.
- **`compact`** — silent.
- **`usage`** — accumulated; surfaced once at the end of the run in the
  `done` summary (see below). Never per-call.
- **`pr_url`** — posted as a celebratory message: `🚀 PR ready: <url>`,
  pinned to the thread. The starter message gets a 🚀 reaction.
- **`pr_monitor`** — silent. CI status changes can be reflected later.
- **`done`** — posts a final summary embed: total cost, tokens (in/out),
  models used, wall time, PR url if any. Flushes any remaining text buffer.
  Removes any 💭 reactions.

### Bot reactions humans can use
The bridge polls/listens for reactions on its own messages:
- ⏸ on any bot message → `POST /interrupt` for that session.
- 🔁 on a bot message → re-send the prior human message (rare; recovery
  path for transient API errors).
- 🛑 on the *starter* message → end session, archive thread.
- Anything else → ignored, no error.

### Lifecycle
- **Session ends** when:
  - Forge emits `done` with `pr_url` set → bridge marks session "complete",
    keeps thread open for follow-up but stops the SSE relay.
  - Thread is archived in Discord → bridge calls `/interrupt` then drops
    the mapping.
  - User reacts 🛑 to starter → bridge calls `/interrupt`, archives the
    thread, drops the mapping.
  - Bridge crash/restart → see Resilience below.
- **Idle eviction**: a thread with no activity for 7 days and no active
  Forge session has its mapping pruned. The thread stays in Discord;
  re-posting in it does NOT auto-revive (user must create a new thread).

### Resilience / persistence
- State store: SQLite at `/data/bridge.db` (volume-mounted in the
  container).
- Schema:
  - `sessions(discord_thread_id PK, forge_session_id, status, created_at, last_event_at, last_event_id)`
  - `messages(forge_session_id, forge_event_id PK, discord_message_id, posted_at)` — idempotency for at-least-once SSE
  - `pending(discord_message_id PK, thread_id, body, retry_count, next_retry_at)` — outbound retry queue
- On bridge restart:
  1. For every `sessions` row with `status='active'`: re-open the SSE
     subscription using `last_event_id` as the resume token (Forge SSE
     does not currently support resume — see Out of Scope; for now, the
     bridge requests the full stream and dedupes via `forge_event_id`).
  2. Drain the `pending` queue with exponential backoff (1s, 4s, 16s,
     60s, 5m, 30m, then dead-letter at 6h).
- Discord rate limit handling: respect `X-RateLimit-Remaining` /
  `Retry-After`; serialize bursts per channel; coalesce multiple text
  events into one message rather than spraying.

### Configuration
Env (all required unless noted):
- `DISCORD_BOT_TOKEN`
- `DISCORD_GUILD_ID` (single guild for v1; multi-guild later)
- `FORGE_GATEWAY_URL` — e.g. `http://forge-gateway:3000`
- `BRIDGE_DB_PATH` (default `/data/bridge.db`)
- `BRIDGE_LISTEN_ADDR` (default `:8080`, for healthchecks + admin)
- `BRIDGE_BOT_NAME` (optional override)
- `BRIDGE_BOT_AVATAR_URL` (optional)
- `BRIDGE_SHOW_THINKING` (default `false`)
- `BRIDGE_LOG_LEVEL` (default `info`)

Channel config (JSON file mounted at `/config/channels.json`, hot-reloaded
on SIGHUP):
```json
{
  "channels": [
    {
      "channelId": "1504550234661978343",
      "repoPath": "/code/forge",
      "defaultBaseBranch": "main",
      "allowedUserIds": null
    }
  ]
}
```
- `allowedUserIds: null` (or missing) → anyone in the channel can start a task.
- `allowedUserIds: [...]` → only listed Discord user ids can start a task.
  Others get a polite refusal and a 🚫 reaction.

### Admin HTTP API
On `BRIDGE_LISTEN_ADDR`:
- `GET /healthz` — returns 200 if Discord WS is connected and Forge
  gateway is reachable.
- `GET /readyz` — 200 once initial reconciliation is complete.
- `GET /sessions` — list active mappings (auth: `Authorization: Bearer
  <BRIDGE_ADMIN_TOKEN>` env, optional; if unset, the route is disabled).
- `POST /sessions/{threadId}/interrupt` — manual interrupt (same auth).
- `GET /metrics` — Prometheus format: active session count, events/sec by
  type, Discord rate-limit retries, Forge API errors.

### Containerization
- Single Dockerfile (Go binary, distroless final stage).
- `docker-compose.yml` example in `deploy/` showing bridge + forge-gateway
  + named volumes for `bridge.db` and forge worktrees.
- Health check uses `/healthz`.
- Non-root user. No bind mounts of host paths beyond `/data` and
  `/config`. Forge worktrees are NOT mounted into the bridge container —
  the bridge only talks HTTP to Forge.

## Constraints
- The bridge speaks the public Forge HTTP API only. It MAY import
  `internal/types` for the `OutboundEvent` contract (and similar small
  shared structs) since the binaries ship from the same repo, but it MUST
  NOT import handlers, the bus, the session store, or any other gateway
  internals. The wire is the public API; sharing struct definitions is a
  build-time convenience, not a coupling.
- The bridge MUST handle SSE reconnection. If Forge gateway restarts mid-
  session, the bridge reconnects with backoff and reports the gap as a
  ⚠️ reaction on the most recent message.
- The bridge MUST be idempotent on inbound Discord events (Discord may
  deliver `THREAD_CREATE` multiple times during gateway resumes).
- The bridge MUST NOT post the API key / model name / session id in
  channel content. Session id goes in the `done` summary embed *only* if
  `BRIDGE_REVEAL_SESSION_ID=true`.
- The bridge MUST NOT respond to its own messages (loop prevention).
- Discord thread names: **ASCII only** — see MEMORY.md re: `Invalid Form
  Body` on non-ASCII. Sanitize on rename if we ever auto-rename threads.
- Containers only. No `go run` on the host as a deploy path.

## Interfaces

```go
// cmd/forge-discord-bridge/main.go — single binary entrypoint.

// internal/discord/ — discordgo wrapper, gateway events, reactions, posting.
type Client interface {
    PostMessage(ctx context.Context, threadID, content string, opts ...PostOption) (messageID string, err error)
    EditMessage(ctx context.Context, threadID, messageID, content string) error
    AddReaction(ctx context.Context, threadID, messageID, emoji string) error
    RemoveReaction(ctx context.Context, threadID, messageID, emoji string) error
    ArchiveThread(ctx context.Context, threadID string) error
    SubscribeEvents(ctx context.Context) (<-chan Event, error)
}

// internal/forge/ — Forge HTTP gateway client.
type Forge interface {
    CreateSession(ctx context.Context, cwd string, metadata map[string]any) (sessionID string, err error)
    SendMessage(ctx context.Context, sessionID, text string) error
    Interrupt(ctx context.Context, sessionID string) error
    SubscribeEvents(ctx context.Context, sessionID, sinceEventID string) (<-chan types.OutboundEvent, error)
}

// internal/bridge/ — translation layer.
type Bridge struct {
    forge    Forge
    discord  Client
    store    Store
    cfg      Config
}

func (b *Bridge) OnDiscordEvent(ctx context.Context, evt discord.Event) error
func (b *Bridge) OnForgeEvent(ctx context.Context, threadID string, evt types.OutboundEvent) error

// internal/store/ — SQLite persistence.
type Store interface {
    UpsertSession(ctx context.Context, s SessionRecord) error
    GetSessionByThread(ctx context.Context, threadID string) (*SessionRecord, error)
    ListActiveSessions(ctx context.Context) ([]SessionRecord, error)
    RecordEvent(ctx context.Context, sessionID, eventID, discordMsgID string) error
    EventSeen(ctx context.Context, sessionID, eventID string) (bool, error)
    EnqueuePending(ctx context.Context, item PendingMessage) error
    DequeueDuePending(ctx context.Context, now time.Time, limit int) ([]PendingMessage, error)
}
```

Since the bridge lives in the same repo, it imports `OutboundEvent` and
sibling structs directly from `internal/types`. If we later split the
bridge into its own repo, the move is mechanical: copy the contract struct
over and pin the gateway version it targets.

## Edge Cases
- **Thread created by a bot** (e.g. another integration): ignore, no
  session created.
- **Forge gateway unreachable on thread create**: post `⏳ Forge is
  unreachable. Will retry…` to the thread, enqueue the create, retry with
  backoff. Surface ❌ after 5 minutes of failure.
- **Discord token revoked mid-run**: bridge restarts, fails `/healthz`,
  orchestrator (compose / k8s) restarts it. Sessions resume from
  `last_event_id`.
- **Two humans in the same thread**: both can send messages; both go
  through. Forge sees a flat user message stream. No author attribution
  for now (Forge doesn't model per-message authors). Out of scope: tag
  the human in `commit.coAuthor`.
- **Very long agent output**: chunked into multiple messages on sentence
  boundaries. Code blocks split at fence boundaries to avoid mid-block
  splits.
- **Forge emits a `text` event mid-tool**: buffer it. Don't post until
  the tool resolves (it'd look like the bot is talking over itself).
- **PR creation fails**: surface the `phase_error` and DO NOT silently
  retry. The user can react 🔁 to retry.
- **Discord WebSocket disconnect**: discordgo handles reconnect. While
  disconnected, outbound posts queue in `pending`. Inbound events during
  the gap are recovered via Discord's resume token (discordgo built-in)
  or missed entirely if resume fails — log a warning, post a ⚠️ to the
  starter message of any active thread on reconnect.
- **Same human types two messages in quick succession**: forwarded
  in-order. No coalescing on the bridge side — let Forge's queueing
  decide.
- **Forge session id collision** (UUID birthday-paradox bullshit): not a
  real concern at our scale; treat as impossible. If a UUID collision
  surfaces, log error and refuse the create.

## Tests
- **Unit tests** for the event translator: golden inputs (`OutboundEvent`
  streams) → expected Discord call sequences. Cover all event types
  listed above.
- **Unit tests** for chunking: long text, long code blocks, mixed
  fences, weird whitespace.
- **Integration test** with a fake Forge gateway (in-process HTTP server
  + scripted SSE stream) and a Discord stub. Drives: thread create →
  session create → message → events → done → summary.
- **Restart resilience test**: kill the bridge mid-stream, restart, assert
  no duplicate Discord posts and final state converges.
- **Idempotency test**: replay the same `THREAD_CREATE` twice; assert one
  session, one mapping.
- **Container test**: `docker build` succeeds; the resulting image runs
  `/healthz` green against a stub.

## Rollout
1. Build the bridge in this repo under `cmd/forge-discord-bridge/`.
   New binary, new Dockerfile, new compose entry. Existing `forge` /
   `forge gateway` / `forge agent` binaries untouched.
2. Create a new Discord application "Troy" (separate from `pelton`).
   Required bot scopes: `bot`, `applications.commands`. Bot permissions:
   `View Channels`, `Send Messages`, `Send Messages in Threads`, `Create
   Public Threads`, `Manage Threads` (archive), `Add Reactions`, `Read
   Message History`, `Embed Links`, `Attach Files`.
3. Stage: deploy on the Mac mini via docker-compose alongside a local
   Forge gateway. Test against a private channel in Study Room F.
4. Iterate on event-translation noise level until threads feel like
   conversation, not telemetry.
5. Cut a `v0.1.0` tag once a Troy PR is opened, reviewed, and merged
   *via the bridge end-to-end*.

## Out of Scope
- **Resumable SSE in Forge gateway.** Bridge will replay-from-zero +
  dedupe via `forge_event_id` until Forge gateway adds a resume header.
  Separate spec: extend `GET /events` to honor `Last-Event-ID`.
- **Multi-guild support.** v1 is single-guild. The config shape is
  guild-agnostic so we can add later.
- **Slack / Mattermost / Matrix bridges.** Same translator core, but
  delivery layer differs. Not now.
- **Keycard-vended credentials for the bridge.** v1 reads
  `DISCORD_BOT_TOKEN` from env. Phase 2 fetches it from Keycard at boot.
- **Per-message human attribution in commits.** Out of this spec; tracked
  separately under the autonomous-attribution follow-ups (gateway-injected
  `GIT_AUTHOR_*`).
- **Inline action buttons** (Discord components v2). v1 uses emoji
  reactions only — broad client support, no migration friction. Buttons
  later.
- **Voice/stage channels.** lol no.
