package chatwork

import (
	"context"
	"errors"
	"testing"
	"time"

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

// TestEpochLoop_ClearedOnceThenDone verifies that a /clear triggers a second
// epoch: two epochs run, the second task comes from the inbox, and History is
// nil in the second epoch.
func TestEpochLoop_ClearedOnceThenDone(t *testing.T) {
	t.Parallel()

	inbox := newChatInbox()

	clearCh := make(chan struct{}, 1)
	cfg := &harness.Config{
		History: []llm.Message{{Role: "user", Content: "old"}},
	}

	epoch := 0
	tasks := make([]string, 0, 2)

	var secondHistory []llm.Message

	// Push the re-sent primer from a goroutine AFTER epoch 1 finishes. The
	// brief sleep gives epochLoop time to call Drain() (which runs right after
	// run() returns) before the push arrives, so the primer is not itself
	// drained. Without the drain fix, a pre-queued primer would be lost to
	// Drain(); pushing it here after the fact avoids that ordering problem.
	epoch1Done := make(chan struct{})

	go func() {
		<-epoch1Done
		time.Sleep(time.Millisecond)
		inbox.push(harness.UserMessage{MessageID: "m1", Content: "re-sent primer"})
	}()

	run := func(_ context.Context, task string) (bool, error) {
		epoch++

		tasks = append(tasks, task)

		if epoch == 2 {
			secondHistory = cfg.History
		}

		if epoch == 1 {
			close(epoch1Done)
		}

		return epoch == 1, nil // epoch 1: cleared; epoch 2: done
	}

	err := epochLoop(context.Background(), clearCh, inbox, cfg, "initial task", run)
	require.NoError(t, err)
	assert.Equal(t, 2, epoch)
	require.Len(t, tasks, 2)
	assert.Equal(t, "initial task", tasks[0])
	assert.Equal(t, "re-sent primer", tasks[1])
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
// already queued in the inbox when a /clear fires. Without the drain a stale
// user_message sitting in the inbox before the clear becomes the next epoch's
// primer, silently losing the human's re-sent task.
func TestClearDrainsPendingMessage(t *testing.T) {
	t.Parallel()

	inbox := newChatInbox()

	// Stale message queued before /clear fires (simulates a message already
	// in-flight when the user hits clear).
	inbox.push(harness.UserMessage{MessageID: "stale", Content: "stale-primer"})

	clearCh := make(chan struct{}, 1)
	cfg := &harness.Config{}

	epoch := 0

	run := func(_ context.Context, _ string) (bool, error) {
		epoch++

		if epoch == 1 {
			// Simulate no re-sent primer: close the inbox so Wait returns
			// ErrInboxClosed after the stale message is drained. Before the
			// fix, Wait returns the stale message and epoch 2 runs.
			inbox.closeInbox()

			return true, nil // cleared
		}

		return false, nil
	}

	err := epochLoop(context.Background(), clearCh, inbox, cfg, "initial", run)
	require.NoError(t, err)

	// Before fix: stale message was returned by Wait → epoch 2 ran → epoch == 2.
	// After fix:  Drain() drops the stale message → Wait returns ErrInboxClosed
	//             → loop exits after epoch 1.
	assert.Equal(t, 1, epoch, "stale pre-clear message must be drained; no second epoch without a fresh primer")
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
