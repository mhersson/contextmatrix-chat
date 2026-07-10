package chatwork

import (
	"bytes"
	"context"
	"errors"
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

// TestConfigureGitAuth verifies Run's git-auth setup: a CM-provisioned token
// stages the credentials config the git-credential/gh-wrapper subcommands
// read; an absent token degrades to a git-less session (warn, not fail —
// unlike inference, a git-less chat is still usable).
func TestConfigureGitAuth(t *testing.T) {
	t.Run("degrades to git-less session without a token", func(t *testing.T) {
		t.Setenv("TMPDIR", t.TempDir())

		selfPath, err := configureGitAuth(context.Background(), "")
		require.NoError(t, err)
		assert.Empty(t, selfPath, "no gh wrapper without git credentials")
	})

	t.Run("stages the credentials config with a token", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("TMPDIR", dir)
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
		t.Setenv("HOME", dir)
		t.Setenv("CM_GIT_CREDENTIALS_URL", "http://cm:8080/api/worker/git-credentials")

		selfPath, err := configureGitAuth(context.Background(), "sess-abc.bearer")
		require.NoError(t, err)
		assert.NotEmpty(t, selfPath, "the gh wrapper needs the resolved self path")

		src, err := secrets.Open(gitCredentialsConfigPath())
		require.NoError(t, err)
		assert.Equal(t, "sess-abc.bearer", src.Get("CM_GIT_CREDENTIALS_TOKEN"))
		assert.Equal(t, "http://cm:8080/api/worker/git-credentials", src.Get("CM_GIT_CREDENTIALS_URL"))
	})
}

// TestValidateLLMKey verifies Run's worker-side backstop for handleChatStart's
// fail-closed launch guard: a non-empty key passes, an empty key (the guard
// having been bypassed) fails with a legible, self-explanatory error.
func TestValidateLLMKey(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateLLMKey("sk-something"))

	err := validateLLMKey("")
	require.Error(t, err)
	assert.Equal(t, "no llm api key available: CM did not provision an llm endpoint", err.Error())
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
