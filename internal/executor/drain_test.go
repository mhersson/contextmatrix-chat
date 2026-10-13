package executor

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testGuard bounds how long a test waits for an event that must happen; it
// only turns a hang into a failure and never orders events.
const testGuard = 5 * time.Second

// streamDocker is a client.APIClient that serves one container whose attach
// output the test writes by hand and whose exit the test triggers. Calls it
// does not override hit the nil embedded interface and panic.
type streamDocker struct {
	client.APIClient

	out    *bufio.Reader
	stdin  net.Conn
	exitCh chan container.WaitResponse
}

func newStreamDocker(t *testing.T) (*streamDocker, *io.PipeWriter) {
	t.Helper()

	pr, pw := io.Pipe()
	stdin, peer := net.Pipe()

	t.Cleanup(func() {
		_ = pw.Close()
		_ = peer.Close()
	})

	return &streamDocker{
		out:    bufio.NewReader(pr),
		stdin:  stdin,
		exitCh: make(chan container.WaitResponse, 1),
	}, pw
}

func (d *streamDocker) ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	return client.ContainerCreateResult{ID: "container-1"}, nil
}

func (d *streamDocker) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	return client.ContainerAttachResult{HijackedResponse: client.HijackedResponse{Conn: d.stdin, Reader: d.out}}, nil
}

func (d *streamDocker) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, nil
}

func (d *streamDocker) ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
	return client.ContainerWaitResult{Result: d.exitCh, Error: make(chan error)}
}

func (d *streamDocker) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	return client.ContainerRemoveResult{}, nil
}

// writeFrame writes payload as one multiplexed attach frame for stream.
func writeFrame(t *testing.T, w io.Writer, stream stdcopy.StdType, payload string) {
	t.Helper()

	header := make([]byte, 8)
	header[0] = byte(stream)
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))

	_, err := w.Write(append(header, payload...))
	require.NoError(t, err)
}

// exitWatcher records bridged lines and snapshots them when onExit fires.
type exitWatcher struct {
	mu          sync.Mutex
	lines       []string
	linesAtExit []string
	exited      chan struct{}
}

func newExitWatcher() *exitWatcher {
	return &exitWatcher{exited: make(chan struct{})}
}

func (w *exitWatcher) onLog(_ string, line []byte, _ bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.lines = append(w.lines, string(line))
}

func (w *exitWatcher) onExit(string, int64) {
	w.mu.Lock()
	w.linesAtExit = append([]string(nil), w.lines...)
	w.mu.Unlock()

	close(w.exited)
}

// warnRecorder is a slog handler that keeps the warn-level records it sees.
type warnRecorder struct {
	mu    sync.Mutex
	warns []slog.Record
}

func (h *warnRecorder) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

func (h *warnRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.warns = append(h.warns, r)

	return nil
}

func (h *warnRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *warnRecorder) WithGroup(string) slog.Handler { return h }

func (h *warnRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.warns)
}

func launchStreamed(t *testing.T, d *streamDocker, w *exitWatcher, logger *slog.Logger) *DockerExecutor {
	t.Helper()

	return NewDockerExecutor(Config{
		Docker:     d,
		Tracker:    NewTracker(1),
		PullPolicy: PullNever,
		OnExit:     w.onExit,
		OnLog:      w.onLog,
		Logger:     logger,
	})
}

// TestWaitAndCleanup_BridgesTrailingOutputBeforeOnExit pins the redaction
// ordering: onExit drops the session's secrets from the log-bridge redactor, so
// output still in the attach stream when the container exits must be bridged
// before onExit fires, never after.
func TestWaitAndCleanup_BridgesTrailingOutputBeforeOnExit(t *testing.T) {
	d, out := newStreamDocker(t)
	w := newExitWatcher()
	e := launchStreamed(t, d, w, slog.New(&warnRecorder{}))

	armed := make(chan struct{})
	e.drainAfter = func(time.Duration) <-chan time.Time {
		close(armed)

		return make(chan time.Time)
	}

	require.NoError(t, e.Launch(context.Background(), LaunchSpec{SessionID: "sess-1", Image: "img"}))

	d.exitCh <- container.WaitResponse{StatusCode: 2}

	select {
	case <-w.exited:
		t.Fatal("onExit fired while the attach stream still had output to bridge")
	case <-armed:
	case <-time.After(testGuard):
		t.Fatal("cleanup neither fired onExit nor waited for the output pump")
	}

	// The worker's last words: a panic trace with no trailing newline, written
	// after the container already reported its exit.
	writeFrame(t, out, stdcopy.Stderr, "panic: token sk-live-123")
	require.NoError(t, out.Close())

	select {
	case <-w.exited:
	case <-time.After(testGuard):
		t.Fatal("onExit did not fire after the attach stream ended")
	}

	assert.Contains(t, w.linesAtExit, "panic: token sk-live-123",
		"the trailing line must be bridged while the session's secrets are still registered")
}

// TestWaitAndCleanup_DrainBoundReleasesStuckStream proves a stream that never
// ends cannot hold teardown hostage: when the drain bound expires, onExit still
// fires and the overrun is logged.
func TestWaitAndCleanup_DrainBoundReleasesStuckStream(t *testing.T) {
	d, _ := newStreamDocker(t)
	w := newExitWatcher()
	warns := &warnRecorder{}
	e := launchStreamed(t, d, w, slog.New(warns))

	armed := make(chan struct{})
	expire := make(chan time.Time, 1)
	e.drainAfter = func(time.Duration) <-chan time.Time {
		close(armed)

		return expire
	}

	require.NoError(t, e.Launch(context.Background(), LaunchSpec{SessionID: "sess-1", Image: "img"}))

	d.exitCh <- container.WaitResponse{StatusCode: 0}

	select {
	case <-w.exited:
		t.Fatal("onExit fired without waiting for the output pump")
	case <-armed:
	case <-time.After(testGuard):
		t.Fatal("cleanup never armed the drain bound")
	}

	before := warns.count()

	expire <- time.Now()

	select {
	case <-w.exited:
	case <-time.After(testGuard):
		t.Fatal("onExit did not fire once the drain bound expired")
	}

	assert.Greater(t, warns.count(), before, "an expired drain bound must be logged")
}
