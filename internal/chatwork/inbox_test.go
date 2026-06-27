package chatwork

import (
	"context"
	"testing"
	"time"

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
