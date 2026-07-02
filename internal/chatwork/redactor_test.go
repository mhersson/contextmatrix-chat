package chatwork

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedactorWatcherPicksUpRotatedToken verifies that when the host-side
// Refresher rewrites the secrets env file mid-session, Apply starts masking
// the new token — not just the boot-time one — without restarting the worker.
func TestRedactorWatcherPicksUpRotatedToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	require.NoError(t, os.WriteFile(path,
		[]byte("LLM_API_KEY=llm-key-123456\nCM_GIT_TOKEN=ghs_boottime123\n"), 0o600))

	w, err := newRedactorWatcher(path, "")
	require.NoError(t, err)
	w.pollInterval = 5 * time.Millisecond

	assert.Equal(t, "token=[REDACTED]", w.Apply("token=ghs_boottime123"))
	assert.Equal(t, "token=ghs_rotated654321", w.Apply("token=ghs_rotated654321"), "rotated token not yet known")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.watch(ctx)

	require.NoError(t, os.WriteFile(path,
		[]byte("LLM_API_KEY=llm-key-123456\nCM_GIT_TOKEN=ghs_rotated654321\n"), 0o600))

	require.Eventually(t, func() bool {
		return w.Apply("token=ghs_rotated654321") == "token=[REDACTED]"
	}, 2*time.Second, 5*time.Millisecond, "expected rotated token to be masked after reload")
}

// TestRedactorWatcherStopsOnContextCancel proves the watch goroutine's ticker
// loop exits promptly when ctx is canceled, matching the epoch loop's
// shutdown contract — no leaked goroutine per session.
func TestRedactorWatcherStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	require.NoError(t, os.WriteFile(path, []byte("CM_GIT_TOKEN=ghs_tok123456\n"), 0o600))

	w, err := newRedactorWatcher(path, "")
	require.NoError(t, err)
	w.pollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.watch(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return after context cancel")
	}
}
