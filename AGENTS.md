# AGENTS.md — ContextMatrix Chat

## What is this project?

`contextmatrix-chat` is the **chat backend** for ContextMatrix. It runs
interactive AI chat sessions — one Docker worker container per session — and
bridges the board's MCP tools into each session, so the model can read and
write cards while it talks to a human. One binary, two runtime roles:

- **`serve`** — the host-side service. Hosts ContextMatrix's chat lifecycle
  webhooks, launches one worker container per session, stages secrets, streams
  container logs back over SSE, and drains on shutdown.
- **`work`** — the container entrypoint (hidden). Runs one interactive chat
  session: assembles the tool registry, optionally clones the project repo,
  seeds history on resume, and drives the epoch loop.

The interactive loop itself is the shared `contextmatrix-harness` (v0.2.0):
in-context compaction, seeded-history resume, and an interactive turn loop. The
**model is chosen by ContextMatrix** and arrives in the chat-start payload —
chat never selects a model.

## How it fits ContextMatrix

ContextMatrix splits its execution backends:

- **TaskBackend** — card execution (`contextmatrix-runner` or
  `contextmatrix-agent`).
- **ChatBackend** — interactive chat sessions (this service).

The chat backend is selected and configured operator-side; it coexists with
whichever task backend is active.

## Channels to ContextMatrix

| Channel          | Direction         | Transport                                                                          |
| ---------------- | ----------------- | ---------------------------------------------------------------------------------- |
| Chat lifecycle   | CM → serve        | HTTP webhooks, HMAC-signed: `POST /chat/start`, `POST /chat/end`, `POST /message`  |
| Log stream       | serve → CM        | Server-Sent Events: `GET /logs?session_id=…`                                       |
| Board operations | worker → CM       | **MCP** at `<container_contextmatrix_url>/mcp`, never raw HTTP                      |
| Session control  | serve → container | JSON-Lines frames on container stdin: user-message / clear                         |

The HMAC `api_key` is real auth (a shared secret, replay-protected); the same
scheme guards the admin `/metrics` endpoint. Board progress runs over **MCP** —
never raw HTTP.

## Architecture

```
cmd/contextmatrix-chat/main.go → entrypoint; runs the cobra root command
internal/cli/                  → cobra commands: serve (host service), work (hidden container entrypoint)
internal/config/               → layered service config (defaults < file < env, CMX_*) + validation
internal/webhook/              → HTTP surface: /chat/start, /chat/end, /message, /logs (SSE), /health, /readyz; HMAC auth, replay + dedup caches, drain gate
internal/executor/             → Docker container lifecycle; Tracker gates concurrency; DockerExecutor satisfies the Executor interface
internal/chatwork/             → container-side work loop: secrets, git credential helper, optional repo clone, tool registry, primer/resume, epoch loop
internal/mcpbridge/            → dials CM's /mcp, lists board tools, adapts each to a harness tools.Tool
internal/logbridge/            → Hub fanning container log frames to SSE subscribers (per-session or all)
internal/frames/               → JSON-Lines control protocol written to container stdin (user-message, clear)
internal/secrets/              → host-side secrets staging: writes the shared env file (LLM endpoint API key + rotating GitHub token), atomic write+rename
internal/metrics/              → Prometheus metric set on a dedicated registry
docker/Dockerfile.worker       → worker image; entrypoint `contextmatrix-chat work`
```

The interactive loop, LLM client, tool primitives, redaction, and event stream
come from the external `contextmatrix-harness` module.

## Boundary discipline (the load-bearing invariant)

The `contextmatrix-harness` module is FSM-free and dependency-free: it imports
only its own `events` / `llm` / `tools` / `redact` / `harness` packages and
takes **no** `contextmatrix-*` dependency. Chat-specific policy (webhooks,
container lifecycle, MCP bridging, transcript filtering) lives here in
`internal/` and is injected into the harness through the seams it already
exposes — the `Inbox`, the tool registry, the event emitter, the redactor. If a
change tempts you to push chat policy down into the harness module, push the
dependency the other way instead: satisfy a harness interface from a consumer
here.

## Tech stack

- Go 1.26+
- cobra (commands) + koanf (layered config) — not viper
- Docker SDK (`github.com/docker/docker`) — client only, against the local daemon
- `contextmatrix-harness` — the shared interactive work loop
- `contextmatrix-protocol` — webhook/payload + log-entry types
- `contextmatrix-githubauth` — GitHub App / PAT token generation
- Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`) — MCP client
- An OpenAI-compatible LLM endpoint (OpenRouter by default) via the harness `llm` client (raw HTTP, no SDK)
- testify

## Coding conventions

### Go

- Everything lives under `internal/` — nothing exported outside the module.
- Interfaces belong in the package that uses them; the webhook server consumes
  the `executor.Executor` interface that `DockerExecutor` satisfies, for example.
- Wrap errors with `fmt.Errorf("operation: %w", err)`. Never swallow errors.
- `context.Context` is the first parameter of any function that does I/O.
- No global state, no `init()` functions. Dependencies injected via struct
  fields, wired in `cli/serve` and `chatwork`.
- Constructors return concrete types; consumers take interfaces.
- Logging: `log/slog` with structured fields. No `fmt.Println` in production
  paths; container-side events go through the harness event stream, not ad-hoc
  printing.
- Tests sit next to code (`handler.go` → `handler_test.go`), table-driven, with
  `t.Helper()` in helpers and `t.TempDir()` for scratch dirs.
- Format with `gofumpt -w .` (`make fmt`), not `gofmt`. CI flags the difference.
- Spell names out. Use "chat", "runner", "agent" — no abbreviations in config
  keys, code, comments, or commit messages.

### GitHub auth

All GitHub tokens come from `githubauth` providers (App or PAT) via the secrets
refresher. Do not read raw tokens from config or env in new code paths.

### Config

Precedence: defaults < file < env. `CMX_*` env prefix; nested keys use `__`
(`CMX_GITHUB__AUTH_MODE`, `CMX_COMPACTION__THRESHOLD`). The koanf wire shape
(`serviceRaw`) is kept separate from the typed `ServiceConfig` so the public
struct never carries half-parsed values. Always `Validate()` after merging.

### Documentation

Document the CURRENT STATE, not changed state. What exists NOW and WHY, not how
we got here.

## Key domain rules

1. **One container per session.** serve refuses a second container for a live
   session (409) and enforces `max_concurrent` (429) before touching Docker.
2. **serve owns the container lifecycle** — launch, resource caps (memory,
   pids), secret refresh, orphan cleanup on startup, and graceful drain (flip
   draining → HTTP shutdown → kill tracked containers). It makes no status
   callback; the chat backend has no ContextMatrix reporter.
3. **work runs the interactive epoch loop.** Each epoch is one `harness.Run`. A
   `/clear` frame ends the epoch, resets history to nil, and the loop blocks for
   the next primer before starting fresh. Resume seeds history from
   `resume.jsonl` when `CM_CHAT_RESUME=1`.
4. **Compaction is on** (harness): older turns drop at `compaction.threshold`
   (default 0.85) of the model's context window; `keep_recent_turns` (default 6)
   are retained verbatim.
5. **Model and MCP key come from ContextMatrix** in the chat-start payload
   (`CM_MODEL`, `CM_MCP_API_KEY`). Chat does not select models or stage the MCP
   key.
6. **Board operations go over MCP**, never raw HTTP. The worker dials
   `<container_contextmatrix_url>/mcp`, lists board tools, and offers them to the
   model alongside the filesystem/shell tools rooted at `/workspace`.
7. **Secrets are staged, never baked.** serve writes `<secrets_dir>/shared/env`
   (LLM endpoint API key + rotating GitHub token) and bind-mounts it read-only at
   `/run/cm-secrets`. The worker's git credential helper reads the token on each
   call, so rotation is transparent. The helper and `GH_HOST` are scoped to
   `github.host` (forwarded as `CM_GIT_HOST`; falls back to the seeded repo
   URL's host, then github.com), so GHE clones authenticate even in
   cross-project sessions. Secrets are redacted from all tool output and
   events.
8. **task-skills come from ContextMatrix** (the single source of truth): serve
   fetches a `{git_remote_url, ref}` pointer from CM, clones it on the host, and
   bind-mounts the clone read-only at `/run/cm-skills`. The model engages them
   via the `Skill` tool. Chat carries no task-skills config.
9. **Webhooks are HMAC-authenticated, replay-protected, and deduplicated.**
   `/message` is idempotent by `message_id`: a retry returns a cached ack without
   re-writing the frame to stdin.
10. **Session IDs are path-validated** before they touch the filesystem (no
    empty, no separators, no `..`) so per-session run-dir cleanup cannot escape
    its base.

Not part of chat — do not document these as if they were: orchestrator phases,
review specialists, model selection / registry / blacklist, per-card budget
ceilings, HITL gates, git autosquash / force-push.

## Observability

Prometheus metrics live on a dedicated registry, exposed at `GET /metrics` on
the loopback admin listener (`127.0.0.1:<admin_port>`) behind the same HMAC
scheme as the webhooks. `admin_port: 0` disables it. The series:

- `cm_chat_webhook_requests_total{endpoint,status,code}`
- `cm_chat_webhook_request_duration_seconds{endpoint}`
- `cm_chat_container_duration_seconds{outcome}`
- `cm_chat_running_containers`
- `cm_chat_broadcaster_drops_total`

plus the standard `go_*` / `process_*` collectors on the same registry.

## Running and testing

```bash
make build          # go build ./... + the contextmatrix-chat binary
make test           # go test ./...
make test-race      # CGO_ENABLED=1 go test -race ./...
make lint           # golangci-lint run
make fmt            # gofumpt -w .
make docker-worker  # build the worker image (contextmatrix-chat-worker:dev)
```

Executor tests that need a real Docker daemon are gated by an env var
(`CMX_TEST_DOCKER`) and skip when it is unset.

## Uncommitted artifacts

Gitignored, never committed: `/contextmatrix-chat` (the binary), `*.test`,
`.envrc`, `transcripts/`.

## Mandatory verification before proceeding

1. `go build ./...` — zero errors.
2. `make test` — no regressions; `go test -race ./...` clean.
3. `make lint` — clean.
4. `gofumpt -l .` — empty.

## Commit discipline

```bash
make fmt    # gofumpt -w . — CI flags any gofmt-vs-gofumpt difference
make test   # must be clean before every commit
make lint   # must be clean before every commit
make build  # must build
```

NEVER commit code without manual approval from the user. No exceptions.

NEVER reference a plan phase, slice ID, task number, or a private ContextMatrix
card ID in commit messages, comments, or code — they are meaningless to outside
readers.

ALWAYS keep commit messages short, clear, and focused. Use bullet points in the
body to explain the "what" and "why"; avoid long paragraphs.

ALWAYS write conventional commit messages with a type, scope, and concise
description. For example:

```
feat(webhook): dedup /message retries by message_id
feat(chatwork): reset history on a /clear epoch boundary
fix(executor): kill tracked containers on graceful drain
feat(mcpbridge): adapt board MCP tools to harness tools
```
