package chatwork

import (
	"context"
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

// checkClaudeCode fails only when no runnable claude exists; an untested
// version logs one warning and the chat runs.
func checkClaudeCode(ctx context.Context, check func(context.Context, string) (found string, tested bool, err error), emit *events.Emitter, logger *slog.Logger) error {
	found, tested, err := check(ctx, claudeCodeBinary)
	if err != nil {
		wrapped := fmt.Errorf("claude code %s is required but the chat worker image (or the project's chat_worker_image override) lacks a usable %s binary: %w",
			claudecode.Version, claudeCodeBinary, err)
		emit.Emit(events.ErrorKind, map[string]any{"error": wrapped.Error()})

		return wrapped
	}

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

	if len(headers) == 0 {
		headers = nil
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

// repoInstructions returns the clone root's AGENTS.md, else CLAUDE.md, capped
// at repoInstructionsCap. Nothing below dir is read. Claude Code does not load
// the repo's own file because the session restricts settings to the user's.
func repoInstructions(dir string) (name, content string) {
	for _, n := range []string{"AGENTS.md", "CLAUDE.md"} {
		if c, ok := readInstructionFile(filepath.Join(dir, n)); ok {
			return n, c
		}
	}

	return "", ""
}

func readInstructionFile(path string) (string, bool) {
	f, err := os.Open(path) //nolint:gosec // fixed file name under the clone root
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
