package chatwork

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/frames"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testOpening = "OPENING"
	testPrimer  = "PRIMER"
)

// fakeSession records what the loop sends and how often it closes the session.
// reply answers turn i; nil means every turn succeeds.
type fakeSession struct {
	id    string
	reply func(ctx context.Context, i int, text string) (harness.Result, error)

	mu     sync.Mutex
	sends  []string
	closes int
}

func (s *fakeSession) Send(ctx context.Context, text string, _ []llm.ImageURL) (harness.Result, error) {
	s.mu.Lock()
	i := len(s.sends)
	s.sends = append(s.sends, text)
	s.mu.Unlock()

	if s.reply == nil {
		return harness.Result{Reason: "done", Completed: true}, nil
	}

	return s.reply(ctx, i, text)
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()

	return nil
}

func (s *fakeSession) ID() string { return s.id }

func (s *fakeSession) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.sends...)
}

func (s *fakeSession) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closes
}

// startStep is one scripted start: a session or an error. hook runs inside
// start, before it returns.
type startStep struct {
	sess *fakeSession
	err  error
	hook func()
}

type fakeStarter struct {
	mu      sync.Mutex
	steps   []startStep
	resumes []string
	started []*fakeSession
}

func (f *fakeStarter) start(_ context.Context, resume string) (chatSession, error) {
	f.mu.Lock()
	i := len(f.resumes)
	f.resumes = append(f.resumes, resume)
	f.mu.Unlock()

	if i >= len(f.steps) {
		return nil, fmt.Errorf("unexpected start %d (resume %q)", i, resume)
	}

	step := f.steps[i]
	if step.hook != nil {
		step.hook()
	}

	if step.err != nil {
		return nil, step.err
	}

	f.mu.Lock()
	f.started = append(f.started, step.sess)
	f.mu.Unlock()

	return step.sess, nil
}

func (f *fakeStarter) starts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.resumes...)
}

// assertClosedOnce checks that the loop closed every session it started once.
func assertClosedOnce(t *testing.T, f *fakeStarter) {
	t.Helper()

	f.mu.Lock()
	started := append([]*fakeSession(nil), f.started...)
	f.mu.Unlock()

	for _, s := range started {
		assert.Equal(t, 1, s.closeCount(), "session %s closes", s.id)
	}
}

func blockUntilCanceled(ctx context.Context) (harness.Result, error) {
	<-ctx.Done()

	return harness.Result{Reason: "canceled"}, context.Cause(ctx)
}

// signalClear does what Pump does for a clear frame.
func signalClear(in *chatInbox, clearCh chan<- struct{}) {
	in.onClear()

	select {
	case clearCh <- struct{}{}:
	default:
	}
}

func pumpFrames(t *testing.T, in *chatInbox, clearCh chan<- struct{}, fs ...frames.Frame) {
	t.Helper()

	var buf bytes.Buffer
	for _, f := range fs {
		require.NoError(t, frames.Write(&buf, f))
	}

	in.Pump(&buf, clearCh)
}

func userFrame(content string) frames.Frame {
	return frames.Frame{Type: frames.TypeUserMessage, MessageID: content, Content: content}
}

func pushUser(in *chatInbox, content string) {
	in.push(harness.UserMessage{MessageID: content, Content: content})
}

type loopFixture struct {
	starter *fakeStarter
	inbox   *chatInbox
	clearCh chan struct{}
	events  *bytes.Buffer
	loop    *sessionLoop
}

func newLoopFixture(redact func(string) string, steps ...startStep) *loopFixture {
	if redact == nil {
		redact = func(s string) string { return s }
	}

	f := &loopFixture{
		starter: &fakeStarter{steps: steps},
		inbox:   newChatInbox(),
		clearCh: make(chan struct{}, 1),
		events:  &bytes.Buffer{},
	}

	f.loop = &sessionLoop{
		start:   f.starter.start,
		inbox:   f.inbox,
		clearCh: f.clearCh,
		emit:    events.NewEmitter(io.Discard, f.events),
		redact:  redact,
		primer:  testPrimer,
	}

	return f
}

func (f *loopFixture) run(ctx context.Context, t *testing.T) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	return f.loop.run(ctx, testOpening)
}

// decoded returns the events the loop emitted; read it only after run returns.
func (f *loopFixture) decoded(t *testing.T) []events.Event {
	t.Helper()

	var evs []events.Event

	sc := bufio.NewScanner(bytes.NewReader(f.events.Bytes()))
	for sc.Scan() {
		var ev events.Event

		require.NoError(t, json.Unmarshal(sc.Bytes(), &ev))

		evs = append(evs, ev)
	}

	return evs
}

func errorEvents(evs []events.Event) []events.Event {
	var out []events.Event

	for _, ev := range evs {
		if ev.Kind == events.ErrorKind {
			out = append(out, ev)
		}
	}

	return out
}

func awaitingHumanCount(evs []events.Event) int {
	n := 0

	for _, ev := range evs {
		if ev.Kind == events.StateChange && ev.Data["state"] == "awaiting_human" {
			n++
		}
	}

	return n
}

func TestJoinTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msgs []harness.UserMessage
		want string
	}{
		{name: "skips empty", msgs: []harness.UserMessage{{Content: "a"}, {Content: ""}, {Content: "b"}}, want: "a\n\nb"},
		{name: "all empty", msgs: []harness.UserMessage{{Content: ""}, {Content: ""}}, want: ""},
		{name: "none", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, joinTurn(tt.msgs))
		})
	}
}

func TestOpeningMessage(t *testing.T) {
	t.Parallel()

	primer := renderPrimer("s-1")

	t.Run("no history is the primer", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, primer, openingMessage(primer, nil))
	})

	t.Run("transcript above the primer", func(t *testing.T) {
		t.Parallel()

		history := []llm.Message{{Role: "user", Content: "find ALPHA-1"}, {Role: "assistant", Content: "ALPHA-1 is in review"}}
		transcript := claudecode.RenderTranscript(history)

		got := openingMessage(primer, history)
		require.Contains(t, got, transcript)
		assert.True(t, strings.HasSuffix(got, primer))
		assert.Less(t, strings.Index(got, transcript), strings.LastIndex(got, primer))
	})

	t.Run("tool summaries survive", func(t *testing.T) {
		t.Parallel()

		history := []llm.Message{{Role: "system", Content: "called list_cards"}, {Role: "system", Content: "list_cards returned 3 cards"}}

		got := openingMessage(primer, history)
		assert.Contains(t, got, "called list_cards")
		assert.Contains(t, got, "list_cards returned 3 cards")
	})
}

func TestWatchClear(t *testing.T) {
	t.Parallel()

	t.Run("a clear cancels the turn and is reported", func(t *testing.T) {
		t.Parallel()

		clearCh := make(chan struct{}, 1)
		turnCtx, stop := watchClear(t.Context(), clearCh)

		clearCh <- struct{}{}

		<-turnCtx.Done()
		require.ErrorIs(t, context.Cause(turnCtx), errCleared)
		assert.True(t, stop())
	})

	t.Run("no clear is not reported", func(t *testing.T) {
		t.Parallel()

		turnCtx, stop := watchClear(t.Context(), make(chan struct{}, 1))

		assert.False(t, stop())
		assert.NotErrorIs(t, context.Cause(turnCtx), errCleared)
	})

	t.Run("a clear pending at stop is reported", func(t *testing.T) {
		t.Parallel()

		for range 100 {
			clearCh := make(chan struct{}, 1)
			_, stop := watchClear(t.Context(), clearCh)

			clearCh <- struct{}{}

			require.True(t, stop())
			require.Empty(t, clearCh)
		}
	})

	t.Run("a clear after stop stays for the next watch", func(t *testing.T) {
		t.Parallel()

		for range 100 {
			clearCh := make(chan struct{}, 1)
			_, stop := watchClear(t.Context(), clearCh)

			require.False(t, stop())

			clearCh <- struct{}{}

			runtime.Gosched()
			require.Len(t, clearCh, 1)
		}
	})
}

func TestSessionLoopOpeningThenJoinedMessages(t *testing.T) {
	t.Parallel()

	s1 := &fakeSession{id: "id-1"}
	f := newLoopFixture(nil, startStep{sess: s1})

	pumpFrames(t, f.inbox, f.clearCh, userFrame("a"), userFrame("b"))

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{""}, f.starter.starts())
	assert.Equal(t, []string{testOpening, "a\n\nb"}, s1.sent())
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopClearRestartsWithPrimer(t *testing.T) {
	t.Parallel()

	s1 := &fakeSession{id: "id-1", reply: func(ctx context.Context, _ int, _ string) (harness.Result, error) {
		return blockUntilCanceled(ctx)
	}}
	s2 := &fakeSession{id: "id-2"}
	f := newLoopFixture(nil, startStep{sess: s1}, startStep{sess: s2})

	pumpFrames(t, f.inbox, f.clearCh, userFrame("stale"), frames.Frame{Type: frames.TypeClear}, userFrame("after"))

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", ""}, f.starter.starts())
	assert.Equal(t, []string{testOpening}, s1.sent())
	assert.Equal(t, []string{testPrimer, "after"}, s2.sent())
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopClearWhileIdle(t *testing.T) {
	t.Parallel()

	opened := make(chan struct{})
	s1 := &fakeSession{id: "id-1", reply: func(context.Context, int, string) (harness.Result, error) {
		close(opened)

		return harness.Result{Reason: "done"}, nil
	}}
	s2 := &fakeSession{id: "id-2"}
	f := newLoopFixture(nil, startStep{sess: s1}, startStep{sess: s2})

	go func() {
		<-opened
		signalClear(f.inbox, f.clearCh)
		pushUser(f.inbox, "next")
		f.inbox.closeInbox()
	}()

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", ""}, f.starter.starts())
	assert.Equal(t, []string{testOpening}, s1.sent())
	assert.Equal(t, []string{testPrimer, "next"}, s2.sent())
	assertClosedOnce(t, f.starter)
}

// A clear that lands after Wait took a message but before the watch stops
// drops that message: it preceded the clear.
func TestSessionLoopClearAfterWaitDropsTurn(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		if i == 1 {
			pushUser(f.inbox, "q2")

			return harness.Result{Reason: "error"}, errors.New("claude exited")
		}

		return harness.Result{Reason: "done"}, nil
	}}
	s2 := &fakeSession{id: "id-1"}
	s3 := &fakeSession{id: "id-3"}

	f = newLoopFixture(nil,
		startStep{sess: s1},
		startStep{sess: s2, hook: func() {
			signalClear(f.inbox, f.clearCh)
			pushUser(f.inbox, "after")
			f.inbox.closeInbox()
		}},
		startStep{sess: s3},
	)

	pushUser(f.inbox, "q1")

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1", ""}, f.starter.starts())
	assert.Equal(t, []string{testOpening, "q1"}, s1.sent())
	assert.Empty(t, s2.sent())
	assert.Equal(t, []string{testPrimer, "after"}, s3.sent())
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopClearAfterEOFStartsNothing(t *testing.T) {
	t.Parallel()

	s1 := &fakeSession{id: "id-1", reply: func(ctx context.Context, _ int, _ string) (harness.Result, error) {
		return blockUntilCanceled(ctx)
	}}
	f := newLoopFixture(nil, startStep{sess: s1})

	pumpFrames(t, f.inbox, f.clearCh, frames.Frame{Type: frames.TypeClear})

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{""}, f.starter.starts())
	assert.Equal(t, []string{testOpening}, s1.sent())
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopUsageLimitResumesOnNextMessage(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		if i == 1 {
			pushUser(f.inbox, "q2")
			f.inbox.closeInbox()

			return harness.Result{Reason: "error"}, &claudecode.UsageLimitError{Window: "five_hour"}
		}

		return harness.Result{Reason: "done"}, nil
	}}
	s2 := &fakeSession{id: "id-1"}

	f = newLoopFixture(nil, startStep{sess: s1}, startStep{sess: s2})
	pushUser(f.inbox, "q1")

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1"}, f.starter.starts())
	assert.Equal(t, []string{"q2"}, s2.sent())

	evs := f.decoded(t)
	assert.Empty(t, errorEvents(evs))
	assert.Equal(t, 1, awaitingHumanCount(evs))
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopCrashEmitsRedactedErrorAndResumes(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		if i == 1 {
			pushUser(f.inbox, "q2")
			f.inbox.closeInbox()

			return harness.Result{Reason: "error"}, errors.New("claude exited: secret-000000")
		}

		return harness.Result{Reason: "done"}, nil
	}}
	s2 := &fakeSession{id: "id-1"}

	redact := func(s string) string { return strings.ReplaceAll(s, "secret-000000", "[REDACTED]") }
	f = newLoopFixture(redact, startStep{sess: s1}, startStep{sess: s2})
	pushUser(f.inbox, "q1")

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1"}, f.starter.starts())
	assert.Equal(t, []string{"q2"}, s2.sent())

	evs := f.decoded(t)
	errs := errorEvents(evs)
	require.Len(t, errs, 1)

	msg, ok := errs[0].Data["error"].(string)
	require.True(t, ok)
	assert.NotEmpty(t, msg)
	assert.NotContains(t, msg, "secret-000000")
	assert.Equal(t, 1, awaitingHumanCount(evs))
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopResumeStartFailureStartsFresh(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		if i == 1 {
			pushUser(f.inbox, "q2")
			f.inbox.closeInbox()

			return harness.Result{Reason: "error"}, errors.New("claude exited")
		}

		return harness.Result{Reason: "done"}, nil
	}}
	s3 := &fakeSession{id: "id-3"}

	f = newLoopFixture(nil,
		startStep{sess: s1},
		startStep{err: errors.New("no transcript for id-1")},
		startStep{sess: s3},
	)
	pushUser(f.inbox, "q1")

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1", ""}, f.starter.starts())
	assert.Equal(t, []string{testPrimer, "q2"}, s3.sent())
	assert.Len(t, errorEvents(f.decoded(t)), 2)
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopNotLoggedInIsFatal(t *testing.T) {
	t.Parallel()

	s1 := &fakeSession{id: "id-1", reply: func(context.Context, int, string) (harness.Result, error) {
		return harness.Result{Reason: "error"}, fmt.Errorf("turn: %w", claudecode.ErrNotLoggedIn)
	}}
	f := newLoopFixture(nil, startStep{sess: s1})

	err := f.run(t.Context(), t)
	require.ErrorIs(t, err, claudecode.ErrNotLoggedIn)
	assert.Len(t, errorEvents(f.decoded(t)), 1)
	assert.Equal(t, []string{""}, f.starter.starts())
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopInitialStartFailureIsFatal(t *testing.T) {
	t.Parallel()

	startErr := errors.New("claude exited before answering the initialize request")
	f := newLoopFixture(nil, startStep{err: startErr})

	err := f.run(t.Context(), t)
	require.ErrorIs(t, err, startErr)
	assert.Len(t, errorEvents(f.decoded(t)), 1)
	assert.Equal(t, []string{""}, f.starter.starts())
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopShutdownClosesSession(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	inTurn := make(chan struct{})
	s1 := &fakeSession{id: "id-1", reply: func(ctx context.Context, _ int, _ string) (harness.Result, error) {
		close(inTurn)

		return blockUntilCanceled(ctx)
	}}
	f := newLoopFixture(nil, startStep{sess: s1})

	go func() {
		<-inTurn
		cancel()
	}()

	require.NoError(t, f.run(ctx, t))
	assert.Equal(t, []string{testOpening}, s1.sent())
	assert.Empty(t, errorEvents(f.decoded(t)))
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopInboxClosedWhileDownStartsNothing(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		if i == 1 {
			f.inbox.closeInbox()

			return harness.Result{Reason: "error"}, &claudecode.UsageLimitError{}
		}

		return harness.Result{Reason: "done"}, nil
	}}

	f = newLoopFixture(nil, startStep{sess: s1})
	pushUser(f.inbox, "q1")

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{""}, f.starter.starts())
	assertClosedOnce(t, f.starter)
}
