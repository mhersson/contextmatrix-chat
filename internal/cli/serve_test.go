package cli

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-chat/internal/executor"
	"github.com/mhersson/contextmatrix-chat/internal/logbridge"
	"github.com/mhersson/contextmatrix-chat/internal/webhook"
)

// stubExecutor implements executor.Executor for unit tests. Kill records the
// session IDs it receives; Launch and StopAll are no-ops.
type stubExecutor struct {
	kills []string
}

func (e *stubExecutor) Launch(_ context.Context, _ executor.LaunchSpec) error { return nil }

func (e *stubExecutor) Stop(_ context.Context, _ string) error { return nil }

func (e *stubExecutor) Kill(_ context.Context, sessionID string) error {
	e.kills = append(e.kills, sessionID)

	return nil
}

func (e *stubExecutor) StopAll(_ context.Context) ([]*executor.Run, error) { return nil, nil }

func TestChatExit(t *testing.T) {
	t.Parallel()

	hub := logbridge.NewHub()

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

func TestComposeMCPURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base string
		want string
	}{
		{"no trailing slash", "http://host:8080", "http://host:8080/mcp"},
		{"trailing slash", "http://host:8080/", "http://host:8080/mcp"},
		{"double trailing slash", "http://host:8080//", "http://host:8080/mcp"},
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
		{"double trailing slash", "http://host:8080//", "http://host:8080/api/worker/git-credentials"},
		{"with subpath", "http://host:8080/contextmatrix", "http://host:8080/contextmatrix/api/worker/git-credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, composeGitCredentialsURL(tt.base))
		})
	}
}

// TestHealthEndpoint is a smoke test: build a Server with a minimal config and
// verify that GET /health (unauthenticated) returns 200.
func TestHealthEndpoint(t *testing.T) {
	t.Parallel()

	srv := webhook.NewServer(webhook.Config{APIKey: "test-api-key-for-serve-health-smoke"})

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
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
