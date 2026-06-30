package chatwork

import (
	"context"
	"errors"
	"testing"

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
	inbox.push(harness.UserMessage{MessageID: "m1", Content: "re-sent primer"})

	clearCh := make(chan struct{}, 1)
	cfg := &harness.Config{
		History: []llm.Message{{Role: "user", Content: "old"}},
	}

	epoch := 0
	tasks := make([]string, 0, 2)

	var secondHistory []llm.Message

	run := func(_ context.Context, task string) (bool, error) {
		epoch++

		tasks = append(tasks, task)

		if epoch == 2 {
			secondHistory = cfg.History
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
