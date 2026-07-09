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

	w, err := newRedactorWatcher(path, "", "", "", "")
	require.NoError(t, err)

	w.pollInterval = 5 * time.Millisecond

	assert.Equal(t, "token=[REDACTED]", w.Apply("token=ghs_boottime123"))
	assert.Equal(t, "token=ghs_rotated654321", w.Apply("token=ghs_rotated654321"), "rotated token not yet known")

	ctx := t.Context()

	go w.watch(ctx)

	require.NoError(t, os.WriteFile(path,
		[]byte("LLM_API_KEY=llm-key-123456\nCM_GIT_TOKEN=ghs_rotated654321\n"), 0o600))

	require.Eventually(t, func() bool {
		return w.Apply("token=ghs_rotated654321") == "token=[REDACTED]"
	}, 2*time.Second, 5*time.Millisecond, "expected rotated token to be masked after reload")
}

// TestRedactorWatcherMasksPayloadDeliveredLLMKey verifies that an LLM API key
// delivered only via the per-session container env override (a CM-provisioned
// llm_endpoint, protocol v0.5.0) is masked, even though it never appears in
// the shared secrets env file — the harness redactor set must cover it so the
// key never reaches tool output, events, or logs.
func TestRedactorWatcherMasksPayloadDeliveredLLMKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	// No LLM_API_KEY in the file: it only ever arrives as the payload-delivered
	// llmKey argument, mirroring run.go's env-first-then-file resolution.
	require.NoError(t, os.WriteFile(path, []byte("CM_GIT_TOKEN=ghs_boottime123\n"), 0o600))

	w, err := newRedactorWatcher(path, "", "sk-payload-key-123456", "", "")
	require.NoError(t, err)

	assert.Equal(t, "key=[REDACTED]", w.Apply("key=sk-payload-key-123456"))
}

// TestRedactorWatcherStopsOnContextCancel proves the watch goroutine's ticker
// loop exits promptly when ctx is canceled, matching the epoch loop's
// shutdown contract — no leaked goroutine per session.
func TestRedactorWatcherStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	require.NoError(t, os.WriteFile(path, []byte("CM_GIT_TOKEN=ghs_tok123456\n"), 0o600))

	w, err := newRedactorWatcher(path, "", "", "", "")
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

// TestRedactorWatcherMasksGitCredentialsBearer verifies that the
// CM-provisioned git-credentials bearer (protocol v0.5.2,
// CM_GIT_CREDENTIALS_TOKEN) is masked from construction — it is a static,
// known-at-startup value like the LLM key and MCP key, not something that
// needs to wait for a poll tick.
func TestRedactorWatcherMasksGitCredentialsBearer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o600))

	w, err := newRedactorWatcher(path, "", "", "sess1.git-credentials-bearer-000000", "")
	require.NoError(t, err)

	assert.Equal(t, "bearer=[REDACTED]", w.Apply("bearer=sess1.git-credentials-bearer-000000"))
}

// TestRedactorWatcherMasksWorkerFetchedTokens verifies that a token the
// credential-helper/gh-wrapper subcommands fetch at runtime (recorded via
// recordFetchedToken into fetchedTokensPath, a separate writable scratch file
// from the read-only secretsEnvPath mount) becomes masked once the watcher
// picks it up — the same poll mechanism the GitHub-token-rotation watch
// already relies on, just against a second file.
func TestRedactorWatcherMasksWorkerFetchedTokens(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o600))

	fetchedPath := filepath.Join(dir, "fetched-tokens")

	w, err := newRedactorWatcher(path, "", "", "", fetchedPath)
	require.NoError(t, err)

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

// TestRedactorWatcherToleratesAbsentFetchedTokensFile verifies that a nil/empty
// fetchedPath, and a configured-but-not-yet-created fetchedPath, are both
// harmless — the common case when CM never provisioned git credentials, or a
// provisioned session that has not yet fetched any credential.
func TestRedactorWatcherToleratesAbsentFetchedTokensFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	require.NoError(t, os.WriteFile(path, []byte("CM_GIT_TOKEN=ghs_tok123456\n"), 0o600))

	w, err := newRedactorWatcher(path, "", "", "", filepath.Join(dir, "never-created"))
	require.NoError(t, err)

	assert.Equal(t, "token=[REDACTED]", w.Apply("token=ghs_tok123456"), "static file secrets still work")
}
