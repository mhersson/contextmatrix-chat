package chatwork

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-chat/internal/mcpbridge"
	"github.com/mhersson/contextmatrix-chat/internal/secrets"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
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
// the tool registry and harness config, and drives one harness.Run to
// completion. The /clear epoch-reset loop is deferred to task 3.4b.
func Run(ctx context.Context) error {
	// 1. Open secrets.
	src, err := secrets.Open(secretsEnvPath)
	if err != nil {
		return fmt.Errorf("read secrets: %w", err)
	}

	openRouterKey := src.Get("OPENROUTER_API_KEY")
	gitToken := src.Get("CM_GIT_TOKEN")

	// 2. Configure the git credential helper so clones authenticate via the
	// rotating token in the secrets env file.
	if err := ConfigureGitCredentialHelper(ctx, secretsEnvPath); err != nil {
		return fmt.Errorf("configure git credential helper: %w", err)
	}

	// 3. Clone the project repo into /workspace (best-effort: a clone failure
	// is logged but must not kill the session — the model can re-clone via
	// Path-B tools).
	if repoURL := os.Getenv("CM_CHAT_REPO_URL"); repoURL != "" {
		cloneDir := filepath.Join(workspaceRoot, cloneTarget())

		cmd := exec.CommandContext(ctx, "git", "clone", repoURL, cloneDir) //nolint:gosec // G702: repoURL is the operator-supplied CM_CHAT_REPO_URL
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("repo clone failed; continuing", //nolint:gosec // G706: operator-supplied env var
				"url", repoURL, "dir", cloneDir,
				"error", err, "output", strings.TrimSpace(string(out)))
		}
	}

	// 4. Build the LLM client and resolve the model's context window from the
	// live catalog. Catalog failures are non-fatal: the default window is used
	// so the session still runs.
	client := llm.NewClient(openRouterKey, llm.WithRetry(llm.DefaultRetryPolicy()))
	ctxWindow := defaultContextWindow
	model := os.Getenv("CM_MODEL")

	if cat, err := client.FetchCatalog(ctx); err != nil {
		slog.Warn("fetch catalog failed; using default context window", "error", err)
	} else if entry, ok := cat.Find(model); ok {
		ctxWindow = entry.ContextLength
	}

	// 5. Tool registry: filesystem/shell tools + optional skill tool + MCP board tools.
	reg, bridge, err := buildToolRegistry(ctx)
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

	// 8. Redactor: mask the secrets from all tool output and event data.
	red := redact.New([]string{openRouterKey, gitToken, os.Getenv("CM_MCP_API_KEY")})

	// 9. Compaction config from env, with documented defaults.
	threshold := envFloatDefault("CMX_COMPACTION_THRESHOLD", defaultCompactionThreshold)
	keepRecent := envIntDefault("CMX_COMPACTION_KEEP_RECENT_TURNS", defaultKeepRecentTurns)

	// 10. Inbox: channel-backed; Pump reads stdin frames in a goroutine and
	// closes the inbox on EOF so harness.Run exits when the host closes stdin.
	in := newChatInbox()
	go in.Pump(os.Stdin)

	// 11. Emitter: board tool_call lines are filtered from the transcript
	// (noise reduction, matching the runner's mcp__* skip). All other events
	// reach stdout for the serve-side log bridge.
	filteredWriter := newBoardFilterWriter(os.Stdout, bridge.BoardToolNames())
	emit := events.NewEmitter(io.Discard, filteredWriter)

	// 12. Run once. The /clear epoch-reset loop is task 3.4b.
	cfg := harness.Config{
		Model:            model,
		ContextWindow:    ctxWindow,
		Interactive:      true,
		MaxTurns:         0,
		MaxCostUSD:       0,
		Compaction:       &harness.Compaction{Threshold: threshold, KeepRecentTurns: keepRecent},
		History:          history,
		Inbox:            in,
		RedactToolOutput: red.Apply,
		SystemPrompt:     chatSystemPrompt,
	}

	res, err := harness.Run(ctx, client, reg, emit, primer, cfg)

	slog.Info("chat session finished",
		"reason", res.Reason,
		"turns", res.Turns,
		"cost_usd", res.TotalCostUSD)

	if err != nil {
		return fmt.Errorf("harness run: %w", err)
	}

	return nil
}

// buildToolRegistry assembles the model-facing tool registry: filesystem/shell
// tools rooted at /workspace, the optional skill tool, and the MCP board tools
// from the Connect bridge.
func buildToolRegistry(ctx context.Context) (*tools.Registry, *mcpbridge.Bridge, error) {
	mcpURL := os.Getenv("CM_MCP_URL")
	mcpAPIKey := os.Getenv("CM_MCP_API_KEY")

	bridge, err := mcpbridge.Connect(ctx, mcpURL, mcpAPIKey)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to mcp: %w", err)
	}

	ts := []tools.Tool{
		tools.NewReadTool(workspaceRoot),
		tools.NewWriteTool(workspaceRoot),
		tools.NewEditTool(workspaceRoot),
		tools.NewBashTool(workspaceRoot),
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
