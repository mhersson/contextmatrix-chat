package chatwork

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
)

// chatSession is the part of *claudecode.Session the loop drives.
type chatSession interface {
	Send(ctx context.Context, text string, images []llm.ImageURL) (harness.Result, error)
	Close() error
	ID() string
}

// startSessionFunc starts a claude process: resume "" is a new conversation,
// otherwise the Claude Code session ID to continue in the same Home.
type startSessionFunc func(ctx context.Context, resume string) (chatSession, error)

// errCleared is the cancel cause of a turn context ended by a /clear frame.
var errCleared = errors.New("chat cleared")

// resumeOpening puts the restored transcript above the primer, whose opening
// turn tells a resumed session to call chat_rehydration_complete.
const resumeOpening = `This chat session restarted. The conversation it had before the restart is restored below as a transcript. It is history, not a new request.

<prior_conversation>
%s
</prior_conversation>

That transcript is the prior conversation the orientation below refers to; treat it as already in your history.

%s`

// sessionLoop runs a chat on one long-lived Claude Code session with the
// inbox and /clear semantics of epochLoop. Only run's goroutine touches sess
// and keptID.
type sessionLoop struct {
	start   startSessionFunc
	inbox   *chatInbox
	clearCh <-chan struct{}
	emit    *events.Emitter
	redact  func(string) string
	primer  string // rendered primer: the first message after a /clear or a fresh restart

	sess   chatSession // nil while no claude process is live
	keptID string      // the session the next message resumes; "" after a /clear
}

// run starts the session, sends opening, then answers each user turn until
// stdin closes or ctx ends. Only a failed first start and a rejected token are
// errors.
func (l *sessionLoop) run(ctx context.Context, opening string) error {
	defer l.closeSession()

	if err := l.startSession(ctx, ""); err != nil {
		if ctx.Err() != nil {
			return nil
		}

		return err
	}

	texts := []string{opening}

	for {
		cleared, err := l.sendAll(ctx, texts)
		if err != nil {
			return err
		}

		var done bool

		if !cleared {
			texts, cleared, done = l.await(ctx)
		}

		if cleared {
			texts, done = l.restart(ctx)
		}

		if done {
			return nil
		}
	}
}

// sendAll sends texts in order as separate turns and stops at a /clear or at
// a failed turn, which leaves the session down and drops the rest.
func (l *sessionLoop) sendAll(ctx context.Context, texts []string) (cleared bool, err error) {
	for _, text := range texts {
		if l.sess == nil || ctx.Err() != nil {
			return false, nil
		}

		cleared, err = l.send(ctx, text)
		if cleared || err != nil {
			return cleared, err
		}
	}

	return false, nil
}

// send runs one turn under a clear watch. A cleared or shut-down turn is not a
// failure.
func (l *sessionLoop) send(ctx context.Context, text string) (cleared bool, err error) {
	turnCtx, stop := watchClear(ctx, l.clearCh)
	res, sendErr := l.sess.Send(turnCtx, text, nil)
	cleared = stop()

	slog.Info("chat turn finished", "reason", res.Reason, "turns", res.Turns)

	if cleared || ctx.Err() != nil || sendErr == nil {
		return cleared, nil
	}

	return false, l.turnFailed(sendErr)
}

// turnFailed closes the session after a failed turn and keeps its ID, so the
// next message resumes the conversation. Only a rejected token is fatal.
func (l *sessionLoop) turnFailed(err error) error {
	var limit *claudecode.UsageLimitError

	switch {
	case errors.As(err, &limit):
		// vendorkit already emitted usage_limit; a retry cannot pass before the reset.
		l.closeSession()
	case errors.Is(err, claudecode.ErrNotLoggedIn):
		l.emitError(err)
		l.closeSession()

		return fmt.Errorf("claude code turn: %w", err)
	default:
		l.emitError(err)
		l.closeSession()
	}

	l.emitAwaitingHuman()

	return nil
}

// await waits for the next user turn and, with no live session, reopens one,
// all under one clear watch: a /clear reported by it drops the turn, which
// preceded the clear. done reports stdin closed or ctx ended.
func (l *sessionLoop) await(ctx context.Context) (texts []string, cleared, done bool) {
	if ctx.Err() != nil {
		return nil, false, true
	}

	turnCtx, stop := watchClear(ctx, l.clearCh)

	msg, err := l.inbox.Wait(turnCtx)
	if err == nil {
		if text := joinTurn(append([]harness.UserMessage{msg}, l.inbox.Drain()...)); text != "" {
			texts = l.reopen(ctx, text)
		}
	}

	if stop() {
		return nil, true, false
	}

	// Wait ends without a message only on a closed inbox or an ended ctx: the
	// turn context is otherwise cancelled only by a clear, reported above.
	if err != nil || ctx.Err() != nil {
		return nil, false, true
	}

	return texts, false, false
}

// reopen returns the turns to send for text, starting a session when none is
// live: the kept one is resumed, else a fresh one opens with the primer.
func (l *sessionLoop) reopen(ctx context.Context, text string) []string {
	if l.sess != nil {
		return []string{text}
	}

	if l.keptID != "" && l.startSession(ctx, l.keptID) == nil {
		return []string{text}
	}

	if l.startSession(ctx, "") == nil {
		return []string{l.primer, text}
	}

	if ctx.Err() == nil {
		l.emitAwaitingHuman()
	}

	return nil
}

// restart handles a /clear: the conversation ends and a fresh session opens
// with the primer. A failed start stays down until the next message.
func (l *sessionLoop) restart(ctx context.Context) (texts []string, done bool) {
	if ctx.Err() != nil {
		return nil, true
	}

	l.closeSession()
	l.keptID = ""

	if closed := l.inbox.releaseClear(); closed {
		return nil, true
	}

	// drain any stale clear signal that arrived while handling this one
	select {
	case <-l.clearCh:
	default:
	}

	if err := l.startSession(ctx, ""); err != nil {
		return nil, ctx.Err() != nil
	}

	return []string{l.primer}, false
}

// startSession starts a claude process and keeps its ID for a later resume.
// A failure is emitted as an error event unless ctx ended.
func (l *sessionLoop) startSession(ctx context.Context, resume string) error {
	sess, err := l.start(ctx, resume)
	if err != nil {
		err = fmt.Errorf("start claude code session: %w", err)
		if ctx.Err() == nil {
			l.emitError(err)
		}

		return err
	}

	l.sess = sess
	l.keptID = sess.ID()

	return nil
}

// closeSession closes the live session, if any, exactly once.
func (l *sessionLoop) closeSession() {
	if l.sess == nil {
		return
	}

	if err := l.sess.Close(); err != nil {
		slog.Warn("close claude code session", "error", l.redact(err.Error()))
	}

	l.sess = nil
}

func (l *sessionLoop) emitError(err error) {
	l.emit.Emit(events.ErrorKind, map[string]any{"error": l.redact(err.Error())})
}

// emitAwaitingHuman returns CM's chat status to idle after a turn that left
// the session down.
func (l *sessionLoop) emitAwaitingHuman() {
	l.emit.Emit(events.StateChange, map[string]any{"state": "awaiting_human"})
}

// openingMessage is the first message of a session: the primer, or on resume
// the restored transcript above it.
func openingMessage(primer string, history []llm.Message) string {
	if len(history) == 0 {
		return primer
	}

	// SeedHistory folds tool calls and results into system messages, which
	// RenderTranscript skips; relabel them so the tool summaries survive.
	msgs := make([]llm.Message, len(history))
	for i, m := range history {
		if m.Role == "system" {
			m.Role = "assistant"
		}

		msgs[i] = m
	}

	return fmt.Sprintf(resumeOpening, claudecode.RenderTranscript(msgs), primer)
}

// joinTurn joins the non-empty messages of one turn in arrival order.
func joinTurn(msgs []harness.UserMessage) string {
	parts := make([]string, 0, len(msgs))

	for _, m := range msgs {
		if m.Content != "" {
			parts = append(parts, m.Content)
		}
	}

	return strings.Join(parts, "\n\n")
}

// watchClear returns a context that a /clear signal on clearCh cancels with
// errCleared. stop ends the watch and reports whether a clear arrived; a
// signal sent after stop returns stays in clearCh for the next watch.
func watchClear(ctx context.Context, clearCh <-chan struct{}) (turnCtx context.Context, stop func() (cleared bool)) {
	turnCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	exited := make(chan struct{})

	var seen bool // written by the goroutine before exited closes

	go func() {
		defer close(exited)

		select {
		case <-clearCh:
			seen = true

			cancel(errCleared)
		case <-done:
		}
	}()

	stop = func() bool {
		close(done)
		<-exited
		cancel(nil)

		if seen {
			return true
		}

		// A signal already queued when stop runs belongs to this watch, so a
		// turn taken before that clear is dropped rather than sent.
		select {
		case <-clearCh:
			return true
		default:
			return false
		}
	}

	return turnCtx, stop
}
