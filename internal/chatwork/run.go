package chatwork

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/mhersson/contextmatrix-chat/internal/mcpbridge"
	"github.com/mhersson/contextmatrix-chat/internal/secrets"
	"github.com/mhersson/contextmatrix-chat/internal/tlsca"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

const (
	secretsEnvPath = "/run/cm-secrets/env" //nolint:gosec // path, not a credential
	primerPath     = "/run/cm-chat/primer.txt"
	resumePath     = "/run/cm-chat/resume.jsonl"
	workspaceRoot  = "/workspace"

	defaultContextWindow       = 128000
	defaultCompactionThreshold = 0.85
	defaultKeepRecentTurns     = 6
)

// Run is the container entrypoint for one interactive chat session. It opens
// secrets, configures git auth, optionally clones the project repo, assembles
// the tool registry and harness config, and drives the epoch loop to completion.
// Each epoch is one harness.Run; a /clear frame ends the current epoch and
// starts a fresh one with no history.
func Run(ctx context.Context) error {
	// 1. Open secrets.
	src, err := secrets.Open(secretsEnvPath)
	if err != nil {
		return fmt.Errorf("read secrets: %w", err)
	}

	llmKey := src.Get("LLM_API_KEY")
	llmBaseURL := src.Get("LLM_BASE_URL")
	llmType := src.Get("LLM_TYPE")
	gitToken := src.Get("CM_GIT_TOKEN")

	// 2. Configure the git credential helper so clones authenticate via the
	// rotating token in the secrets env file. Scoped to the host the token is
	// minted for (GHE-aware) so the token is never offered to an unrelated
	// https host. Non-fatal: a degraded git-auth environment must not kill an
	// otherwise-usable interactive session.
	ghHost := gitHost()
	if err := ConfigureGitCredentialHelper(ctx, secretsEnvPath, ghHost); err != nil {
		slog.Warn("git credential helper setup failed; continuing without git auth", "error", err)
	}

	// 3. Clone the project repo into /workspace (best-effort: a clone failure
	// is logged but must not kill the session — the model can re-clone via
	// Path-B tools).
	if repoURL := os.Getenv("CM_CHAT_REPO_URL"); repoURL != "" {
		cloneDir := filepath.Join(workspaceRoot, cloneTarget())

		cmd := exec.CommandContext(ctx, "git", "clone", "--", repoURL, cloneDir) //nolint:gosec // G702: repoURL is the operator-supplied CM_CHAT_REPO_URL
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("repo clone failed; continuing", //nolint:gosec // G706: operator-supplied env var
				"url", repoURL, "dir", cloneDir,
				"error", err, "output", strings.TrimSpace(string(out)))
		}
	}

	// 4. Build the LLM client and resolve the model's context window from the
	// live catalog. Catalog failures are non-fatal: the default window is used
	// so the session still runs.
	clientOpts := []llm.Option{llm.WithRetry(llm.DefaultRetryPolicy()), llm.WithDialect(dialectFromType(llmType))}
	if llmBaseURL != "" {
		clientOpts = append(clientOpts, llm.WithBaseURL(llmBaseURL))
	}

	// When an extra CA is bind-mounted (ca_cert_file), route the LLM client's
	// outbound TLS through a transport that trusts it so egress via a
	// TLS-inspecting proxy validates. The same transport is shared with the MCP
	// bridge below. Fatal on a malformed PEM: a misconfigured CA would otherwise
	// fail every request with an opaque x509 error.
	var caHTTPClient *http.Client

	if caCertPath := os.Getenv("CMX_CA_CERT_FILE"); caCertPath != "" {
		caHTTPClient, err = tlsca.HTTPClientWithCA(caCertPath)
		if err != nil {
			return fmt.Errorf("build CA http client: %w", err)
		}

		clientOpts = append(clientOpts, llm.WithHTTPClient(caHTTPClient))
	}

	client := llm.NewClient(llmKey, clientOpts...)
	ctxWindow := defaultContextWindow
	model := os.Getenv("CM_MODEL")

	if cat, err := client.FetchCatalog(ctx); err != nil {
		slog.Warn("fetch catalog failed; using default context window", "error", err)
	} else if entry, ok := cat.Find(model); ok && entry.ContextLength > 0 {
		ctxWindow = entry.ContextLength
	}

	// 5. Tool registry: filesystem/shell tools + optional skill tool + MCP board
	// tools. Share the CA transport (when configured) so board tool calls trust
	// the same extra CA as the LLM client.
	var mcpBase http.RoundTripper
	if caHTTPClient != nil {
		mcpBase = caHTTPClient.Transport
	}

	reg, bridge, err := buildToolRegistry(ctx, gitToken, mcpBase)
	if err != nil {
		return err
	}

	defer func() { _ = bridge.Close() }()

	// 6. History: seed from a prior session when CM_CHAT_RESUME == "1".
	var history []llm.Message

	if os.Getenv("CM_CHAT_RESUME") == "1" {
		rc, err := LoadResume(resumePath)
		if err != nil {
			slog.Warn("load resume failed; starting fresh", "error", err)
		} else {
			history = SeedHistory(rc)
		}
	}

	// 7. Primer: the task string passed as the first user turn.
	primer := readPrimer(primerPath)

	// 8. Redactor: mask the secrets from all tool output and event data. Backed
	// by a watcher so a token the host rotates mid-session (App installation
	// tokens expire ~60m) is picked up without restarting the worker.
	redWatcher, err := newRedactorWatcher(secretsEnvPath, os.Getenv("CM_MCP_API_KEY"))
	if err != nil {
		return fmt.Errorf("build redactor: %w", err)
	}

	go redWatcher.watch(ctx)

	// 9. Compaction and tool-output config from env, with documented defaults.
	threshold := envFloatDefault("CMX_COMPACTION_THRESHOLD", defaultCompactionThreshold)
	keepRecent := envIntDefault("CMX_COMPACTION_KEEP_RECENT_TURNS", defaultKeepRecentTurns)
	toolOutputMaxBytes := envIntDefault("CMX_TOOL_OUTPUT_MAX_BYTES", 131072)

	// 10. Inbox: channel-backed; Pump reads stdin frames in a goroutine and
	// closes the inbox on EOF so harness.Run exits when the host closes stdin.
	// clearCh carries /clear signals from the frame reader to the epoch loop.
	in := newChatInbox()

	clearCh := make(chan struct{}, 1)
	go in.Pump(os.Stdin, clearCh)

	// 11. Emitter: board tool_call lines are filtered from the transcript
	// (noise reduction, matching the runner's mcp__* skip). All other events
	// reach stdout for the serve-side log bridge.
	filteredWriter := newBoardFilterWriter(os.Stdout, bridge.BoardToolNames())
	emit := events.NewEmitter(io.Discard, filteredWriter)

	// 12. Epoch loop: one harness.Run per epoch; /clear resets history and
	// restarts with the re-sent primer as the new task.
	cfg := harness.Config{
		Model:              model,
		ContextWindow:      ctxWindow,
		Interactive:        true,
		MaxTurns:           0,
		MaxCostUSD:         0,
		ToolOutputMaxBytes: toolOutputMaxBytes,
		Compaction:         &harness.Compaction{Threshold: threshold, KeepRecentTurns: keepRecent},
		History:            history,
		Inbox:              in,
		RedactToolOutput:   redWatcher.Apply,
		SystemPrompt:       chatSystemPrompt,
		Reasoning:          reasoningRaw(os.Getenv("CMX_REASONING_EFFORT")),
	}

	run := func(ctx context.Context, epochTask string) (bool, error) {
		epochCtx, cancel := context.WithCancel(ctx)

		var cleared atomic.Bool

		go func() {
			select {
			case <-clearCh:
				cleared.Store(true)
				cancel()
			case <-epochCtx.Done():
			}
		}()

		res, err := harness.Run(epochCtx, client, reg, emit, epochTask, cfg)

		cancel()

		slog.Info("chat epoch finished",
			"reason", res.Reason,
			"turns", res.Turns,
			"cost_usd", res.TotalCostUSD)

		wasCleared := cleared.Load()
		if err != nil && !wasCleared {
			return false, fmt.Errorf("harness run: %w", err)
		}

		return wasCleared, nil
	}

	return epochLoop(ctx, clearCh, in, &cfg, primer, run)
}

// epochLoop drives the per-epoch harness.Run lifecycle. run is called once per
// epoch; if it returns cleared=true the epoch was cut short by a /clear frame,
// History is reset to nil, and the loop blocks for the re-sent primer before
// starting the next epoch. The loop exits when run returns cleared=false (done
// or error) or when inbox.Wait returns an error between epochs (inbox closed or
// parent ctx canceled).
func epochLoop(
	ctx context.Context,
	clearCh <-chan struct{},
	inbox *chatInbox,
	cfg *harness.Config,
	task string,
	run func(context.Context, string) (bool, error),
) error {
	for {
		cleared, err := run(ctx, task)
		if !cleared {
			return err
		}

		cfg.History = nil

		// The clear boundary (drop of pre-clear messages) was set in Pump when
		// the clear frame was read, in-order with the frame stream. NextAfterClear
		// releases that hold and blocks for the re-sent primer, which cannot have
		// been swallowed by the dying epoch.
		msg, werr := inbox.NextAfterClear(ctx)
		if werr != nil {
			return nil
		}

		task = msg.Content

		// drain any stale clear signal that arrived between epochs
		select {
		case <-clearCh:
		default:
		}
	}
}

// buildToolRegistry assembles the model-facing tool registry: filesystem/shell
// tools rooted at /workspace, the optional skill tool, and the MCP board tools
// from the Connect bridge. gitToken, when non-empty, gets a `gh` PATH shim
// installed for the bash tool so the model can run `gh` (e.g. `gh pr create`)
// with a token read fresh per invocation. mcpBase is the outbound RoundTripper
// for the MCP bridge (nil uses http.DefaultTransport); it carries the extra CA
// trust when ca_cert_file is configured.
func buildToolRegistry(ctx context.Context, gitToken string, mcpBase http.RoundTripper) (*tools.Registry, *mcpbridge.Bridge, error) {
	mcpURL := os.Getenv("CM_MCP_URL")
	mcpAPIKey := os.Getenv("CM_MCP_API_KEY")

	bridge, err := mcpbridge.Connect(ctx, mcpURL, mcpAPIKey, mcpBase)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to mcp: %w", err)
	}

	bashTimeout := envIntDefault("CMX_BASH_TIMEOUT_MAX_SECONDS", 600)

	bashTool := tools.NewBashTool(workspaceRoot).WithMaxTimeout(bashTimeout)

	if gitToken != "" {
		var ghEnv []string

		// gh reads GH_TOKEN from the env and has no hook into the rotating
		// credential helper git uses; a baked token goes stale in a long session
		// (App installation tokens expire ~60m). Install a `gh` shim on PATH that
		// reads CM_GIT_TOKEN fresh from the secrets file per invocation instead.
		if dir, err := installGHWrapper(secretsEnvPath); err != nil {
			slog.Warn("gh wrapper install failed; gh may be unavailable this session", "error", err)
		} else {
			ghEnv = append(ghEnv, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		}

		// gh cannot infer a GitHub Enterprise host from the git remote and refuses
		// to open a PR without it; GH_HOST names it explicitly. Harmless for
		// github.com. Mirrors the runner entrypoint.
		if host := gitHost(); host != "" {
			ghEnv = append(ghEnv, "GH_HOST="+host)
		}

		if len(ghEnv) > 0 {
			bashTool = bashTool.WithExtraEnv(ghEnv)
		}
	}

	ts := []tools.Tool{
		tools.NewReadTool(workspaceRoot),
		tools.NewWriteTool(workspaceRoot),
		tools.NewEditTool(workspaceRoot),
		bashTool,
		tools.NewGrepTool(workspaceRoot),
		tools.NewGitTool(workspaceRoot),
		tools.NewGlobTool(workspaceRoot),
	}

	if dir := os.Getenv("CMX_TASK_SKILLS_DIR"); dir != "" {
		if st, ok := tools.NewSkillTool(dir, nil, false, nil); ok {
			ts = append(ts, st)
		}
	}

	ts = append(ts, bridge.Tools()...)

	return tools.NewRegistry(ts...), bridge, nil
}

// cloneTarget returns the directory name to use under /workspace when cloning.
// Uses CM_CHAT_PROJECT when set; otherwise derives a name from CM_CHAT_REPO_URL.
func cloneTarget() string {
	if p := os.Getenv("CM_CHAT_PROJECT"); p != "" {
		return p
	}

	return dirFromURL(os.Getenv("CM_CHAT_REPO_URL"))
}

// dirFromURL extracts the last path segment from a git URL, stripping .git.
// Falls back to "repo" when nothing useful can be parsed.
func dirFromURL(u string) string {
	u = strings.TrimSuffix(u, ".git")
	parts := strings.Split(u, "/")

	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			return parts[i]
		}
	}

	return "repo"
}

// gitHost returns the host the git token is valid for: the configured
// github.host forwarded by the launcher as CM_GIT_HOST, falling back to the
// seeded repo URL's host. The fallback alone is wrong for cross-project
// sessions (no repo URL → the credential helper would default to github.com
// while the token is minted for the GHE host). Empty means github.com.
func gitHost() string {
	if h := os.Getenv("CM_GIT_HOST"); h != "" {
		return h
	}

	return hostFromRepoURL(os.Getenv("CM_CHAT_REPO_URL"))
}

// hostFromRepoURL returns the host[:port] of an https repo URL, or "" when
// repoURL is empty or not a parseable URL with a host (e.g. an scp-style
// remote). Used to set GH_HOST so gh recognizes a GitHub Enterprise host.
func hostFromRepoURL(repoURL string) string {
	if repoURL == "" {
		return ""
	}

	u, err := url.Parse(repoURL)
	if err != nil {
		return ""
	}

	return u.Host
}

// envFloatDefault parses an optional float64 env var, returning def when the
// var is absent or malformed (malformed values are logged).
func envFloatDefault(name string, def float64) float64 {
	s := os.Getenv(name)
	if s == "" {
		return def
	}

	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Warn("invalid env var; using default", "name", name, "value", s, "default", def) //nolint:gosec // G706: env var name is a string literal; value is the caller-supplied env var

		return def
	}

	return v
}

// dialectFromType maps the LLM_TYPE env value to the harness wire dialect.
// "openai" → DialectOpenAI; everything else (including "" and "openrouter") →
// DialectOpenRouter (byte-identical to the prior default).
func dialectFromType(s string) llm.Dialect {
	if s == "openai" {
		return llm.DialectOpenAI
	}

	return llm.DialectOpenRouter
}

// reasoningRaw converts a reasoning effort string to a json.RawMessage for
// harness.Config.Reasoning. An empty effort returns nil (reasoning disabled).
func reasoningRaw(effort string) json.RawMessage {
	if effort == "" {
		return nil
	}

	raw, err := (llm.Reasoning{Effort: &effort}).Raw()
	if err != nil {
		return nil
	}

	return raw
}

// envIntDefault parses an optional integer env var, returning def when the
// var is absent or malformed (malformed values are logged).
func envIntDefault(name string, def int) int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}

	v, err := strconv.Atoi(s)
	if err != nil {
		slog.Warn("invalid env var; using default", "name", name, "value", s, "default", def) //nolint:gosec // G706: env var name is a string literal; value is the caller-supplied env var

		return def
	}

	return v
}
