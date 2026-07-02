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

// TestNextAfterClearSurvivesRacingSecondClear reproduces the double-clear
// release-window hang: onClear() re-arms held, and if that race lands between
// NextAfterClear's release step and its first delivery, the held-gated Wait
// path never unblocks again (nothing else ever clears held). NextAfterClear
// must deliver regardless of a racing held, since it is the sole consumer once
// the cleared epoch's harness.Run has returned. The race is timing-dependent,
// so this loops with a bounded per-iteration timeout: ctx is context.Background
// (no escape hatch), so a genuine hang never returns and any single timeout is
// a failure. A short sleep after spawning the goroutine biases the scheduler so
// it clears its own release step and parks in the blocking wait before the
// second onClear()+push land — without it, the racing goroutine usually hasn't
// even started before the main goroutine finishes both steps, so the window is
// missed almost every time.
func TestNextAfterClearSurvivesRacingSecondClear(t *testing.T) {
	t.Parallel()

	for i := range 300 {
		in := newChatInbox()
		in.onClear() // first clear boundary

		done := make(chan struct{})

		go func() {
			_, _ = in.NextAfterClear(context.Background()) // no ctx escape: a real hang never returns

			close(done)
		}()

		time.Sleep(time.Millisecond) // let the goroutine clear its release step and park

		in.onClear()                                    // second clear races the release window
		in.push(harness.UserMessage{Content: "primer"}) // the post-second-clear primer

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("NextAfterClear hung after a racing second clear (iter %d)", i)
		}
	}
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
