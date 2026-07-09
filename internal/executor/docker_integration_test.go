package executor

import (
	"context"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-chat/internal/metrics"
)

// integrationGuard skips the test unless CMX_TEST_DOCKER is set. These tests
// require a reachable Docker daemon and pull the alpine image.
func integrationGuard(t *testing.T) {
	t.Helper()

	if os.Getenv("CMX_TEST_DOCKER") == "" {
		t.Skip("set CMX_TEST_DOCKER=1 to run docker integration tests")
	}
}

const alpineImage = "alpine:3"

// exitRecorder collects onExit callbacks so a test can wait for the exit and
// assert the code.
type exitRecorder struct {
	mu    sync.Mutex
	done  chan struct{}
	once  sync.Once
	code  int64
	fired bool
}

func newExitRecorder() *exitRecorder {
	return &exitRecorder{done: make(chan struct{})}
}

func (r *exitRecorder) onExit(_ string, code int64) {
	r.mu.Lock()
	r.code = code
	r.fired = true
	r.mu.Unlock()
	r.once.Do(func() { close(r.done) })
}

func (r *exitRecorder) wait(t *testing.T, d time.Duration) int64 {
	t.Helper()

	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()

		return r.code
	case <-time.After(d):
		t.Fatalf("onExit did not fire within %s", d)

		return 0
	}
}

// logCollector accumulates onLog lines.
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCollector) onLog(_ string, line []byte, _ bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lines = append(c.lines, string(line))
}

func (c *logCollector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.lines...)
}

func newTestExecutor(t *testing.T, cfg Config) *DockerExecutor {
	t.Helper()

	cli, err := NewClient()
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })

	cfg.Docker = cli

	if cfg.Tracker == nil {
		cfg.Tracker = NewTracker(8)
	}

	if cfg.PullPolicy == "" {
		cfg.PullPolicy = PullIfNotPresent
	}

	return NewDockerExecutor(cfg)
}

func TestIntegration_LaunchEchoAndExit(t *testing.T) {
	integrationGuard(t)

	exits := newExitRecorder()
	logs := &logCollector{}
	m := metrics.New()

	exec := newTestExecutor(t, Config{
		OnExit:  exits.onExit,
		OnLog:   logs.onLog,
		Metrics: m,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const sessionID = "echo-session-1"

	spec := LaunchSpec{
		SessionID:   sessionID,
		Image:       alpineImage,
		MemoryBytes: 256 * 1024 * 1024,
		PidsLimit:   128,
		Cmd:         []string{"sh", "-c", "read line; echo got:$line; sleep 1"},
	}

	require.NoError(t, exec.Launch(ctx, spec))

	run, ok := exec.tracker.Get(sessionID)
	require.True(t, ok, "run must be tracked after launch")

	// Inspect: the container must carry the session labels.
	info, err := exec.docker.ContainerInspect(ctx, run.ContainerID)
	require.NoError(t, err)
	assert.Equal(t, "true", info.Config.Labels[labelChat])
	assert.Equal(t, sessionID, info.Config.Labels[labelSession])

	// Drive stdin: the container reads one line, echoes it, then exits 0.
	_, err = run.Stdin.Write([]byte("hello\n"))
	require.NoError(t, err)

	code := exits.wait(t, 30*time.Second)
	assert.Equal(t, int64(0), code, "clean exit")

	// Verify that waitAndCleanup observed exactly one container_duration sample.
	// The observation happens before onExit fires, so by the time wait() returns
	// the histogram is already populated.
	assert.Equal(t, 1, testutil.CollectAndCount(m.ContainerDuration),
		"one container_duration series after one exit")

	mfs, err := m.Registry.Gather()
	require.NoError(t, err)

	var durationFound bool

	for _, mf := range mfs {
		if mf.GetName() != "cm_chat_container_duration_seconds" {
			continue
		}

		require.Len(t, mf.GetMetric(), 1, "exactly one outcome label combination")

		var outcomeVal string

		for _, lp := range mf.GetMetric()[0].GetLabel() {
			if lp.GetName() == "outcome" {
				outcomeVal = lp.GetValue()
			}
		}

		assert.Equal(t, metrics.OutcomeSuccess, outcomeVal, "outcome label must be success")
		assert.Equal(t, uint64(1), mf.GetMetric()[0].GetHistogram().GetSampleCount(),
			"histogram sample count must be 1")

		durationFound = true

		break
	}

	require.True(t, durationFound, "cm_chat_container_duration_seconds family must be present")

	// Pump captured the echoed line.
	assert.Eventually(t, func() bool {
		return slices.Contains(logs.snapshot(), "got:hello")
	}, 5*time.Second, 50*time.Millisecond, "expected pump to capture got:hello")

	// Container removed, tracker empty.
	assert.Eventually(t, func() bool {
		return exec.tracker.Count() == 0
	}, 5*time.Second, 50*time.Millisecond)

	_, err = exec.docker.ContainerInspect(ctx, run.ContainerID)
	assert.Error(t, err, "container must be removed after exit")
}

// TestIntegration_LongLivedContainerNotIdleReaped asserts that the chat
// executor has no idle watchdog: a silent container must remain running well
// past what would have been an idle window in the agent executor.
func TestIntegration_LongLivedContainerNotIdleReaped(t *testing.T) {
	integrationGuard(t)

	exits := newExitRecorder()

	exec := newTestExecutor(t, Config{
		OnExit: exits.onExit,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const sessionID = "long-lived-session"

	require.NoError(t, exec.Launch(ctx, LaunchSpec{
		SessionID:   sessionID,
		Image:       alpineImage,
		MemoryBytes: 256 * 1024 * 1024,
		PidsLimit:   128,
		Cmd:         []string{"sleep", "60"},
	}))

	// Wait well past what would have been an agent idle window (200 ms in the
	// agent integration tests). The chat executor has no watchdog, so the
	// silent container must remain running.
	time.Sleep(500 * time.Millisecond)

	select {
	case <-exits.done:
		t.Fatal("container was reaped; the chat executor must not idle-kill containers")
	default:
	}

	assert.Equal(t, 1, exec.tracker.Count(), "long-lived container still tracked after idle window")

	// Clean up: kill it explicitly so the test leaves no containers behind.
	run, ok := exec.tracker.Get(sessionID)
	require.True(t, ok)
	require.NoError(t, exec.docker.ContainerKill(ctx, run.ContainerID, "SIGKILL"))

	code := exits.wait(t, 10*time.Second)
	assert.Equal(t, int64(137), code, "SIGKILL surfaces 137 via the wait path")

	assert.Eventually(t, func() bool {
		return exec.tracker.Count() == 0
	}, 5*time.Second, 50*time.Millisecond)
}

func TestIntegration_StopAllAndCleanupOrphans(t *testing.T) {
	integrationGuard(t)

	exits := newExitRecorder()

	exec := newTestExecutor(t, Config{
		OnExit: exits.onExit,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	require.NoError(t, exec.Launch(ctx, LaunchSpec{
		SessionID:   "stop-session-1",
		Image:       alpineImage,
		MemoryBytes: 256 * 1024 * 1024,
		PidsLimit:   128,
		Cmd:         []string{"sleep", "60"},
	}))

	killed, err := exec.StopAll(ctx)
	require.NoError(t, err)
	assert.Len(t, killed, 1)

	code := exits.wait(t, 30*time.Second)
	assert.Equal(t, int64(137), code, "SIGKILL surfaces 137 via the wait path")

	assert.Eventually(t, func() bool {
		return exec.tracker.Count() == 0
	}, 5*time.Second, 50*time.Millisecond)

	// CleanupOrphans is a no-op now (nothing labeled remains) but must not error.
	require.NoError(t, exec.CleanupOrphans(ctx))

	left, err := exec.docker.ContainerList(ctx, container.ListOptions{All: true})
	require.NoError(t, err)

	for _, c := range left {
		assert.NotEqual(t, "true", c.Labels[labelChat], "no chat container should remain")
	}
}

// TestIntegration_OnExitRunsWhileTracked asserts that waitAndCleanup fires
// onExit (chatExit's run-dir teardown) before it releases the tracker slot.
// If the tracker slot were released first, a concurrent same-session
// /chat/start could pass the conflict check, recreate the run dir, and write
// a fresh primer.txt/resume.jsonl that this cleanup would then delete out
// from under the restarted session.
func TestIntegration_OnExitRunsWhileTracked(t *testing.T) {
	integrationGuard(t)

	tracker := NewTracker(8)

	var trackedAtExit atomic.Bool

	done := make(chan struct{})

	var once sync.Once

	exec := newTestExecutor(t, Config{
		Tracker: tracker,
		OnExit: func(sessionID string, _ int64) {
			_, ok := tracker.Get(sessionID)
			trackedAtExit.Store(ok)
			once.Do(func() { close(done) })
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const sessionID = "ordering-session-1"

	require.NoError(t, exec.Launch(ctx, LaunchSpec{
		SessionID:   sessionID,
		Image:       alpineImage,
		MemoryBytes: 256 * 1024 * 1024,
		PidsLimit:   128,
		Cmd:         []string{"sh", "-c", "true"}, // exits 0 immediately
	}))

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("onExit did not fire")
	}

	assert.True(t, trackedAtExit.Load(),
		"onExit (chatExit run-dir teardown) must run while the session is still tracked, "+
			"so a concurrent same-session /chat/start stays 409-blocked until cleanup completes")
}
