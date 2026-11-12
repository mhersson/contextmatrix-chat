package chatwork

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// fakeEvents is what a fakeSession emits for a turn, as vendorkit's mapper does.
type fakeEvents int

const (
	noEvents          fakeEvents = iota
	inputEvents                  // user_input only
	crashEvents                  // user_input, then error for a failed turn (a process exit)
	resultErrorEvents            // user_input, then error and awaiting_human for a failed turn (a result error)
)

// errDeadProcess is a Send on a process that had already exited: vendorkit
// returns it at once and emits nothing.
var errDeadProcess = errors.New("claudecode: session ended (done)")

// fakeSession records what the loop sends and how often it closes the session.
// reply answers turn i; nil means every turn succeeds. reports selects what each
// turn emits through emit, redacted with redact; the starter sets both.
type fakeSession struct {
	id      string
	reply   func(ctx context.Context, i int, text string) (harness.Result, error)
	reports fakeEvents

	emit   *events.Emitter
	redact func(string) string

	mu     sync.Mutex
	sends  []string
	closes int
}

func (s *fakeSession) Send(ctx context.Context, text string, _ []llm.ImageURL) (harness.Result, error) {
	s.mu.Lock()
	i := len(s.sends)
	s.sends = append(s.sends, text)
	s.mu.Unlock()

	res, err := harness.Result{Reason: "done", Completed: true}, error(nil)
	if s.reply != nil {
		res, err = s.reply(ctx, i, text)
	}

	s.report(res, err)

	return res, err
}

func (s *fakeSession) report(res harness.Result, err error) {
	if s.reports == noEvents || errors.Is(err, errDeadProcess) {
		return
	}

	s.emit.Emit(events.UserInput, map[string]any{"content_len": 1})

	var (
		limit  *claudecode.UsageLimitError
		halted *claudecode.MCPServerError
	)

	switch {
	case err == nil, res.Reason == "canceled", s.reports == inputEvents, errors.As(err, &halted):
	case errors.As(err, &limit):
		s.emit.Emit(events.Kind("usage_limit"), map[string]any{"window": limit.Window})
	default:
		s.emit.Emit(events.ErrorKind, map[string]any{"error": s.redact(err.Error())})

		if s.reports == resultErrorEvents {
			s.emit.Emit(events.StateChange, map[string]any{"state": "awaiting_human"})
		}
	}
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
	emit   *events.Emitter
	redact func(string) string

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

	step.sess.emit, step.sess.redact = f.emit, f.redact

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

// turn is the text of one turn that carries parts.
func turn(parts ...string) string {
	return strings.Join(parts, "\n\n")
}

// assertOpeningTurn checks that sent is one turn: opening first, then the user
// texts that waited for it, in order.
func assertOpeningTurn(t *testing.T, sent []string, opening string, user ...string) {
	t.Helper()

	require.Len(t, sent, 1, "one turn carries the opening and the user text")
	assert.True(t, strings.HasPrefix(sent[0], opening), "the opening comes first: %q", sent[0])
	assert.True(t, strings.HasSuffix(sent[0], turn(user...)), "the user text comes after it: %q", sent[0])
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
		inbox:   newChatInbox(),
		clearCh: make(chan struct{}, 1),
		events:  &bytes.Buffer{},
	}

	tap := newEventTap(f.events)
	emit := events.NewEmitter(io.Discard, tap)
	f.starter = &fakeStarter{emit: emit, redact: redact, steps: steps}

	f.loop = &sessionLoop{
		start:   f.starter.start,
		inbox:   f.inbox,
		clearCh: f.clearCh,
		emit:    emit,
		tap:     tap,
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

func TestOpeningTurn(t *testing.T) {
	t.Parallel()

	t.Run("no user text is the opening alone", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, testOpening, openingTurn(testOpening, ""))
	})

	// The opening tells the model to wait for a message, so the folded one is set off from it.
	t.Run("user text follows the opening, set off from it", func(t *testing.T) {
		t.Parallel()

		got := openingTurn(testOpening, "q1")
		require.True(t, strings.HasPrefix(got, testOpening))
		require.True(t, strings.HasSuffix(got, "q1"))

		between := strings.TrimSuffix(strings.TrimPrefix(got, testOpening), "q1")
		assert.NotEmpty(t, strings.TrimSpace(between))
	})
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

// User text already waiting goes in the opening's turn, as the harness drains
// its inbox before its first model call; text sent during that turn goes next.
func TestSessionLoopOpeningTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		queued []string // messages waiting at boot
		asked  []string // messages that arrive during the opening turn
	}{
		{name: "queued messages go with the opening", queued: []string{"q1", "q2"}},
		{name: "nothing queued, the opening goes alone", asked: []string{"q1", "q2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var f *loopFixture

			s1 := &fakeSession{id: "id-1", reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
				if i == 0 {
					for _, q := range tt.asked {
						pushUser(f.inbox, q)
					}

					f.inbox.closeInbox()
				}

				return harness.Result{Reason: "done"}, nil
			}}
			f = newLoopFixture(nil, startStep{sess: s1})

			for _, q := range tt.queued {
				pushUser(f.inbox, q)
			}

			require.NoError(t, f.run(t.Context(), t))
			assert.Equal(t, []string{""}, f.starter.starts())

			if len(tt.queued) > 0 {
				assertOpeningTurn(t, s1.sent(), testOpening, tt.queued...)
			} else {
				assert.Equal(t, []string{testOpening, turn(tt.asked...)}, s1.sent())
			}

			assertClosedOnce(t, f.starter)
		})
	}
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
	assertOpeningTurn(t, s2.sent(), testPrimer, "after")
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopClearWhileIdle(t *testing.T) {
	t.Parallel()

	opened := make(chan struct{})
	pushed := make(chan struct{})
	s1 := &fakeSession{id: "id-1", reply: func(context.Context, int, string) (harness.Result, error) {
		close(opened)

		return harness.Result{Reason: "done"}, nil
	}}
	s2 := &fakeSession{id: "id-2"}
	f := newLoopFixture(nil, startStep{sess: s1}, startStep{sess: s2, hook: func() { <-pushed }})

	go func() {
		<-opened
		signalClear(f.inbox, f.clearCh)
		pushUser(f.inbox, "next")
		f.inbox.closeInbox()
		close(pushed)
	}()

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", ""}, f.starter.starts())
	assert.Equal(t, []string{testOpening}, s1.sent())
	assertOpeningTurn(t, s2.sent(), testPrimer, "next")
	assertClosedOnce(t, f.starter)
}

// A clear that lands after Wait took a message but before the watch stops
// drops that message: it preceded the clear.
func TestSessionLoopClearAfterWaitDropsTurn(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		switch i {
		case 0:
			pushUser(f.inbox, "q1")
		case 1:
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

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1", ""}, f.starter.starts())
	assert.Equal(t, []string{testOpening, "q1"}, s1.sent())
	assert.Empty(t, s2.sent())
	assertOpeningTurn(t, s3.sent(), testPrimer, "after")
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopClearAfterEOFStartsNothing(t *testing.T) {
	t.Parallel()

	s1 := &fakeSession{id: "id-1", reply: func(ctx context.Context, _ int, _ string) (harness.Result, error) {
		return blockUntilCanceled(ctx)
	}}
	f := newLoopFixture(nil, startStep{sess: s1})

	pumpFrames(t, f.inbox, f.clearCh, userFrame("stale"), frames.Frame{Type: frames.TypeClear})

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{""}, f.starter.starts())
	assert.Equal(t, []string{testOpening}, s1.sent())
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopUsageLimitResumesOnNextMessage(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		switch i {
		case 0:
			pushUser(f.inbox, "q1")
		case 1:
			pushUser(f.inbox, "q2")
			f.inbox.closeInbox()

			return harness.Result{Reason: "error"}, &claudecode.UsageLimitError{Window: "five_hour"}
		}

		return harness.Result{Reason: "done"}, nil
	}}
	s2 := &fakeSession{id: "id-1"}

	f = newLoopFixture(nil, startStep{sess: s1}, startStep{sess: s2})

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1"}, f.starter.starts())
	assert.Equal(t, []string{"q2"}, s2.sent())

	evs := f.decoded(t)
	assert.Empty(t, errorEvents(evs))
	assert.Equal(t, 1, awaitingHumanCount(evs))
	assertClosedOnce(t, f.starter)
}

// One failed turn shows one redacted error line and one awaiting_human,
// whichever of the session and the loop emits them.
func TestSessionLoopCrashEmitsRedactedErrorAndResumes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		reports fakeEvents
	}{
		{name: "process exit reported by the session", reports: crashEvents},
		{name: "result error reported by the session", reports: resultErrorEvents},
		{name: "error the session did not report", reports: inputEvents},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var f *loopFixture

			s1 := &fakeSession{id: "id-1", reports: tt.reports, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
				switch i {
				case 0:
					pushUser(f.inbox, "q1")
				case 1:
					pushUser(f.inbox, "q2")
					f.inbox.closeInbox()

					return harness.Result{Reason: "error"}, errors.New("claude exited: secret-000000")
				}

				return harness.Result{Reason: "done"}, nil
			}}
			s2 := &fakeSession{id: "id-1"}

			redact := func(s string) string { return strings.ReplaceAll(s, "secret-000000", "[REDACTED]") }
			f = newLoopFixture(redact, startStep{sess: s1}, startStep{sess: s2})

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
		})
	}
}

func TestSessionLoopResumeStartFailureStartsFresh(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		switch i {
		case 0:
			pushUser(f.inbox, "q1")
		case 1:
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

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1", ""}, f.starter.starts())
	assertOpeningTurn(t, s3.sent(), testPrimer, "q2")
	assert.Len(t, errorEvents(f.decoded(t)), 2)
	assertClosedOnce(t, f.starter)
}

func TestSessionLoopNotLoggedInIsFatal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		reports fakeEvents
	}{
		{name: "reported by the session", reports: resultErrorEvents},
		{name: "not reported, not mistaken for a dead process", reports: noEvents},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s1 := &fakeSession{id: "id-1", reports: tt.reports, reply: func(context.Context, int, string) (harness.Result, error) {
				return harness.Result{Reason: "error"}, fmt.Errorf("turn: %w", claudecode.ErrNotLoggedIn)
			}}
			f := newLoopFixture(nil, startStep{sess: s1})

			err := f.run(t.Context(), t)
			require.ErrorIs(t, err, claudecode.ErrNotLoggedIn)
			assert.Len(t, errorEvents(f.decoded(t)), 1)
			assert.Equal(t, []string{""}, f.starter.starts())
			assertClosedOnce(t, f.starter)
		})
	}
}

// A Send that emits no user_input never reached claude: the process had
// exited while idle. The text goes once to a reopened session, and the dead
// session shows no error.
func TestSessionLoopIdleDeathResendsText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		resumeErr   error
		wantStarts  []string
		wantOpening string // what the reopened session's turn opens with; "" when it is resumed
		wantErrors  int
	}{
		{name: "resumed", wantStarts: []string{"", "id-1"}},
		{
			name:        "resume fails, fresh with the primer",
			resumeErr:   errors.New("no transcript for id-1"),
			wantStarts:  []string{"", "id-1", ""},
			wantOpening: testPrimer,
			wantErrors:  1, // the failed resume start, not the dead session
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var f *loopFixture

			s1 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
				if i == 1 {
					return harness.Result{Reason: "error"}, errDeadProcess
				}

				pushUser(f.inbox, "q1")
				f.inbox.closeInbox()

				return harness.Result{Reason: "done"}, nil
			}}
			reopened := &fakeSession{id: "id-2", reports: crashEvents}

			steps := []startStep{{sess: s1}}
			if tt.resumeErr != nil {
				steps = append(steps, startStep{err: tt.resumeErr})
			}

			f = newLoopFixture(nil, append(steps, startStep{sess: reopened})...)

			require.NoError(t, f.run(t.Context(), t))
			assert.Equal(t, tt.wantStarts, f.starter.starts())
			assert.Equal(t, []string{testOpening, "q1"}, s1.sent())

			if tt.wantOpening == "" {
				assert.Equal(t, []string{"q1"}, reopened.sent())
			} else {
				assertOpeningTurn(t, reopened.sent(), tt.wantOpening, "q1")
			}

			evs := f.decoded(t)
			assert.Len(t, errorEvents(evs), tt.wantErrors)
			assert.Zero(t, awaitingHumanCount(evs))
			assertClosedOnce(t, f.starter)
		})
	}
}

func TestSessionLoopIdleDeathResendsOnlyOnce(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	dead := func(context.Context, int, string) (harness.Result, error) {
		return harness.Result{Reason: "error"}, errDeadProcess
	}
	s1 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(ctx context.Context, i int, text string) (harness.Result, error) {
		if i == 1 {
			return dead(ctx, i, text)
		}

		pushUser(f.inbox, "q1")
		f.inbox.closeInbox()

		return harness.Result{Reason: "done"}, nil
	}}
	s2 := &fakeSession{id: "id-1", reports: crashEvents, reply: dead}

	f = newLoopFixture(nil, startStep{sess: s1}, startStep{sess: s2})

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1"}, f.starter.starts())
	assert.Equal(t, []string{"q1"}, s2.sent())

	evs := f.decoded(t)
	assert.Len(t, errorEvents(evs), 1)
	assert.Equal(t, 1, awaitingHumanCount(evs))
	assertClosedOnce(t, f.starter)
}

// mcpHalt is a turn vendorkit halted because Claude Code marked the
// contextmatrix MCP server failed.
func mcpHalt() (harness.Result, error) {
	return harness.Result{Reason: "error"}, fmt.Errorf("claudecode: turn: %w", &claudecode.MCPServerError{Server: cmMCPServerName, Status: "failed"})
}

// Claude Code keeps a failed MCP server failed for the rest of the process, so
// the halted turn goes once to the conversation resumed on a new one, with no
// error and no awaiting_human: the turn goes on.
func TestSessionLoopMCPHaltMidSessionResumesAndResends(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		if i == 1 {
			return mcpHalt()
		}

		pushUser(f.inbox, "q1")
		f.inbox.closeInbox()

		return harness.Result{Reason: "done"}, nil
	}}
	s2 := &fakeSession{id: "id-1", reports: crashEvents}

	f = newLoopFixture(nil, startStep{sess: s1}, startStep{sess: s2})

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1"}, f.starter.starts())
	assert.Equal(t, []string{testOpening, "q1"}, s1.sent())
	assert.Equal(t, []string{"q1"}, s2.sent())

	evs := f.decoded(t)
	assert.Empty(t, errorEvents(evs))
	assert.Zero(t, awaitingHumanCount(evs))
	assertClosedOnce(t, f.starter)
}

// A halt on the first turn of a new conversation leaves nothing stored to
// resume. That turn is the opening, which carries the primer and the user text
// that waited for it, so it goes once, alone, to a fresh process.
func TestSessionLoopMCPHaltOnFreshOpeningResendsItAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		clear      bool   // a /clear restarts the boot process before the halt
		opening    string // what the halted process's first turn opens with
		wantStarts []string
	}{
		{name: "boot opening", opening: testOpening, wantStarts: []string{"", ""}},
		{name: "primer after a /clear", clear: true, opening: testPrimer, wantStarts: []string{"", "", ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			halted := &fakeSession{id: "id-1", reports: crashEvents, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
				if i == 0 {
					return mcpHalt()
				}

				return harness.Result{Reason: "done"}, nil
			}}
			steps := []startStep{{sess: halted}}
			fs := []frames.Frame{userFrame("q1")}

			if tt.clear {
				boot := &fakeSession{id: "id-0", reply: func(ctx context.Context, _ int, _ string) (harness.Result, error) {
					return blockUntilCanceled(ctx)
				}}
				steps = []startStep{{sess: boot}, {sess: halted}}
				fs = []frames.Frame{{Type: frames.TypeClear}, userFrame("q1")}
			}

			fresh := &fakeSession{id: "id-2", reports: crashEvents}
			f := newLoopFixture(nil, append(steps, startStep{sess: fresh})...)
			pumpFrames(t, f.inbox, f.clearCh, fs...)

			require.NoError(t, f.run(t.Context(), t))
			assert.Equal(t, tt.wantStarts, f.starter.starts())
			assertOpeningTurn(t, halted.sent(), tt.opening, "q1")
			assert.Equal(t, halted.sent(), fresh.sent(), "the halted turn is resent whole")

			for _, s := range []*fakeSession{halted, fresh} {
				assert.Equal(t, 1, strings.Count(strings.Join(s.sent(), "\n"), "q1"), "user text sends to %s", s.id)
			}

			evs := f.decoded(t)
			assert.Empty(t, errorEvents(evs))
			assert.Zero(t, awaitingHumanCount(evs))
			assertClosedOnce(t, f.starter)
		})
	}
}

// A fresh process that cannot start for the halted opening shows its start
// error, returns CM's chat to idle, and the next message opens a fresh
// conversation instead of resuming one with nothing stored.
func TestSessionLoopMCPHaltFreshStartFailureAwaitsHuman(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(context.Context, int, string) (harness.Result, error) {
		return mcpHalt()
	}}
	s3 := &fakeSession{id: "id-3", reports: crashEvents}

	f = newLoopFixture(nil,
		startStep{sess: s1},
		startStep{err: errors.New("spawn failed"), hook: func() {
			pushUser(f.inbox, "q1")
			f.inbox.closeInbox()
		}},
		startStep{sess: s3},
	)

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "", ""}, f.starter.starts())
	assertOpeningTurn(t, s3.sent(), testPrimer, "q1")

	evs := f.decoded(t)
	assert.Len(t, errorEvents(evs), 1)
	assert.Equal(t, 1, awaitingHumanCount(evs))
	assertClosedOnce(t, f.starter)
}

// A resumed process halted on its first turn still has the transcript it
// resumed, so the turn goes to the same conversation resumed again.
func TestSessionLoopMCPHaltOnResumedProcessResumesAgain(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		switch i {
		case 0:
			pushUser(f.inbox, "q1")
		case 1:
			pushUser(f.inbox, "q2")
			f.inbox.closeInbox()

			return harness.Result{Reason: "error"}, &claudecode.UsageLimitError{Window: "five_hour"}
		}

		return harness.Result{Reason: "done"}, nil
	}}
	s2 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(context.Context, int, string) (harness.Result, error) {
		return mcpHalt()
	}}
	s3 := &fakeSession{id: "id-1", reports: crashEvents}

	f = newLoopFixture(nil, startStep{sess: s1}, startStep{sess: s2}, startStep{sess: s3})

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "id-1", "id-1"}, f.starter.starts())
	assert.Equal(t, []string{"q2"}, s2.sent())
	assert.Equal(t, []string{"q2"}, s3.sent())

	evs := f.decoded(t)
	assert.Empty(t, errorEvents(evs))
	assert.Equal(t, 1, awaitingHumanCount(evs), "the usage limit's only")
	assertClosedOnce(t, f.starter)
}

// A resent turn halted again shows one error and one awaiting_human, and the
// next message resumes only a conversation that has something stored.
func TestSessionLoopMCPHaltResendsOnlyOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		asked      []string // messages that arrive during the opening turn
		halted     string   // the boot process's halted turn
		resentID   string   // the ID of the process the halted turn is resent on
		wantStarts []string
		nextOpens  string // what the next process's turn opens with; "" when it resumes
	}{
		{
			name:       "resumed conversation",
			asked:      []string{"q1"},
			halted:     "q1",
			resentID:   "id-1",
			wantStarts: []string{"", "id-1", "id-1"},
		},
		{
			name:       "fresh opening",
			halted:     testOpening,
			resentID:   "id-2",
			wantStarts: []string{"", "", ""},
			nextOpens:  testPrimer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var f *loopFixture

			s1 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(_ context.Context, i int, text string) (harness.Result, error) {
				if i == 0 {
					for _, q := range tt.asked {
						pushUser(f.inbox, q)
					}
				}

				if text == tt.halted {
					return mcpHalt()
				}

				return harness.Result{Reason: "done"}, nil
			}}
			s2 := &fakeSession{id: tt.resentID, reports: crashEvents, reply: func(context.Context, int, string) (harness.Result, error) {
				pushUser(f.inbox, "next")
				f.inbox.closeInbox()

				return mcpHalt()
			}}
			s3 := &fakeSession{id: "id-3", reports: crashEvents}

			f = newLoopFixture(nil, startStep{sess: s1}, startStep{sess: s2}, startStep{sess: s3})

			require.NoError(t, f.run(t.Context(), t))
			assert.Equal(t, tt.wantStarts, f.starter.starts())
			assert.Equal(t, []string{tt.halted}, s2.sent())

			if tt.nextOpens == "" {
				assert.Equal(t, []string{"next"}, s3.sent())
			} else {
				assertOpeningTurn(t, s3.sent(), tt.nextOpens, "next")
			}

			evs := f.decoded(t)
			assert.Len(t, errorEvents(evs), 1)
			assert.Equal(t, 1, awaitingHumanCount(evs))
			assertClosedOnce(t, f.starter)
		})
	}
}

func TestEventTap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		emit  func(e *events.Emitter)
		lines int
		want  tapCounts
	}{
		{
			name:  "user_input",
			emit:  func(e *events.Emitter) { e.Emit(events.UserInput, map[string]any{"content_len": 3}) },
			lines: 1,
			want:  tapCounts{userInputs: 1},
		},
		{
			name:  "error",
			emit:  func(e *events.Emitter) { e.Emit(events.ErrorKind, map[string]any{"error": "boom"}) },
			lines: 1,
			want:  tapCounts{errors: 1},
		},
		{
			name:  "awaiting_human",
			emit:  func(e *events.Emitter) { e.Emit(events.StateChange, map[string]any{"state": "awaiting_human"}) },
			lines: 1,
			want:  tapCounts{awaiting: 1},
		},
		{
			name: "other events are not counted",
			emit: func(e *events.Emitter) {
				e.Emit(events.StateChange, map[string]any{"stop": "done"})
				e.Emit(events.ToolCallKind, map[string]any{"name": "awaiting_human"})
				e.Emit(events.Kind("usage_limit"), map[string]any{"window": "five_hour"})
			},
			lines: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var dst bytes.Buffer

			tap := newEventTap(&dst)
			tt.emit(events.NewEmitter(io.Discard, tap))

			assert.Equal(t, tt.want, tap.counts())
			assert.Len(t, strings.Split(strings.TrimSpace(dst.String()), "\n"), tt.lines)
		})
	}

	t.Run("lines in one write count separately", func(t *testing.T) {
		t.Parallel()

		var dst bytes.Buffer

		tap := newEventTap(&dst)
		before := tap.counts()
		p := []byte(`{"kind":"user_input"}` + "\n" + `{"kind":"error","data":{"error":"x"}}` + "\n")

		n, err := tap.Write(p)
		require.NoError(t, err)
		assert.Equal(t, len(p), n)
		assert.Equal(t, string(p), dst.String())
		assert.Equal(t, tapCounts{userInputs: 1, errors: 1}, tap.counts().since(before))
	})
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

		pushUser(f.inbox, "q1")

		return harness.Result{Reason: "done"}, nil
	}}

	f = newLoopFixture(nil, startStep{sess: s1})

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{""}, f.starter.starts())
	assertClosedOnce(t, f.starter)
}

// A turn that completed but returned an error (a transcript-store failure)
// leaves a healthy process: no error event, no close, the next message goes to
// the same session.
func TestSessionLoopCompletedTurnErrorKeepsSession(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reports: inputEvents, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		switch i {
		case 0:
			pushUser(f.inbox, "q1")
		case 1:
			pushUser(f.inbox, "q2")
			f.inbox.closeInbox()

			return harness.Result{Reason: "done", Completed: true}, errors.New("store transcripts: disk full")
		}

		return harness.Result{Reason: "done", Completed: true}, nil
	}}
	f = newLoopFixture(nil, startStep{sess: s1})

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{""}, f.starter.starts())
	assert.Equal(t, []string{testOpening, "q1", "q2"}, s1.sent())

	evs := f.decoded(t)
	assert.Empty(t, errorEvents(evs))
	assert.Zero(t, awaitingHumanCount(evs))
	assertClosedOnce(t, f.starter)
}

// A /clear mid-turn whose fresh start fails still returns CM's chat to idle.
func TestSessionLoopClearThenFailedRestartAwaitsHuman(t *testing.T) {
	t.Parallel()

	s1 := &fakeSession{id: "id-1", reply: func(ctx context.Context, _ int, _ string) (harness.Result, error) {
		return blockUntilCanceled(ctx)
	}}
	s3 := &fakeSession{id: "id-3"}
	f := newLoopFixture(nil, startStep{sess: s1}, startStep{err: errors.New("spawn failed")}, startStep{sess: s3})

	pumpFrames(t, f.inbox, f.clearCh, userFrame("stale"), frames.Frame{Type: frames.TypeClear}, userFrame("after"))

	require.NoError(t, f.run(t.Context(), t))
	assert.Equal(t, []string{"", "", ""}, f.starter.starts())
	assertOpeningTurn(t, s3.sent(), testPrimer, "after")

	evs := f.decoded(t)
	assert.Len(t, errorEvents(evs), 1)
	assert.Equal(t, 1, awaitingHumanCount(evs))
	assertClosedOnce(t, f.starter)
}

// levelRecorder is a slog.Handler that records each record's level.
type levelRecorder struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (h *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *levelRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.levels = append(h.levels, r.Level)

	return nil
}

func (h *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *levelRecorder) WithGroup(string) slog.Handler { return h }

// assertNoWarnings checks that rec recorded something and nothing at WARN or above.
func assertNoWarnings(t *testing.T, rec *levelRecorder) {
	t.Helper()

	rec.mu.Lock()
	defer rec.mu.Unlock()

	require.NotEmpty(t, rec.levels)

	for _, lvl := range rec.levels {
		assert.Less(t, lvl, slog.LevelWarn)
	}
}

// A halted turn that the resend recovers is not a warning: the worker's stderr
// becomes a chat row, and a warning shows there as a problem.
func TestSessionLoopMCPHaltResendLogsNoWarning(t *testing.T) {
	t.Parallel()

	halted := &fakeSession{id: "id-1", reports: crashEvents, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		if i == 0 {
			return mcpHalt()
		}

		return harness.Result{Reason: "done"}, nil
	}}
	fresh := &fakeSession{id: "id-2", reports: crashEvents}
	f := newLoopFixture(nil, startStep{sess: halted}, startStep{sess: fresh})
	rec := &levelRecorder{}
	f.loop.log = slog.New(rec)
	pumpFrames(t, f.inbox, f.clearCh, userFrame("q1"))

	require.NoError(t, f.run(t.Context(), t))
	assertNoWarnings(t, rec)
}

// A text the exited process never took, resent on a resumed one, is a
// recovery, not a warning.
func TestSessionLoopIdleDeathResendLogsNoWarning(t *testing.T) {
	t.Parallel()

	var f *loopFixture

	s1 := &fakeSession{id: "id-1", reports: crashEvents, reply: func(_ context.Context, i int, _ string) (harness.Result, error) {
		if i == 0 {
			pushUser(f.inbox, "q1")
			f.inbox.closeInbox()

			return harness.Result{Reason: "done"}, nil
		}

		return harness.Result{Reason: "error"}, errDeadProcess
	}}
	resumed := &fakeSession{id: "id-1", reports: crashEvents}
	f = newLoopFixture(nil, startStep{sess: s1}, startStep{sess: resumed})
	rec := &levelRecorder{}
	f.loop.log = slog.New(rec)

	require.NoError(t, f.run(t.Context(), t))
	require.Equal(t, []string{"q1"}, resumed.sent(), "the resend reached the resumed session")
	assertNoWarnings(t, rec)
}
