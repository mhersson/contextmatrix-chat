package chatwork

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-chat/internal/frames"
	"github.com/mhersson/contextmatrix-chat/internal/secrets"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDialectFromType(t *testing.T) {
	t.Parallel()

	assert.Equal(t, llm.DialectOpenAI, dialectFromType("openai"))
	assert.Equal(t, llm.DialectOpenRouter, dialectFromType("openrouter"))
	assert.Equal(t, llm.DialectOpenRouter, dialectFromType(""))
}

func TestHostFromRepoURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{"enterprise host", "https://acme.ghe.com/org/repo.git", "acme.ghe.com"},
		{"github.com", "https://github.com/org/repo.git", "github.com"},
		{"empty", "", ""},
		{"scp-style yields no host", "git@github.com:org/repo.git", ""},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, hostFromRepoURL(tc.url))
		})
	}
}

func TestGitHost(t *testing.T) {
	tests := []struct {
		name    string
		gitHost string
		repoURL string
		want    string
	}{
		{"configured host wins over repo URL", "acme.ghe.com", "https://github.com/org/repo.git", "acme.ghe.com"},
		{"configured host without repo URL (cross-project)", "acme.ghe.com", "", "acme.ghe.com"},
		{"falls back to repo URL host", "", "https://acme.ghe.com/org/repo.git", "acme.ghe.com"},
		{"neither set", "", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CM_GIT_HOST", tc.gitHost)
			t.Setenv("CM_CHAT_REPO_URL", tc.repoURL)

			assert.Equal(t, tc.want, gitHost())
		})
	}
}

// TestClearBoundaryDeliversPostClearPrimer verifies that a /clear during
// active work establishes an epoch boundary: a stale in-flight message queued
// before the clear is dropped, and the re-sent primer that follows the clear
// survives to become the next epoch's task.
func TestClearBoundaryDeliversPostClearPrimer(t *testing.T) {
	t.Parallel()

	// Frame stream a real session produces on /clear during active work: a
	// stale in-flight user message, the clear, then the re-sent primer.
	var buf bytes.Buffer
	require.NoError(t, frames.Write(&buf, frames.Frame{Type: frames.TypeUserMessage, MessageID: "stale", Content: "stale"}))
	require.NoError(t, frames.Write(&buf, frames.Frame{Type: frames.TypeClear}))
	require.NoError(t, frames.Write(&buf, frames.Frame{Type: frames.TypeUserMessage, MessageID: "m1", Content: "re-sent primer"}))

	inbox := newChatInbox()
	clearCh := make(chan struct{}, 1)

	// Pump processes every frame in order, then closes the inbox on EOF: the
	// clear boundary is set (stale dropped) and the primer is queued after it
	// before epochLoop runs — deterministic, no sleep.
	inbox.Pump(&buf, clearCh)

	cfg := &harness.Config{History: []llm.Message{{Role: "user", Content: "old"}}}

	epoch := 0
	tasks := make([]string, 0, 2)

	var secondHistory []llm.Message

	run := func(_ context.Context, task string) (bool, error) {
		epoch++

		tasks = append(tasks, task)

		if epoch == 2 {
			secondHistory = cfg.History
		}

		return epoch == 1, nil // epoch 1 cleared; epoch 2 done
	}

	err := epochLoop(context.Background(), clearCh, inbox, cfg, "initial task", run)
	require.NoError(t, err)
	require.Len(t, tasks, 2)
	assert.Equal(t, "initial task", tasks[0])
	assert.Equal(t, "re-sent primer", tasks[1], "post-clear primer must survive; stale dropped")
	assert.Nil(t, secondHistory)
}

// TestEpochLoop_NaturalDone verifies that a run returning done (not cleared)
// exits the loop after a single epoch.
func TestEpochLoop_NaturalDone(t *testing.T) {
	t.Parallel()

	inbox := newChatInbox()
	clearCh := make(chan struct{}, 1)
	cfg := &harness.Config{}

	epoch := 0

	run := func(_ context.Context, _ string) (bool, error) {
		epoch++

		return false, nil // done
	}

	err := epochLoop(context.Background(), clearCh, inbox, cfg, "task", run)
	require.NoError(t, err)
	assert.Equal(t, 1, epoch)
}

// TestEpochLoop_InboxClosedAfterClear verifies that if the inbox is closed
// while waiting for the re-sent primer between epochs, the loop exits cleanly.
func TestEpochLoop_InboxClosedAfterClear(t *testing.T) {
	t.Parallel()

	inbox := newChatInbox()
	inbox.closeInbox() // no next message: session ends between epochs

	clearCh := make(chan struct{}, 1)
	cfg := &harness.Config{}

	epoch := 0

	run := func(_ context.Context, _ string) (bool, error) {
		epoch++

		return true, nil // cleared
	}

	err := epochLoop(context.Background(), clearCh, inbox, cfg, "task", run)
	require.NoError(t, err) // clean exit when inbox closed between epochs
	assert.Equal(t, 1, epoch)
}

// TestReasoningRaw verifies that reasoningRaw returns nil for an empty effort
// and a valid JSON object for standard and non-standard tiers.
func TestReasoningRaw(t *testing.T) {
	t.Parallel()

	assert.Nil(t, reasoningRaw(""))
	assert.JSONEq(t, `{"effort":"medium"}`, string(reasoningRaw("medium")))
	assert.JSONEq(t, `{"effort":"xhigh"}`, string(reasoningRaw("xhigh")))
}

// TestClearDrainsPendingMessage verifies that epochLoop discards messages
// already queued in the inbox before a /clear boundary. Without the boundary
// a stale user_message sitting in the inbox before the clear becomes the next
// epoch's primer, silently losing the human's re-sent task.
func TestClearDrainsPendingMessage(t *testing.T) {
	t.Parallel()

	inbox := newChatInbox()

	// Stale message queued before /clear fires (simulates a message already
	// in-flight when the user hits clear).
	inbox.push(harness.UserMessage{MessageID: "stale", Content: "stale-primer"})
	inbox.onClear()    // clear boundary drops the pre-clear stale message
	inbox.closeInbox() // no re-sent primer follows

	clearCh := make(chan struct{}, 1)
	cfg := &harness.Config{}

	epoch := 0

	run := func(_ context.Context, _ string) (bool, error) {
		epoch++

		return true, nil // cleared
	}

	err := epochLoop(context.Background(), clearCh, inbox, cfg, "initial", run)
	require.NoError(t, err)

	// Before fix: the stale message survived the clear and was returned by
	// Wait → epoch 2 ran → epoch == 2. After fix: onClear() drops the stale
	// message and NextAfterClear sees the closed, empty inbox → loop exits
	// after epoch 1.
	assert.Equal(t, 1, epoch, "stale pre-clear message must be dropped at the clear boundary; no second epoch without a fresh primer")
}

// TestEnvOrSecret verifies the env-first-then-file resolution used for the
// CM-provisioned LLM endpoint values: a per-session container env override
// (set by the launcher when ChatStartPayload.LLMEndpoint is present) wins
// over the value staged in the shared /run/cm-secrets/env file, including
// when the override is explicitly empty (the type's canonical default is a
// real provisioned value, not "absent"). Absent env falls back to the file —
// today's path for a CM that did not provision an llm endpoint.
func TestEnvOrSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	require.NoError(t, os.WriteFile(path,
		[]byte("LLM_API_KEY=file-key-123456\nLLM_BASE_URL=https://file.example/v1\n"), 0o600))

	src, err := secrets.Open(path)
	require.NoError(t, err)

	t.Run("env override wins over file value", func(t *testing.T) {
		t.Setenv("LLM_API_KEY", "payload-key-123456")
		assert.Equal(t, "payload-key-123456", envOrSecret("LLM_API_KEY", src))
	})

	t.Run("env override wins even when explicitly empty", func(t *testing.T) {
		t.Setenv("LLM_BASE_URL", "")
		assert.Empty(t, envOrSecret("LLM_BASE_URL", src))
	})

	t.Run("falls back to file value when env unset", func(t *testing.T) {
		// Hermetic: a host or CI that exports LLM_API_KEY/LLM_BASE_URL would
		// otherwise make envOrSecret return the exported value and fail this
		// subtest. Explicitly unset (restoring any prior value on cleanup) so the
		// fallback-to-file path is what is actually exercised.
		unsetEnv(t, "LLM_API_KEY")
		unsetEnv(t, "LLM_BASE_URL")

		assert.Equal(t, "file-key-123456", envOrSecret("LLM_API_KEY", src))
		assert.Equal(t, "https://file.example/v1", envOrSecret("LLM_BASE_URL", src))
	})
}

// unsetEnv removes key for the duration of the test, restoring any prior value
// on cleanup. Used instead of t.Setenv when a test needs the var genuinely
// ABSENT (os.LookupEnv → ok=false), which t.Setenv cannot express. t.Setenv is
// incompatible with t.Parallel; unsetEnv shares that constraint, so callers
// must not mark the test parallel.
func unsetEnv(t *testing.T, key string) {
	t.Helper()

	prev, had := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))

	t.Cleanup(func() {
		if had {
			require.NoError(t, os.Setenv(key, prev))
		}
	})
}

// TestEpochLoop_RunError verifies that a non-clear error from run propagates.
func TestEpochLoop_RunError(t *testing.T) {
	t.Parallel()

	inbox := newChatInbox()
	clearCh := make(chan struct{}, 1)
	cfg := &harness.Config{}

	want := errors.New("harness error")

	run := func(_ context.Context, _ string) (bool, error) {
		return false, want
	}

	err := epochLoop(context.Background(), clearCh, inbox, cfg, "task", run)
	assert.ErrorIs(t, err, want)
}
