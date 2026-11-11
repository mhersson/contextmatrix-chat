package chatwork

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-harness/events"
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
				assert.Equal(t, 1, strings.Count(logBuf.String(), "level=WARN"))
				assert.Contains(t, logBuf.String(), tt.found)
				assert.Contains(t, logBuf.String(), claudecode.Version)
			} else {
				assert.Empty(t, logBuf.String())
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

	tests := []struct {
		name     string
		files    map[string]string
		dirs     []string
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

			name, content := repoInstructions(dir)
			assert.Equal(t, tt.wantName, name)
			tt.check(t, content)
		})
	}
}
