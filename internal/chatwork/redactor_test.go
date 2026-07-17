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

// TestRedactorWatcherMasksPayloadDeliveredLLMKey verifies that the LLM API key
// delivered via the per-session container env (a CM-provisioned llm_endpoint,
// protocol v0.5.0) is masked from construction - the harness redactor set
// must cover it so the key never reaches tool output, events, or logs.
func TestRedactorWatcherMasksPayloadDeliveredLLMKey(t *testing.T) {
	t.Parallel()

	w := newRedactorWatcher("", "sk-payload-key-123456", "", "")

	assert.Equal(t, "key=[REDACTED]", w.Apply("key=sk-payload-key-123456"))
}

// TestRedactorWatcherStopsOnContextCancel proves the watch goroutine's ticker
// loop exits promptly when ctx is canceled, matching the epoch loop's
// shutdown contract - no leaked goroutine per session.
func TestRedactorWatcherStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	w := newRedactorWatcher("", "", "", "")

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

// TestRedactorWatcherMasksGitCredentialsBearer verifies that the
// CM-provisioned git-credentials bearer (protocol v0.5.2,
// CM_GIT_CREDENTIALS_TOKEN) is masked from construction - it is a static,
// known-at-startup value like the LLM key and MCP key, not something that
// needs to wait for a poll tick.
func TestRedactorWatcherMasksGitCredentialsBearer(t *testing.T) {
	t.Parallel()

	w := newRedactorWatcher("", "", "sess1.git-credentials-bearer-000000", "")

	assert.Equal(t, "bearer=[REDACTED]", w.Apply("bearer=sess1.git-credentials-bearer-000000"))
}

// TestRedactorWatcherMasksWorkerFetchedTokens verifies that a token the
// credential-helper/gh-wrapper subcommands fetch at runtime (recorded via
// recordFetchedToken into fetchedTokensPath, a writable scratch file) becomes
// masked once the watcher's poll picks it up.
func TestRedactorWatcherMasksWorkerFetchedTokens(t *testing.T) {
	t.Parallel()

	fetchedPath := filepath.Join(t.TempDir(), "fetched-tokens")

	w := newRedactorWatcher("", "", "", fetchedPath)

	w.pollInterval = 5 * time.Millisecond

	assert.Equal(t, "tok=ghs_workerfetched999999", w.Apply("tok=ghs_workerfetched999999"),
		"not yet fetched: must not be masked")

	ctx := t.Context()

	go w.watch(ctx)

	require.NoError(t, os.WriteFile(fetchedPath, []byte("ghs_workerfetched999999\n"), 0o600))

	require.Eventually(t, func() bool {
		return w.Apply("tok=ghs_workerfetched999999") == "tok=[REDACTED]"
	}, 2*time.Second, 5*time.Millisecond, "expected worker-fetched token to be masked after reload")
}

// TestRedactorWatcherToleratesAbsentFetchedTokensFile verifies that an empty
// fetchedPath, and a configured-but-not-yet-created fetchedPath, are both
// harmless - the common case when CM never provisioned git credentials, or a
// provisioned session that has not yet fetched any credential.
func TestRedactorWatcherToleratesAbsentFetchedTokensFile(t *testing.T) {
	t.Parallel()

	w := newRedactorWatcher("mcp-key-123456", "", "", filepath.Join(t.TempDir(), "never-created"))

	assert.Equal(t, "key=[REDACTED]", w.Apply("key=mcp-key-123456"), "static secrets still masked")
}
