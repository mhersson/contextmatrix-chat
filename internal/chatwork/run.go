package chatwork

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	resumePath    = "/run/cm-chat/resume.jsonl"
	workspaceRoot = "/workspace"

	defaultContextWindow       = 128000
	defaultCompactionThreshold = 0.85
	defaultKeepRecentTurns     = 6
)

// Run is the container entrypoint for one interactive chat session. It reads
// the CM-provisioned credentials from its env, configures git auth, optionally
// clones the project repo, assembles the tool registry and harness config, and
// drives the epoch loop to completion. Each epoch is one harness.Run; a /clear
// frame ends the current epoch and starts a fresh one with no history.
func Run(ctx context.Context) error {
	// 1. CM-provisioned LLM endpoint: handleChatStart sets LLM_API_KEY/
	// LLM_BASE_URL/LLM_TYPE as per-session container env (protocol v0.5.0),
	// the same delivery mechanism as CM_CHAT_REPO_URL. All three are always
	// set for a launched session — an empty value is a real provisioned
	// answer (e.g. base_url meaning "the type's canonical default").
	llmKey := os.Getenv("LLM_API_KEY")
	llmBaseURL := os.Getenv("LLM_BASE_URL")
	llmType := os.Getenv("LLM_TYPE")

	// Backstop for handleChatStart's fail-closed launch guard: that guard
	// should already have refused to start any session without a
	// CM-provisioned llm_endpoint, so an empty llmKey here means the guard was
	// bypassed — e.g. an older chat service paired with a newer, more
	// permissive CM. Fail fast with a legible error instead of letting the
	// harness LLM client fail opaquely on the first turn.
	if err := validateLLMKey(llmKey); err != nil {
		return err
	}

	gitCredentialsToken := os.Getenv("CM_GIT_CREDENTIALS_TOKEN")

	// 2. Configure git credential auth (CM_GIT_CREDENTIALS_TOKEN, protocol
	// v0.5.2).
	selfPath, err := configureGitAuth(ctx, gitCredentialsToken)
	if err != nil {
		return err
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
		c, err := tlsca.HTTPClientWithCA(caCertPath)
		if err != nil {
			return fmt.Errorf("build CA http client: %w", err)
		}

		caHTTPClient = c

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

	reg, bridge, err := buildToolRegistry(ctx, selfPath, mcpBase)
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

	// 8. Redactor: mask the secrets from all tool output and event data. Backed
	// by a watcher so a fresh per-repo git token the credential-helper/
	// gh-wrapper subcommands fetch mid-session (see fetchedTokensPath) is
	// picked up without restarting the worker.
	redWatcher := newRedactorWatcher(os.Getenv("CM_MCP_API_KEY"), llmKey, gitCredentialsToken, fetchedTokensPath())

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
	// (noise reduction — the MCP bridge tools, named mcp__*). All other events
	// reach stdout for the serve-side log bridge.
	filteredWriter := newBoardFilterWriter(os.Stdout, bridge.BoardToolNames())
	emit := events.NewEmitter(io.Discard, filteredWriter)

	// 12. Epoch loop: one harness.Run per epoch; /clear resets history and
	// restarts with the embedded primer as the new task. Every epoch's first
	// user turn is the primer (chatPrimer, embedded next to the environment it
	// describes) — the host sends only /clear, never orientation text.
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

	return epochLoop(ctx, clearCh, in, &cfg, chatPrimer, run)
}

// epochLoop drives the per-epoch harness.Run lifecycle. run is called once per
// epoch; if it returns cleared=true the epoch was cut short by a /clear frame,
// History is reset to nil, and the next epoch re-orients itself with the
// embedded primer as its task. The loop exits when run returns cleared=false
// (done or error) or when the inbox is closed with nothing queued at the epoch
// boundary (stdin gone: nobody would drive a fresh epoch).
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
		// the clear frame was read, in-order with the frame stream. Release the
		// hold so any message that arrived after the clear reaches the next
		// epoch's Wait/Drain in order; the epoch's task is the primer itself.
		if closed := inbox.releaseClear(); closed {
			return nil
		}

		task = chatPrimer

		// drain any stale clear signal that arrived between epochs
		select {
		case <-clearCh:
		default:
		}
	}
}

// buildToolRegistry assembles the model-facing tool registry: filesystem/shell
// tools rooted at /workspace, the optional skill tool, and the MCP board tools
// from the Connect bridge. mcpBase is the outbound RoundTripper for the MCP
// bridge (nil uses http.DefaultTransport); it carries the extra CA trust when
// ca_cert_file is configured.
//
// gh wrapper selection: selfPath non-empty means CM provisioned git
// credentials this session (protocol v0.5.2) — install the v2 wrapper, which
// fetches a fresh per-repo credential from CM on every gh invocation via
// selfPath's hidden "gh-wrapper" subcommand. An empty selfPath (no provisioned
// git credentials) leaves gh unwrapped — a footgun the model discovers itself
// if it tries to use it.
func buildToolRegistry(ctx context.Context, selfPath string, mcpBase http.RoundTripper) (*tools.Registry, *mcpbridge.Bridge, error) {
	mcpURL := os.Getenv("CM_MCP_URL")
	mcpAPIKey := os.Getenv("CM_MCP_API_KEY")

	bridge, err := mcpbridge.Connect(ctx, mcpURL, mcpAPIKey, mcpBase)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to mcp: %w", err)
	}

	bashTimeout := envIntDefault("CMX_BASH_TIMEOUT_MAX_SECONDS", 600)

	bashTool := tools.NewBashTool(workspaceRoot).WithMaxTimeout(bashTimeout)

	if selfPath != "" {
		// gh reads GH_TOKEN from the env and has no hook into a rotating
		// credential source; a baked token goes stale mid-session (App
		// installation tokens expire ~60m, and a provisioned session's token is
		// per-repo besides). Install a `gh` shim on PATH that fetches a fresh
		// credential from CM per invocation via the hidden gh-wrapper
		// subcommand instead — GH_HOST is resolved by that subcommand itself
		// from the target repo, not forwarded here (provisioned mode is
		// multi-host by construction).
		if dir, err := installGHWrapperV2(selfPath); err != nil {
			slog.Warn("gh wrapper v2 install failed; gh may be unavailable this session", "error", err)
		} else {
			bashTool = bashTool.WithExtraEnv([]string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")})
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

// validateLLMKey is Run's worker-side backstop for handleChatStart's
// fail-closed launch guard (see the comment at Run's llmKey resolution site):
// an empty llmKey means no CM-provisioned llm_endpoint was available, so this
// session has no way to authenticate any inference call.
func validateLLMKey(llmKey string) error {
	if llmKey == "" {
		return fmt.Errorf("no llm api key available: CM did not provision an llm endpoint")
	}

	return nil
}

// configureGitAuth stages the CM-provisioned git-credentials config into a
// 0600 scratch file the git-credential/gh-wrapper subcommands read from (see
// gitCredentialsConfigPath's doc for why NOT env — they run through the
// harness bash tool's scrubbed environment) and registers the v2 helper
// GLOBALLY, since a provisioned session is multi-host by construction. It
// returns the resolved self path for the gh-wrapper install, "" when git auth
// is unavailable. Setup failures are non-fatal: a degraded git-auth
// environment must not kill an otherwise-usable interactive session. An
// absent token mirrors validateLLMKey's backstop (the launch guard was
// bypassed), but degrades instead of failing — unlike inference, a git-less
// chat session is still usable.
func configureGitAuth(ctx context.Context, gitCredentialsToken string) (string, error) {
	if gitCredentialsToken == "" {
		slog.Warn("CM did not provision git credentials; git auth unavailable this session")

		return "", nil
	}

	if err := secrets.WriteEnvFile(gitCredentialsConfigPath(), map[string]string{
		"CM_GIT_CREDENTIALS_URL":   os.Getenv("CM_GIT_CREDENTIALS_URL"),
		"CM_GIT_CREDENTIALS_TOKEN": gitCredentialsToken,
	}); err != nil {
		return "", fmt.Errorf("stage git-credentials config: %w", err)
	}

	selfPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve self path for git credential helper: %w", err)
	}

	if err := ConfigureGitCredentialHelperV2(ctx, selfPath); err != nil {
		slog.Warn("git credential helper v2 setup failed; continuing without git auth", "error", err)
	}

	return selfPath, nil
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
