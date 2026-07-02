package chatwork

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/mhersson/contextmatrix-chat/internal/frames"
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
