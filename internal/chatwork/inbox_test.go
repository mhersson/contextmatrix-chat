package chatwork

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-chat/internal/frames"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatInbox_Drain(t *testing.T) {
	t.Parallel()

	in := newChatInbox()
	in.push(harness.UserMessage{MessageID: "m1", Content: "hello"})
	in.push(harness.UserMessage{MessageID: "m2", Content: "world"})

	got := in.Drain()
	require.Len(t, got, 2)
	assert.Equal(t, "m1", got[0].MessageID)
	assert.Equal(t, "hello", got[0].Content)
	assert.Equal(t, "m2", got[1].MessageID)

	// Second Drain on empty inbox returns nil.
	assert.Nil(t, in.Drain())
}

func TestChatInbox_Wait_MessageArrives(t *testing.T) {
	t.Parallel()

	in := newChatInbox()
	ctx := context.Background()

	// Push the message from another goroutine after a short delay so Wait blocks.
	go func() {
		time.Sleep(5 * time.Millisecond)
		in.push(harness.UserMessage{MessageID: "async", Content: "hi"})
	}()

	msg, err := in.Wait(ctx)
	require.NoError(t, err)
	assert.Equal(t, "async", msg.MessageID)
	assert.Equal(t, "hi", msg.Content)
}

func TestChatInbox_Wait_ErrInboxClosed(t *testing.T) {
	t.Parallel()

	in := newChatInbox()
	ctx := context.Background()

	go func() {
		time.Sleep(5 * time.Millisecond)
		in.closeInbox()
	}()

	_, err := in.Wait(ctx)
	assert.ErrorIs(t, err, harness.ErrInboxClosed)
}

func TestChatInbox_Wait_Queued_BeforeClosed(t *testing.T) {
	t.Parallel()

	in := newChatInbox()
	ctx := context.Background()

	// Push a message and then close; Wait must deliver the message first.
	in.push(harness.UserMessage{MessageID: "queued", Content: "data"})
	in.closeInbox()

	msg, err := in.Wait(ctx)
	require.NoError(t, err)
	assert.Equal(t, "queued", msg.MessageID)

	// Second Wait sees ErrInboxClosed.
	_, err = in.Wait(ctx)
	assert.ErrorIs(t, err, harness.ErrInboxClosed)
}

func TestChatInbox_Wait_ContextCanceled(t *testing.T) {
	t.Parallel()

	in := newChatInbox()
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	_, err := in.Wait(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestReleaseClearOpensHoldForNextEpoch verifies the epoch-boundary release:
// a message that arrived after the clear stays behind the hold, and once
// releaseClear opens it the next epoch's Wait delivers that message in order.
func TestReleaseClearOpensHoldForNextEpoch(t *testing.T) {
	t.Parallel()

	in := newChatInbox()
	in.push(harness.UserMessage{Content: "stale"})
	in.onClear() // drops the stale message, holds the inbox
	in.push(harness.UserMessage{Content: "post-clear"})

	closed := in.releaseClear()
	assert.False(t, closed, "an open inbox must start the next epoch")

	msg, err := in.Wait(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "post-clear", msg.Content)
}

// TestReleaseClearReportsClosedInbox verifies the epoch loop's exit signal:
// a closed inbox with nothing queued means stdin is gone and no fresh epoch
// should start; a closed inbox that still holds a post-clear message must
// still run the next epoch so the message is delivered before ErrInboxClosed.
func TestReleaseClearReportsClosedInbox(t *testing.T) {
	t.Parallel()

	t.Run("closed and empty exits", func(t *testing.T) {
		t.Parallel()

		in := newChatInbox()
		in.onClear()
		in.closeInbox()

		assert.True(t, in.releaseClear())
	})

	t.Run("closed with a queued message still delivers", func(t *testing.T) {
		t.Parallel()

		in := newChatInbox()
		in.onClear()
		in.push(harness.UserMessage{Content: "last words"})
		in.closeInbox()

		assert.False(t, in.releaseClear(), "the queued message must reach the next epoch")

		msg, err := in.Wait(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "last words", msg.Content)
	})
}

func TestChatInbox_Pump_ClearSignal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, frames.Write(&buf, frames.Frame{Type: frames.TypeClear}))

	in := newChatInbox()
	clearCh := make(chan struct{}, 1)

	done := make(chan struct{})

	go func() {
		defer close(done)

		in.Pump(&buf, clearCh)
	}()

	// Wait for Pump to finish (it exits on EOF after the single frame).
	<-done

	// The clear frame must have produced a signal in the buffered channel.
	select {
	case <-clearCh:
		// received ✓
	default:
		t.Fatal("no clear signal in clearCh after Pump processed clear frame")
	}

	// Pump closes the inbox on EOF.
	_, err := in.Wait(context.Background())
	assert.ErrorIs(t, err, harness.ErrInboxClosed)
}
