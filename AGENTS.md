# AGENTS.md — ContextMatrix Chat

Contributor guide for `contextmatrix-chat`, the chat backend for ContextMatrix.
For what the service is, how it fits ContextMatrix, the runtime data flow, and
how to run it, see the [README](README.md). This file covers the invariants,
conventions, and discipline that keep contributions correct.

Two runtime roles in one binary:

- **`serve`** — host service: lifecycle webhooks, one worker container per
  session, secret staging, log SSE, graceful drain.
- **`work`** — hidden container entrypoint: assembles the tool registry,
  optionally clones the project repo, seeds resume history, runs the epoch loop.

The interactive loop, LLM client, tool primitives, redaction, and event stream
come from the shared `contextmatrix-harness` module (version pinned in
`go.mod`).

## Package map

```
cmd/contextmatrix-chat/   entrypoint; runs the cobra root command
internal/cli/             cobra commands: serve, work (hidden)
internal/config/          layered service config (defaults < file < env, CMX_*) + Validate
internal/webhook/         HTTP surface: /chat/start, /chat/end, /message, /logs (SSE), /health, /readyz; HMAC auth, replay + dedup caches, drain gate
internal/executor/        Docker container lifecycle; Tracker gates concurrency; DockerExecutor implements Executor
internal/chatwork/        container work loop: provisioned credentials, git credential helper, optional clone, tool registry, primer/resume, epoch loop
internal/mcpbridge/       dials CM's /mcp, adapts each board tool to a harness tools.Tool
internal/logbridge/       Hub fanning container log frames to SSE subscribers
internal/frames/          JSON-Lines control protocol on container stdin (user-message, clear)
internal/secrets/         KEY=value env-file read/write (atomic write+rename); stages the worker's provisioned git-credentials config
internal/metrics/         Prometheus metric set on a dedicated registry
docker/Dockerfile.worker  worker image; entrypoint `contextmatrix-chat work`
```

## Boundary discipline (the load-bearing invariant)

The `contextmatrix-harness` module is FSM-free and dependency-free: it imports
only its own `events` / `llm` / `tools` / `redact` / `harness` packages and
takes **no** `contextmatrix-*` dependency. Chat-specific policy (webhooks,
container lifecycle, MCP bridging, transcript filtering) lives here in
`internal/` and is injected into the harness through the seams it already
exposes — the `Inbox`, the tool registry, the event emitter, the redactor. If a
change tempts you to push chat policy down into the harness, push the dependency
the other way instead: satisfy a harness interface from a consumer here.

## Coding conventions

### Go

- Everything lives under `internal/` — nothing exported outside the module.
- Interfaces belong in the package that uses them: the webhook server consumes
  the `executor.Executor` interface that `DockerExecutor` satisfies.
- Constructors return concrete types; consumers take interfaces.
- Wrap errors with `fmt.Errorf("operation: %w", err)`. Never swallow errors.
- `context.Context` is the first parameter of any function that does I/O.
- No global state, no `init()`. Dependencies injected via struct fields, wired
  in `cli/serve` and `chatwork`.
- Logging: `log/slog` with structured fields. No `fmt.Println` in production
  paths; container-side events go through the harness event stream.
- Tests sit next to code (`handler.go` → `handler_test.go`), table-driven, with
  `t.Helper()` in helpers and `t.TempDir()` for scratch dirs.
- Spell names out: "chat", "runner", "agent" — no abbreviations in config keys,
  code, comments, or commit messages.

### Credentials

All credentials (LLM endpoint, git, task-skills clone token) are CM-provisioned
per session via the chat-start payload and the task-skills-source endpoint.
Never add local credential config or read raw tokens from config or env in new
code paths.

### Config

koanf, not viper. Precedence: defaults < file < env. `CMX_*` env prefix; nested
keys use `__` (`CMX_COMPACTION__THRESHOLD`, `CMX_COMPACTION__KEEP_RECENT_TURNS`).
The koanf wire shape (`serviceRaw`) is kept separate from the typed
`ServiceConfig` so the public struct never carries half-parsed values. Always
`Validate()` after merging.

### Documentation

Document the CURRENT STATE: what exists NOW and WHY, not how we got here.

## Key domain rules

1. **One container per session.** serve refuses a second container for a live
   session (409) and enforces `max_concurrent` (429) before touching Docker.
2. **serve owns the container lifecycle** — launch, resource caps, orphan
   cleanup on startup, and graceful drain (flip draining → HTTP shutdown →
   kill tracked containers). It makes no status callback to CM.
3. **work runs the interactive epoch loop.** Each epoch is one `harness.Run`. A
   `/clear` frame ends the epoch, resets history to nil, and blocks for the next
   primer. Resume seeds history from `resume.jsonl` when `CM_CHAT_RESUME=1`.
4. **Compaction is on** (harness): older turns drop at `compaction.threshold`
   (default 0.85) of the model's context window; `keep_recent_turns` (default 6)
   are kept verbatim.
5. **Model and MCP key come from ContextMatrix** in the chat-start payload
   (`CM_MODEL`, `CM_MCP_API_KEY`). Chat never selects models or stages the MCP
   key.
6. **Board operations go over MCP, never raw HTTP.** The worker dials
   `<container_contextmatrix_url>/mcp`, lists board tools, and offers them to
   the model alongside the filesystem/shell tools rooted at `/workspace`.
7. **Git credentials are fetched per-repo, per-operation from CM**
   (`CM_GIT_CREDENTIALS_TOKEN`, always provisioned — chat/start fails closed
   without it). At boot, `Run` stages the credentials URL/token into a 0600
   scratch file and registers the global v2 git credential helper and `gh`
   wrapper; both read only that file, never `os.Getenv` (the model's git/`gh`
   calls run through the harness bash tool's scrubbed environment). Each call
   GETs CM's `/api/worker/git-credentials` with the target repo's
   `(host, path)` and mints a fresh token, so one session can span multiple
   projects/hosts. A fetch failure logs one stderr note (never a credential)
   and continues. Minted tokens are recorded to a scratch file the redactor
   polls, so they stay masked.
8. **task-skills come from ContextMatrix** (single source of truth): serve
   fetches a `{git_remote_url, ref}` pointer, clones it on the host, and
   bind-mounts the clone read-only at `/run/cm-skills`. Chat carries no
   task-skills config.
9. **Webhooks are HMAC-authenticated, replay-protected, and deduplicated.**
   `/message` is idempotent by `message_id`: a retry returns a cached ack
   without re-writing the frame to stdin.
10. **Session IDs are path-validated** before touching the filesystem (no empty,
    no separators, no `..`) so per-session run-dir cleanup cannot escape its
    base.

Not part of chat — do not document or implement these as if they were:
orchestrator phases, review specialists, model selection / registry / blacklist,
per-card budget ceilings, HITL gates, git autosquash / force-push.

## Verification & commit discipline

Run before every commit — all must be clean:

```bash
go fix ./...   # modernize stdlib idioms
make fmt       # gofumpt -w . (CI flags any gofmt-vs-gofumpt difference)
make build     # go build ./...
make test      # go test ./...  (also: make test-race)
make lint      # golangci-lint run
```

Also confirm `gofumpt -l .` is empty. Executor tests needing a real Docker
daemon are gated by `CMX_TEST_DOCKER` and skip when it is unset.

**NEVER commit without explicit user approval.** No exceptions.

Commit messages: conventional (`type(scope): description`), short and
imperative, with bullet points in the body for the what/why. NEVER reference a
plan phase, slice ID, task number, or private ContextMatrix card ID — they are
meaningless to outside readers.

```
feat(webhook): dedup /message retries by message_id
feat(chatwork): reset history on a /clear epoch boundary
fix(executor): kill tracked containers on graceful drain
```
