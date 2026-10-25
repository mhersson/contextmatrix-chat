package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-chat/internal/config"
	"github.com/mhersson/contextmatrix-chat/internal/executor"
	"github.com/mhersson/contextmatrix-chat/internal/metrics"
	"github.com/mhersson/contextmatrix-chat/internal/webhook"
)

// stubExecutor implements executor.Executor for unit tests. Kill records the
// session IDs it receives; Launch is a no-op.
type stubExecutor struct {
	kills []string
}

func (e *stubExecutor) Launch(_ context.Context, _ executor.LaunchSpec) error { return nil }

func (e *stubExecutor) Stop(_ context.Context, _ string) error { return nil }

func (e *stubExecutor) Kill(_ context.Context, sessionID string) error {
	e.kills = append(e.kills, sessionID)

	return nil
}

func TestChatExit(t *testing.T) {
	t.Parallel()

	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.SessionID }, nil)

	id, ch := hub.Subscribe("sess-1")
	defer hub.Unsubscribe(id)

	chatRunDir := t.TempDir()
	sessionID := "sess-1"
	runDir := filepath.Join(chatRunDir, sessionID)
	require.NoError(t, os.MkdirAll(runDir, 0o750))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	exitFn := chatExit(hub, chatRunDir, logger)
	exitFn(sessionID, 42)

	// Assert terminal LogEntry was published.
	select {
	case entry := <-ch:
		assert.Equal(t, "system", entry.Type)
		assert.Equal(t, sessionID, entry.SessionID)
		assert.Contains(t, entry.Content, "container exited")
		assert.Contains(t, entry.Content, "42")
	case <-time.After(time.Second):
		t.Fatal("timeout: no LogEntry published to hub")
	}

	// Assert per-session run dir was removed.
	_, err := os.Stat(runDir)
	assert.True(t, os.IsNotExist(err), "run dir must be removed after container exit")
}

func TestComposeLLMBaseURL(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "http://host.docker.internal:9093/llm", composeLLMBaseURL("", 9093))
	assert.Equal(t, "http://10.0.0.5:19093/llm", composeLLMBaseURL("http://10.0.0.5:19093", 9093))
}

func TestComposeMCPURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base string
		want string
	}{
		{"no trailing slash", "http://host:8080", "http://host:8080/mcp"},
		{"trailing slash", "http://host:8080/", "http://host:8080/mcp"},
		{"with subpath", "http://host:8080/contextmatrix", "http://host:8080/contextmatrix/mcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, composeMCPURL(tt.base))
		})
	}
}

func TestComposeGitCredentialsURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base string
		want string
	}{
		{"no trailing slash", "http://host:8080", "http://host:8080/api/worker/git-credentials"},
		{"trailing slash", "http://host:8080/", "http://host:8080/api/worker/git-credentials"},
		{"with subpath", "http://host:8080/contextmatrix", "http://host:8080/contextmatrix/api/worker/git-credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, composeGitCredentialsURL(tt.base))
		})
	}
}

// TestGracefulShutdown verifies that gracefulShutdown sets the draining flag and
// calls Kill for every tracked session. A never-started httpServer is used so
// Shutdown drains immediately and the test stays Docker-free.
//
// runServe itself requires Docker and is covered by the integration test suite.
func TestGracefulShutdown(t *testing.T) {
	t.Parallel()

	var draining atomic.Bool

	tracker := executor.NewTracker(2)
	exec := &stubExecutor{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tracker.AddIfUnderLimit(&executor.Run{SessionID: "s1"})
	tracker.AddIfUnderLimit(&executor.Run{SessionID: "s2"})

	// A never-started server: Shutdown sets the shutdown flag and returns
	// immediately because there are no listeners or idle connections to drain.
	httpSrv := &http.Server{
		Addr:              ":0",
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	gracefulShutdown(httpSrv, nil, exec, tracker, &draining, logger)

	assert.True(t, draining.Load(), "draining flag must be set after shutdown")
	assert.Len(t, exec.kills, 2, "Kill must be called once per tracked session")
}

func TestBuildAdminServer_DisabledWhenZero(t *testing.T) {
	cfg := &config.ServiceConfig{AdminPort: 0}
	srv := webhook.NewServer(webhook.Config{APIKey: "k"})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	assert.Nil(t, buildAdminServer(cfg, srv, metrics.New(), logger))
}

func TestBuildAdminServer_LoopbackAddr(t *testing.T) {
	cfg := &config.ServiceConfig{AdminPort: 9094}
	srv := webhook.NewServer(webhook.Config{APIKey: "k"})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	admin := buildAdminServer(cfg, srv, metrics.New(), logger)
	require.NotNil(t, admin)
	assert.Equal(t, "127.0.0.1:9094", admin.Addr, "empty bind addr defaults to loopback")
}

func TestBuildAdminServer_CustomBindAddr(t *testing.T) {
	cfg := &config.ServiceConfig{AdminPort: 9094, AdminBindAddr: "0.0.0.0"}
	srv := webhook.NewServer(webhook.Config{APIKey: "k"})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	admin := buildAdminServer(cfg, srv, metrics.New(), logger)
	require.NotNil(t, admin)
	assert.Equal(t, "0.0.0.0:9094", admin.Addr)
}

func TestWarnIfNotWorkerUID(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		uid      int
		wantWarn bool
	}{
		{"linux non-1000 warns", "linux", 501, true},
		{"linux root warns", "linux", 0, true},
		{"linux 1000 is quiet", "linux", 1000, false},
		{"non-linux is quiet", "darwin", 501, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			logger := slog.New(slog.NewTextHandler(&buf, nil))
			warnIfNotWorkerUID(logger, tt.goos, tt.uid)

			if tt.wantWarn {
				assert.Contains(t, buf.String(), "level=WARN")
			} else {
				assert.Empty(t, buf.String())
			}
		})
	}
}

func TestChatConfig_LLMBaseURL(t *testing.T) {
	t.Parallel()

	got := chatConfig(&config.ServiceConfig{Port: 9093}, "http://cm:8080")

	assert.Equal(t, "http://host.docker.internal:9093/llm", got.LLMBaseURL)
}

func TestChatConfig_CarriesReplayReasoning(t *testing.T) {
	t.Parallel()

	got := chatConfig(&config.ServiceConfig{ReasoningEffort: "high", ReplayReasoning: true}, "http://cm:8080")

	assert.Equal(t, "high", got.ReasoningEffort)
	assert.True(t, got.ReplayReasoning)
	assert.Equal(t, "http://cm:8080/mcp", got.MCPURL)
}
