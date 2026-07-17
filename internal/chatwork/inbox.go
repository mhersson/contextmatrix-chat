package chatwork

import (
	"context"
	"io"
	"sync"

	"github.com/mhersson/contextmatrix-chat/internal/frames"
	"github.com/mhersson/contextmatrix-harness/harness"
)

// chatInbox is a channel-backed harness.Inbox for interactive chat sessions.
// Messages arrive from a Pump goroutine reading stdin frames; close is signaled
// on EOF so harness.Run exits cleanly when the host closes the pipe.
type chatInbox struct {
	mu      sync.Mutex
	pending []harness.UserMessage
	closed  bool
	held    bool // set by onClear; hides pending from the dying epoch's consumers
	signal  chan struct{}
}

func newChatInbox() *chatInbox {
	return &chatInbox{
		signal: make(chan struct{}, 1),
	}
}

// Pump reads frames from r until EOF or error, routing user_message frames to
// the inbox, signaling clearCh (non-blocking) on clear frames, and closing the
// inbox on EOF. Run Pump in a goroutine; it exits when the reader reaches EOF
// or returns a non-EOF error.
func (in *chatInbox) Pump(r io.Reader, clearCh chan<- struct{}) {
	fr := frames.NewReader(r)

	for {
		f, err := fr.Next()
		if err != nil {
			// io.EOF or a scanner error: session over either way.
			in.closeInbox()

			return
		}

		switch f.Type {
		case frames.TypeUserMessage:
			in.push(harness.UserMessage{MessageID: f.MessageID, Content: f.Content})
		case frames.TypeClear:
			// Drop everything queued before the clear and hold the inbox so
			// the dying epoch's harness cannot swallow a user message that
			// arrives after the clear - it belongs to the next epoch.
			in.onClear()

			select {
			case clearCh <- struct{}{}:
			default:
			}
		}
	}
}

func (in *chatInbox) push(msg harness.UserMessage) {
	in.mu.Lock()
	in.pending = append(in.pending, msg)
	in.mu.Unlock()
	in.ping()
}

func (in *chatInbox) closeInbox() {
	in.mu.Lock()
	in.closed = true
	in.mu.Unlock()
	in.ping()
}

func (in *chatInbox) ping() {
	select {
	case in.signal <- struct{}{}:
	default:
	}
}

// Drain returns all queued messages in order and empties the queue. Never blocks.
func (in *chatInbox) Drain() []harness.UserMessage {
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.held {
		return nil
	}

	out := in.pending
	in.pending = nil

	return out
}

// Wait blocks until a message is available, the inbox is closed
// (ErrInboxClosed), or ctx is done (ctx.Err()). Queued messages are always
// delivered before ErrInboxClosed.
func (in *chatInbox) Wait(ctx context.Context) (harness.UserMessage, error) {
	for {
		in.mu.Lock()

		if !in.held && len(in.pending) > 0 {
			msg := in.pending[0]
			in.pending = in.pending[1:]
			in.mu.Unlock()

			return msg, nil
		}

		closed := in.closed
		in.mu.Unlock()

		if closed {
			return harness.UserMessage{}, harness.ErrInboxClosed
		}

		select {
		case <-ctx.Done():
			return harness.UserMessage{}, ctx.Err()
		case <-in.signal:
		}
	}
}

// onClear marks the clear boundary: drop all pre-clear messages and hold the
// inbox so the current epoch's consumers see no input until releaseClear
// opens it for the next epoch.
func (in *chatInbox) onClear() {
	in.mu.Lock()
	in.pending = nil
	in.held = true
	in.mu.Unlock()
}

// releaseClear opens the clear-boundary hold and reports whether the inbox is
// already closed. Called by epochLoop only after the cleared epoch has fully
// unwound (harness.Run returned), just before it seeds the next epoch with
// the embedded primer: any message that arrived after the clear stays queued,
// in order, for the new epoch's held-gated Wait/Drain. The closed report lets
// the loop skip starting a fresh epoch on a session whose stdin is gone.
func (in *chatInbox) releaseClear() (closed bool) {
	in.mu.Lock()
	in.held = false
	closed = in.closed && len(in.pending) == 0
	in.mu.Unlock()

	in.ping()

	return closed
}
