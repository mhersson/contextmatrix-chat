package chatwork

import (
	"context"
	"io"
	"log/slog"
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
	signal  chan struct{}
}

func newChatInbox() *chatInbox {
	return &chatInbox{
		signal: make(chan struct{}, 1),
	}
}

// Pump reads frames from r until EOF or error, routing user_message frames to
// the inbox and closing it on EOF. clear frames are logged; epoch reset is
// deferred to task 3.4b. Run Pump in a goroutine; it exits when the reader
// reaches EOF or returns a non-EOF error.
func (in *chatInbox) Pump(r io.Reader) {
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
			slog.Info("clear frame received; epoch reset deferred to task 3.4b")
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

		if len(in.pending) > 0 {
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
