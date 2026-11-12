package chatwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
	vkbridge "github.com/mhersson/contextmatrix-vendorkit/mcpbridge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolNames(ts []tools.Tool) []string {
	names := make([]string, 0, len(ts))
	for _, tl := range ts {
		names = append(names, tl.Name())
	}

	return names
}

func TestLocalTools(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("CMX_TASK_SKILLS_DIR", "")

	ts, bashEnv := localTools("")
	assert.Equal(t, []string{"read", "write", "edit", "bash", "grep", "git", "glob"}, toolNames(ts))
	assert.Nil(t, bashEnv)

	_, bashEnv = localTools("/usr/local/bin/contextmatrix-chat")
	require.Len(t, bashEnv, 1)
	require.True(t, strings.HasPrefix(bashEnv[0], "PATH="))

	first := strings.SplitN(strings.TrimPrefix(bashEnv[0], "PATH="), string(os.PathListSeparator), 2)[0]
	info, err := os.Stat(filepath.Join(first, "gh"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&0o100)
}

func writeSkill(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	skillDir := filepath.Join(dir, "demo")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo\ndescription: a demo skill\n---\nbody\n"), 0o644))

	return dir
}

func TestLocalToolsWithTaskSkills(t *testing.T) {
	t.Setenv("CMX_TASK_SKILLS_DIR", writeSkill(t))

	ts, _ := localTools("")
	assert.Contains(t, toolNames(ts), "skill")

	spec := claudeCodeSpec(testCfg(), tools.NewRegistry(ts...), nil, events.NewEmitter(nil, nil), nil)
	assert.NotContains(t, spec.BuiltinTools, "skill")

	var bridged []string
	for _, tl := range spec.Tools.All() {
		bridged = append(bridged, tl.Name())
	}

	assert.Contains(t, bridged, "skill")
}

func TestTakeOAuthToken(t *testing.T) {
	t.Setenv(oauthTokenEnv, "tok-000000")

	got, err := takeOAuthToken()
	require.NoError(t, err)
	assert.Equal(t, "tok-000000", got)

	_, set := os.LookupEnv(oauthTokenEnv)
	assert.False(t, set)

	t.Setenv(oauthTokenEnv, "")

	_, err = takeOAuthToken()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "tok-000000")
}

func TestCheckClaudeCode(t *testing.T) {
	tests := []struct {
		name       string
		found      string
		tested     bool
		checkErr   error
		wantErr    bool
		wantEvents int
		wantWarn   bool
	}{
		{name: "tested version", found: claudecode.Version, tested: true},
		{name: "untested version", found: "2.0.0", wantWarn: true},
		{name: "no binary", checkErr: exec.ErrNotFound, wantErr: true, wantEvents: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var evBuf, logBuf bytes.Buffer

			emit := events.NewEmitter(nil, &evBuf)
			logger := slog.New(slog.NewTextHandler(&logBuf, nil))
			check := func(context.Context, string) (string, bool, error) {
				return tt.found, tt.tested, tt.checkErr
			}

			err := checkClaudeCode(context.Background(), check, emit, logger)

			if tt.wantErr {
				require.ErrorIs(t, err, exec.ErrNotFound)
			} else {
				require.NoError(t, err)
			}

			lines := strings.Split(strings.TrimSpace(evBuf.String()), "\n")
			if tt.wantEvents == 0 {
				assert.Empty(t, strings.TrimSpace(evBuf.String()))
			} else {
				require.Len(t, lines, tt.wantEvents)

				var ev struct {
					Kind string `json:"kind"`
					Data struct {
						Error string `json:"error"`
					} `json:"data"`
				}

				require.NoError(t, json.Unmarshal([]byte(lines[0]), &ev))
				assert.Equal(t, "error", ev.Kind)
				assert.Contains(t, ev.Data.Error, claudecode.Version)
			}

			if tt.wantWarn {
				var warns []string

				for _, l := range strings.Split(logBuf.String(), "\n") {
					if strings.Contains(l, "level=WARN") {
						warns = append(warns, l)
					}
				}

				require.Len(t, warns, 1)
				assert.Contains(t, warns[0], tt.found)
				assert.Contains(t, warns[0], claudecode.Version)
			} else {
				assert.Zero(t, strings.Count(logBuf.String(), "level=WARN"))
			}
		})
	}
}

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()

	m := make(map[string]string, len(env))

	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		require.True(t, ok, kv)

		_, dup := m[k]
		require.False(t, dup, "duplicate key %s", k)

		m[k] = v
	}

	return m
}

func TestClaudeCodeEnv(t *testing.T) {
	t.Setenv("CM_MCP_API_KEY", "mcp-secret-000000")
	t.Setenv("CM_GIT_CREDENTIALS_TOKEN", "git-secret-000000")
	t.Setenv("LLM_API_KEY", "llm-secret-000000")
	t.Setenv("LLM_OAUTH_TOKEN", "oauth-secret-000000")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret-000000")
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("RUSTUP_HOME", "/home/user/.rustup")
	t.Setenv("PATH", "/usr/bin")

	bashEnv := []string{"PATH=/tmp/gh-bin:/usr/bin"}

	tests := []struct {
		name    string
		cfg     claudeCodeConfig
		present map[string]string
		absent  []string
	}{
		{
			name: "base",
			cfg:  claudeCodeConfig{Home: "/home/user", CACertPath: "/run/cm-ca/ca.crt"},
			present: map[string]string{
				"PATH":                "/tmp/gh-bin:/usr/bin",
				"RUSTUP_HOME":         "/home/user/.rustup",
				"HTTPS_PROXY":         "http://proxy:3128",
				"NODE_EXTRA_CA_CERTS": "/run/cm-ca/ca.crt",
			},
			absent: []string{"GIT_CONFIG_GLOBAL", "BASH_MAX_TIMEOUT_MS", "BASH_DEFAULT_TIMEOUT_MS"},
		},
		{
			name:   "no ca cert",
			cfg:    claudeCodeConfig{Home: "/home/user"},
			absent: []string{"NODE_EXTRA_CA_CERTS"},
		},
		{
			name:    "large bash ceiling keeps default",
			cfg:     claudeCodeConfig{BashTimeoutMaxSeconds: 600},
			present: map[string]string{"BASH_MAX_TIMEOUT_MS": "600000"},
			absent:  []string{"BASH_DEFAULT_TIMEOUT_MS"},
		},
		{
			name: "small bash ceiling lowers default",
			cfg:  claudeCodeConfig{BashTimeoutMaxSeconds: 60},
			present: map[string]string{
				"BASH_MAX_TIMEOUT_MS":     "60000",
				"BASH_DEFAULT_TIMEOUT_MS": "60000",
			},
		},
		{
			name:   "unmapped bash ceiling",
			cfg:    claudeCodeConfig{BashTimeoutMaxSeconds: 0},
			absent: []string{"BASH_MAX_TIMEOUT_MS", "BASH_DEFAULT_TIMEOUT_MS"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := claudeCodeEnv(tt.cfg, bashEnv)

			for _, kv := range env {
				assert.NotContains(t, kv, "-secret-000000")
			}

			m := envMap(t, env)
			for k, v := range tt.present {
				assert.Equal(t, v, m[k], k)
			}

			for _, k := range tt.absent {
				assert.NotContains(t, m, k)
			}
		})
	}
}

func testCfg() claudeCodeConfig {
	return claudeCodeConfig{
		Token:     "tok",
		Model:     "claude-sonnet-4-5",
		Effort:    "high",
		SessionID: "sess-1",
		MCPURL:    "http://cm:8080/mcp",
		MCPAPIKey: "mcp-key",
		Home:      "/home/user",
	}
}

func TestClaudeCodeSpec(t *testing.T) {
	t.Setenv("CMX_TASK_SKILLS_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())

	ts, bashEnv := localTools("")
	local := tools.NewRegistry(ts...)
	emit := events.NewEmitter(nil, nil)
	redact := func(s string) string { return strings.ReplaceAll(s, "secret", "***") }
	cfg := testCfg()

	spec := claudeCodeSpec(cfg, local, bashEnv, emit, redact)

	assert.Equal(t, "/workspace", spec.Dir)
	assert.Equal(t, cfg.Home, spec.Home)
	assert.Equal(t, cfg.Token, spec.OAuthToken)
	assert.Equal(t, cfg.Model, spec.Model)
	assert.Equal(t, cfg.Effort, spec.Effort)
	assert.Equal(t, chatSystemPrompt, spec.SystemPromptAppend)
	assert.Empty(t, spec.SystemPrompt)
	assert.Empty(t, spec.SessionID)
	assert.Empty(t, spec.Resume)
	assert.Equal(t, []string{"Read", "Write", "Edit", "Bash"}, spec.BuiltinTools)
	assert.Equal(t, []string{"grep", "git", "glob"}, toolNames(spec.Tools.All()))
	assert.Equal(t, []vkbridge.ServerConfig{{
		Name: "contextmatrix",
		URL:  cfg.MCPURL,
		Headers: map[string]string{
			"Authorization":     "Bearer " + cfg.MCPAPIKey,
			"X-CM-Chat-Session": cfg.SessionID,
		},
	}}, spec.MCPServers)
	assert.Same(t, emit, spec.Emitter)
	assert.NotContains(t, spec.Redact("a secret"), "secret")
	assert.Equal(t, claudeCodeEnv(cfg, bashEnv), spec.Env)
}

func TestClaudeCodeSpecOmitsEmptyHeaders(t *testing.T) {
	t.Setenv("CMX_TASK_SKILLS_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())

	ts, _ := localTools("")
	cfg := testCfg()
	cfg.MCPAPIKey = ""
	cfg.SessionID = ""

	spec := claudeCodeSpec(cfg, tools.NewRegistry(ts...), nil, events.NewEmitter(nil, nil), nil)
	require.Len(t, spec.MCPServers, 1)
	assert.Equal(t, "contextmatrix", spec.MCPServers[0].Name)
	assert.Empty(t, spec.MCPServers[0].Headers)
}

func TestClaudeCodeSpecRepoInstructions(t *testing.T) {
	t.Setenv("CMX_TASK_SKILLS_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())

	ts, _ := localTools("")
	local := tools.NewRegistry(ts...)
	emit := events.NewEmitter(nil, nil)

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("repo-rule-xyzzy"), 0o644))

	cfg := testCfg()
	cfg.RepoDir = repo

	spec := claudeCodeSpec(cfg, local, nil, emit, nil)
	assert.True(t, strings.HasPrefix(spec.SystemPromptAppend, chatSystemPrompt))
	assert.Contains(t, spec.SystemPromptAppend, "repo-rule-xyzzy")
	assert.Equal(t, "/workspace", spec.Dir)

	cfg.RepoDir = ""
	assert.Equal(t, chatSystemPrompt, claudeCodeSpec(cfg, local, nil, emit, nil).SystemPromptAppend)

	cfg.RepoDir = t.TempDir()
	assert.Equal(t, chatSystemPrompt, claudeCodeSpec(cfg, local, nil, emit, nil).SystemPromptAppend)
}

func TestRepoInstructions(t *testing.T) {
	big := strings.Repeat("a", repoInstructionsCap+1000)
	multibyte := strings.Repeat("a", repoInstructionsCap-1) + strings.Repeat("é", 10)

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.md"), []byte("outside"), 0o644))

	tests := []struct {
		name     string
		files    map[string]string
		dirs     []string
		links    map[string]string // link name -> target; relative targets resolve in the root
		wantName string
		check    func(t *testing.T, content string)
	}{
		{
			name:     "AGENTS.md only",
			files:    map[string]string{"AGENTS.md": "agents"},
			wantName: "AGENTS.md",
			check:    func(t *testing.T, c string) { assert.Equal(t, "agents", c) },
		},
		{
			name:     "AGENTS.md wins over CLAUDE.md",
			files:    map[string]string{"AGENTS.md": "agents", "CLAUDE.md": "claude"},
			wantName: "AGENTS.md",
			check:    func(t *testing.T, c string) { assert.Equal(t, "agents", c) },
		},
		{
			name:     "CLAUDE.md only",
			files:    map[string]string{"CLAUDE.md": "claude"},
			wantName: "CLAUDE.md",
			check:    func(t *testing.T, c string) { assert.Equal(t, "claude", c) },
		},
		{
			name:  "neither",
			files: map[string]string{},
			check: func(t *testing.T, c string) { assert.Empty(t, c) },
		},
		{
			name:     "oversize is capped",
			files:    map[string]string{"AGENTS.md": big},
			wantName: "AGENTS.md",
			check: func(t *testing.T, c string) {
				assert.True(t, strings.HasPrefix(c, big[:repoInstructionsCap]))
				assert.Less(t, len(c), repoInstructionsCap+200)
				assert.Greater(t, len(c), repoInstructionsCap)
			},
		},
		{
			name:     "cut lands on a rune boundary",
			files:    map[string]string{"AGENTS.md": multibyte},
			wantName: "AGENTS.md",
			check: func(t *testing.T, c string) {
				assert.True(t, utf8.ValidString(c))
				assert.True(t, strings.HasPrefix(c, strings.Repeat("a", repoInstructionsCap-1)))
			},
		},
		{
			name:  "nested files are not read",
			files: map[string]string{"vendor/x/AGENTS.md": "nested", "sub/CLAUDE.md": "nested"},
			check: func(t *testing.T, c string) { assert.Empty(t, c) },
		},
		{
			name:     "in-repo symlink is followed",
			files:    map[string]string{"AGENTS.md": "agents"},
			links:    map[string]string{"CLAUDE.md": "AGENTS.md"},
			wantName: "AGENTS.md",
			check:    func(t *testing.T, c string) { assert.Equal(t, "agents", c) },
		},
		{
			name:     "symlink leaving the root is rejected",
			files:    map[string]string{"CLAUDE.md": "claude"},
			links:    map[string]string{"AGENTS.md": filepath.Join(outside, "secret.md")},
			wantName: "CLAUDE.md",
			check:    func(t *testing.T, c string) { assert.Equal(t, "claude", c) },
		},
		{
			name:     "directory named AGENTS.md is skipped",
			dirs:     []string{"AGENTS.md"},
			files:    map[string]string{"CLAUDE.md": "claude"},
			wantName: "CLAUDE.md",
			check:    func(t *testing.T, c string) { assert.Equal(t, "claude", c) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()

			for _, d := range tt.dirs {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o755))
			}

			for rel, content := range tt.files {
				p := filepath.Join(dir, rel)
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
				require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
			}

			for link, target := range tt.links {
				require.NoError(t, os.Symlink(target, filepath.Join(dir, link)))
			}

			name, content := repoInstructions(dir)
			assert.Equal(t, tt.wantName, name)
			tt.check(t, content)
		})
	}
}

func TestClaudeCodeMCPURL(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		wantErr    bool
		wantEvents int
	}{
		{name: "set", url: "http://cm:8080/mcp"},
		{name: "empty is fatal", url: "", wantErr: true, wantEvents: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CM_MCP_URL", tt.url)

			var evBuf bytes.Buffer

			got, err := claudeCodeMCPURL(events.NewEmitter(nil, &evBuf))

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.url, got)
			}

			var kinds []string

			for line := range strings.Lines(evBuf.String()) {
				var ev struct {
					Kind string `json:"kind"`
				}

				require.NoError(t, json.Unmarshal([]byte(line), &ev))

				kinds = append(kinds, ev.Kind)
			}

			assert.Len(t, kinds, tt.wantEvents)

			for _, k := range kinds {
				assert.Equal(t, "error", k)
			}
		})
	}
}

// The claude_code effort comes only from the endpoint (CM_CLAUDE_CODE_EFFORT);
// an empty Spec.Effort means vendorkit sends no --effort.
func TestClaudeCodeEffort(t *testing.T) {
	tests := []struct {
		name            string
		endpointEffort  string
		reasoningEffort string
		want            string
		wantWarn        bool
	}{
		{name: "endpoint effort", endpointEffort: "max", reasoningEffort: "low", want: "max"},
		{name: "unset sends none", want: ""},
		{name: "reasoning effort does not apply", reasoningEffort: "high", want: ""},
		{name: "normalized", endpointEffort: " High ", want: "high"},
		{name: "dropped", endpointEffort: "minimal", want: "", wantWarn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CM_CLAUDE_CODE_EFFORT", tt.endpointEffort)
			t.Setenv("CMX_REASONING_EFFORT", tt.reasoningEffort)
			t.Setenv("CMX_TASK_SKILLS_DIR", "")
			t.Setenv("TMPDIR", t.TempDir())

			var logBuf bytes.Buffer

			cfg := testCfg()
			cfg.Effort = claudeCodeEffort(slog.New(slog.NewTextHandler(&logBuf, nil)))

			ts, bashEnv := localTools("")
			spec := claudeCodeSpec(cfg, tools.NewRegistry(ts...), bashEnv, events.NewEmitter(nil, nil), nil)
			assert.Equal(t, tt.want, spec.Effort)

			if tt.wantWarn {
				assert.Equal(t, 1, strings.Count(logBuf.String(), "level=WARN"))
				assert.Contains(t, logBuf.String(), tt.endpointEffort)
			} else {
				assert.Zero(t, strings.Count(logBuf.String(), "level=WARN"))
			}
		})
	}
}

// The pinned claude version is a wire value duplicated in the build files; all
// three must name the release vendorkit was tested against.
func TestClaudeCodeVersionPinsAgree(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ file, pattern string }{
		{"../../Makefile", `(?m)^CLAUDE_CODE_VERSION\s*\?=\s*(\S+)`},
		{"../../docker/Dockerfile.worker", `(?m)^ARG CLAUDE_CODE_VERSION=(\S+)`},
	} {
		data, err := os.ReadFile(tt.file)
		require.NoError(t, err)

		m := regexp.MustCompile(tt.pattern).FindSubmatch(data)
		require.NotNil(t, m, tt.file)
		assert.Equal(t, claudecode.Version, string(m[1]), tt.file)
	}
}

func TestClaudeCodeTap(t *testing.T) {
	var out bytes.Buffer

	tap := newClaudeCodeTap(&out)

	dropped := `{"kind":"state_change","data":{"stop":"done","turns":1}}` + "\n" +
		`{"kind":"tool_call","data":{"name":"mcp__` + cmMCPServerName + `__get_card"}}` + "\n"
	kept := `{"kind":"state_change","data":{"state":"awaiting_human"}}` + "\n" +
		`{"kind":"tool_call","data":{"name":"Bash"}}` + "\n"

	_, err := tap.Write([]byte(dropped + kept))
	require.NoError(t, err)

	assert.Equal(t, kept, out.String())
	assert.EqualValues(t, 1, tap.counts().awaiting)
}

type stubTool struct {
	name     string
	text     string
	err      error
	terminal bool
}

func (s stubTool) Name() string     { return s.name }
func (s stubTool) Schema() llm.Tool { return llm.Tool{} }
func (s stubTool) Terminal() bool   { return s.terminal }
func (s stubTool) Execute(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Text: s.text}, s.err
}

var errStub = errors.New("stub failure")

func bridgedFor(t *testing.T, cfg claudeCodeConfig, redact func(string) string, ts ...tools.Tool) *tools.Registry {
	t.Helper()

	spec := claudeCodeSpec(cfg, tools.NewRegistry(ts...), nil, events.NewEmitter(nil, nil), redact)

	return spec.Tools
}

func TestBridgedToolsCapAndRedact(t *testing.T) {
	const marker = "SYNTHETIC-SECRET-xyz"

	redact := func(s string) string { return strings.ReplaceAll(s, marker, "[REDACTED]") }
	cfg := testCfg()
	cfg.ToolOutputMaxBytes = 300

	t.Run("output over the cap is head and tail trimmed", func(t *testing.T) {
		big := "HEAD" + strings.Repeat("x", 5000) + "TAIL"
		reg := bridgedFor(t, cfg, redact, stubTool{name: "grep", text: big})

		tl, ok := reg.Get("grep")
		require.True(t, ok)

		res, err := tl.Execute(context.Background(), nil)
		require.NoError(t, err)
		assert.Less(t, len(res.Text), 400)
		assert.True(t, strings.HasPrefix(res.Text, "HEAD"))
		assert.True(t, strings.HasSuffix(res.Text, "TAIL"))
	})

	t.Run("secret in output and error is masked", func(t *testing.T) {
		reg := bridgedFor(t, cfg, redact, stubTool{name: "git", text: "out " + marker, err: errors.New("bad " + marker)})

		tl, _ := reg.Get("git")
		res, err := tl.Execute(context.Background(), nil)
		require.Error(t, err)
		assert.NotContains(t, res.Text, marker)
		assert.Contains(t, res.Text, "[REDACTED]")
		assert.NotContains(t, err.Error(), marker)
		assert.Contains(t, err.Error(), "[REDACTED]")
	})

	t.Run("unchanged error is returned as is", func(t *testing.T) {
		reg := bridgedFor(t, cfg, redact, stubTool{name: "git", err: fmt.Errorf("wrapped: %w", errStub)})

		tl, _ := reg.Get("git")
		_, err := tl.Execute(context.Background(), nil)
		assert.ErrorIs(t, err, errStub)
	})

	t.Run("terminal is delegated", func(t *testing.T) {
		reg := bridgedFor(t, cfg, redact, stubTool{name: "done", terminal: true}, stubTool{name: "git"})

		done, _ := reg.Get("done")
		git, _ := reg.Get("git")

		assert.True(t, done.(tools.Terminal).Terminal())
		assert.False(t, git.(tools.Terminal).Terminal())
	})

	t.Run("builtins are not bridged and an empty bridge stays nil", func(t *testing.T) {
		spec := claudeCodeSpec(cfg, tools.NewRegistry(stubTool{name: "read"}), nil, events.NewEmitter(nil, nil), redact)
		assert.Equal(t, []string{"Read"}, spec.BuiltinTools)
		assert.Nil(t, spec.Tools)
	})
}
