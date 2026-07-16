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

// TestClearSelfSeedsNextEpochPrimer verifies that a /clear during active work
// establishes an epoch boundary: a stale in-flight message queued before the
// clear is dropped, the next epoch's task is the embedded primer (the worker
// re-orients itself — nothing is re-sent from the host), and a user message
// that arrives after the clear survives for the new epoch's inbox.
func TestClearSelfSeedsNextEpochPrimer(t *testing.T) {
	t.Parallel()

	// Frame stream a real session produces on /clear during active work: a
	// stale in-flight user message, the clear, then the user's next message.
	var buf bytes.Buffer
	require.NoError(t, frames.Write(&buf, frames.Frame{Type: frames.TypeUserMessage, MessageID: "stale", Content: "stale"}))
	require.NoError(t, frames.Write(&buf, frames.Frame{Type: frames.TypeClear}))
	require.NoError(t, frames.Write(&buf, frames.Frame{Type: frames.TypeUserMessage, MessageID: "m1", Content: "follow-up"}))

	inbox := newChatInbox()
	clearCh := make(chan struct{}, 1)

	// Pump processes every frame in order: the clear boundary is set (stale
	// dropped) and the follow-up is queued behind the hold before epochLoop
	// runs — deterministic, no sleep.
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
	assert.Equal(t, chatPrimer, tasks[1], "the next epoch re-orients from the embedded primer")
	assert.Nil(t, secondHistory)

	// The post-clear user message was not consumed as the task: it is released
	// to the new epoch's inbox, in order, with the stale pre-clear one dropped.
	msgs := inbox.Drain()
	require.Len(t, msgs, 1)
	assert.Equal(t, "follow-up", msgs[0].Content)
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

// TestEpochLoop_InboxClosedAfterClear verifies that if the inbox is already
// closed at the epoch boundary (host closed stdin), the loop exits cleanly
// instead of starting a fresh primer-seeded epoch on a dead session.
func TestEpochLoop_InboxClosedAfterClear(t *testing.T) {
	t.Parallel()

	inbox := newChatInbox()
	inbox.closeInbox() // session ends between epochs

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
