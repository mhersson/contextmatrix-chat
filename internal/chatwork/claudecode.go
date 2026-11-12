package chatwork

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
	vkbridge "github.com/mhersson/contextmatrix-vendorkit/mcpbridge"
)

const (
	claudeCodeBinary = "claude"
	cmMCPServerName  = "contextmatrix"   // vendorkit's own bridge is "cm"
	oauthTokenEnv    = "LLM_OAUTH_TOKEN" //nolint:gosec // env var name, not a credential
	effortEnv        = "CM_CLAUDE_CODE_EFFORT"

	repoInstructionsCap  = 64 * 1024
	bashDefaultCeilingMS = 120000
)

// claudeCodeConfig is what the claude_code path reads once at boot.
type claudeCodeConfig struct {
	Token, Model, Effort, SessionID string
	MCPURL, MCPAPIKey               string
	CACertPath                      string // CMX_CA_CERT_FILE; "" when unset
	Home                            string // the worker user's own HOME (os.UserHomeDir)
	BashTimeoutMaxSeconds           int    // CMX_BASH_TIMEOUT_MAX_SECONDS; 0 = not mapped
	RepoDir                         string // repo clone root; "" when CM_CHAT_REPO_URL is unset
	ToolOutputMaxBytes              int    // CMX_TOOL_OUTPUT_MAX_BYTES; caps each bridged tool result
}

// claudeCodePassthrough is the worker env claude needs beyond tools.ScrubbedEnv.
var claudeCodePassthrough = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "RUSTUP_HOME", "CARGO_HOME"}

// takeOAuthToken reads the subscription token once and unsets it so no child
// process inherits it.
func takeOAuthToken() (string, error) {
	tok := os.Getenv(oauthTokenEnv)
	if tok == "" {
		return "", fmt.Errorf("%s is not set", oauthTokenEnv)
	}

	if err := os.Unsetenv(oauthTokenEnv); err != nil {
		return "", fmt.Errorf("unset %s: %w", oauthTokenEnv, err)
	}

	return tok, nil
}

var _ chatSession = (*claudecode.Session)(nil)

// newClaudeCodeTap counts every event line, then passes it through the
// claude_code transcript filter to dst.
func newClaudeCodeTap(dst io.Writer) *eventTap {
	return newEventTap(newClaudeCodeFilterWriter(dst, mcpServerTools(cmMCPServerName)))
}

// runClaudeCode runs a claude_code chat on one Claude Code session in place of
// the harness epoch loop.
func runClaudeCode(ctx context.Context) error {
	tap := newClaudeCodeTap(os.Stdout)
	emit := events.NewEmitter(io.Discard, tap)

	// The token is taken first so the version check's child cannot inherit it.
	token, err := takeOAuthToken()
	if err != nil {
		return bootError(emit, err)
	}

	if err := checkClaudeCode(ctx, claudecode.CheckVersion, emit, slog.Default()); err != nil {
		return err
	}

	mcpURL, err := claudeCodeMCPURL(emit)
	if err != nil {
		return err
	}

	selfPath, gitCredentialsToken, err := prepareWorkspace(ctx)
	if err != nil {
		return err
	}

	sessionID := os.Getenv("CM_CHAT_SESSION")
	if sessionID == "" {
		slog.Warn("CM_CHAT_SESSION is empty; session_id will be blank in the primer and the X-CM-Chat-Session header is not sent, so ContextMatrix skips its session check")
	}

	ts, bashEnv := localTools(selfPath)
	history := loadResumeHistory()

	redWatcher := newSessionRedactor(token, gitCredentialsToken)

	go redWatcher.watch(ctx)

	in := newChatInbox()

	clearCh := make(chan struct{}, 1)
	go in.Pump(os.Stdin, clearCh)

	home, err := os.UserHomeDir()
	if err != nil {
		return bootError(emit, fmt.Errorf("resolve home for claude code: %w", err))
	}

	var repoDir string
	if os.Getenv("CM_CHAT_REPO_URL") != "" {
		repoDir = filepath.Join(workspaceRoot, cloneTarget())
	}

	cfg := claudeCodeConfig{
		Token:                 token,
		Model:                 os.Getenv("CM_MODEL"),
		Effort:                claudeCodeEffort(slog.Default()),
		SessionID:             sessionID,
		MCPURL:                mcpURL,
		MCPAPIKey:             os.Getenv("CM_MCP_API_KEY"),
		CACertPath:            os.Getenv("CMX_CA_CERT_FILE"),
		Home:                  home,
		BashTimeoutMaxSeconds: envIntDefault("CMX_BASH_TIMEOUT_MAX_SECONDS", 0),
		RepoDir:               repoDir,
		ToolOutputMaxBytes:    envIntDefault("CMX_TOOL_OUTPUT_MAX_BYTES", defaultToolOutputMaxBytes),
	}

	spec := claudeCodeSpec(cfg, tools.NewRegistry(ts...), bashEnv, emit, redWatcher.Apply)

	loop := &sessionLoop{
		start: func(ctx context.Context, resume string) (chatSession, error) {
			s := spec
			s.Resume = resume

			sess, err := claudecode.StartSession(ctx, s)
			if err != nil {
				// A typed nil *Session would be a non-nil chatSession.
				return nil, err
			}

			return sess, nil
		},
		inbox:   in,
		clearCh: clearCh,
		emit:    emit,
		tap:     tap,
		redact:  redWatcher.Apply,
		primer:  renderPrimer(sessionID),
	}

	return loop.run(ctx, openingMessage(loop.primer, history))
}

// claudeCodeMCPURL returns CM_MCP_URL. Without it the chat has no board tools,
// so boot fails closed as harness mode's MCP connect does.
func claudeCodeMCPURL(emit *events.Emitter) (string, error) {
	u := os.Getenv("CM_MCP_URL")
	if u == "" {
		return "", bootError(emit, errors.New("CM_MCP_URL is not set: a claude code chat needs the contextmatrix mcp server"))
	}

	return u, nil
}

// claudeCodeEffort returns the endpoint's Claude Code --effort level; the
// chat's reasoning_effort never applies here. A value Claude Code does not
// accept falls back to the CLI default.
func claudeCodeEffort(logger *slog.Logger) string {
	raw := os.Getenv(effortEnv)

	effort := claudecode.NormalizeEffort(raw)
	if raw != "" && effort == "" {
		logger.Warn("claude code effort not accepted; using its default", "effort", raw)
	}

	return effort
}

// bootError shows a fatal boot error in the chat pane and returns it.
func bootError(emit *events.Emitter, err error) error {
	emit.Emit(events.ErrorKind, map[string]any{"error": err.Error()})

	return err
}

// checkClaudeCode fails only when no runnable claude exists; an untested
// version logs one warning and the chat runs.
func checkClaudeCode(ctx context.Context, check func(context.Context, string) (found string, tested bool, err error), emit *events.Emitter, logger *slog.Logger) error {
	found, tested, err := check(ctx, claudeCodeBinary)
	if err != nil {
		wrapped := fmt.Errorf("no usable %s binary (tested release: claude code %s); the chat worker image (or the project's chat_worker_image override) lacks one: %w",
			claudeCodeBinary, claudecode.Version, err)
		emit.Emit(events.ErrorKind, map[string]any{"error": wrapped.Error()})

		return wrapped
	}

	logger.Info("claude code version", "found", found)

	if !tested {
		logger.Warn("claude code version differs from the tested release", "found", found, "tested", claudecode.Version)
	}

	return nil
}

// claudeCodeEnv is the base env for claude and the commands its Bash tool runs:
// the scrubbed allowlist plus proxy/toolchain passthrough, never a secret.
func claudeCodeEnv(cfg claudeCodeConfig, bashEnv []string) []string {
	var env []string

	index := map[string]int{}
	set := func(kv string) {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := index[k]; ok {
			env[i] = kv

			return
		}

		index[k] = len(env)
		env = append(env, kv)
	}

	for _, kv := range tools.ScrubbedEnv(nil) {
		set(kv)
	}

	for _, k := range claudeCodePassthrough {
		if v, ok := os.LookupEnv(k); ok {
			set(k + "=" + v)
		}
	}

	if cfg.CACertPath != "" {
		set("NODE_EXTRA_CA_CERTS=" + cfg.CACertPath)
	}

	if cfg.BashTimeoutMaxSeconds > 0 {
		ms := cfg.BashTimeoutMaxSeconds * 1000
		set("BASH_MAX_TIMEOUT_MS=" + strconv.Itoa(ms))

		// A ceiling below Claude Code's 2-minute default has no effect unless
		// the default is lowered with it.
		if ms < bashDefaultCeilingMS {
			set("BASH_DEFAULT_TIMEOUT_MS=" + strconv.Itoa(ms))
		}
	}

	for _, kv := range bashEnv {
		set(kv)
	}

	return env
}

// claudeCodeSpec builds the session spec: Claude Code's built-ins for the
// filesystem tools, the remaining local tools on vendorkit's bridge, and CM's
// MCP server wired straight in.
func claudeCodeSpec(cfg claudeCodeConfig, local *tools.Registry, bashEnv []string, emit *events.Emitter, redact func(string) string) claudecode.Spec {
	builtins, bridged := claudecode.SplitRegistry(local)
	bridged = capAndRedact(bridged, cfg.ToolOutputMaxBytes, redact)

	prompt := chatSystemPrompt

	if cfg.RepoDir != "" {
		if name, content := repoInstructions(cfg.RepoDir); name != "" {
			prompt += "\n\n## Repository instructions (" + name + ")\n\n" + content
		}
	}

	headers := map[string]string{}
	if cfg.MCPAPIKey != "" {
		headers["Authorization"] = "Bearer " + cfg.MCPAPIKey
	}

	if cfg.SessionID != "" {
		headers["X-CM-Chat-Session"] = cfg.SessionID
	}

	return claudecode.Spec{
		Binary:             claudeCodeBinary,
		Dir:                workspaceRoot,
		Home:               cfg.Home,
		OAuthToken:         cfg.Token,
		Env:                claudeCodeEnv(cfg, bashEnv),
		Model:              cfg.Model,
		SystemPromptAppend: prompt,
		BuiltinTools:       builtins,
		Tools:              bridged,
		MCPServers:         []vkbridge.ServerConfig{{Name: cmMCPServerName, URL: cfg.MCPURL, Headers: headers}},
		Effort:             cfg.Effort,
		Emitter:            emit,
		Redact:             redact,
	}
}

// capAndRedact wraps every tool of a registry served over vendorkit's bridge,
// where the harness's own output cap and redaction do not run. nil stays nil.
func capAndRedact(reg *tools.Registry, maxBytes int, redact func(string) string) *tools.Registry {
	if reg == nil {
		return nil
	}

	all := reg.All()
	wrapped := make([]tools.Tool, 0, len(all))

	for _, t := range all {
		wrapped = append(wrapped, cappedTool{Tool: t, maxBytes: maxBytes, redact: redact})
	}

	return tools.NewRegistry(wrapped...)
}

type cappedTool struct {
	tools.Tool

	maxBytes int
	redact   func(string) string
}

func (c cappedTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	res, err := c.Tool.Execute(ctx, args)
	res.Text = tools.HeadTail(c.redact(res.Text), c.maxBytes)

	if err != nil {
		// Keep the original error (and its wrapping) when nothing changed.
		if msg := tools.HeadTail(c.redact(err.Error()), c.maxBytes); msg != err.Error() {
			err = errors.New(msg)
		}
	}

	return res, err
}

func (c cappedTool) Terminal() bool {
	term, ok := c.Tool.(tools.Terminal)

	return ok && term.Terminal()
}

// repoInstructions returns the clone root's AGENTS.md, else CLAUDE.md, capped
// at repoInstructionsCap. Nothing outside the clone root is read, symlinks included. Claude Code does not load
// the repo's own file because the session restricts settings to the user's.
func repoInstructions(dir string) (name, content string) {
	for _, n := range []string{"AGENTS.md", "CLAUDE.md"} {
		if c, ok := readInstructionFile(dir, filepath.Join(dir, n)); ok {
			return n, c
		}
	}

	return "", ""
}

// readInstructionFile reads path only when its symlink-resolved target stays
// inside root, so a committed link cannot pull in /proc or credential files.
func readInstructionFile(root, path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}

	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}

	rel, err := filepath.Rel(rootReal, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}

	f, err := os.Open(resolved) //nolint:gosec // resolved target confined to the clone root above
	if err != nil {
		return "", false
	}

	defer f.Close() //nolint:errcheck // read-only

	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	// Bound the read before allocating so a huge file cannot exhaust memory.
	data, err := io.ReadAll(io.LimitReader(f, repoInstructionsCap+1))
	if err != nil {
		return "", false
	}

	if len(data) <= repoInstructionsCap {
		return string(data), true
	}

	cut := repoInstructionsCap
	for cut > 0 && !utf8.RuneStart(data[cut]) {
		cut--
	}

	return string(data[:cut]) + "\n[... truncated at 64 KiB ...]", true
}
