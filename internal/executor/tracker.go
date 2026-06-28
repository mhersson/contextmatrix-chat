// Package executor launches and supervises one Docker container per chat
// session. The Tracker gates concurrency and records per-run state;
// DockerExecutor owns the container lifecycle behind the Executor interface so
// a future KubernetesExecutor can slot in without touching the serve layer.
package executor

import (
	"io"
	"sync"
	"time"
)

// Run is the in-memory record of one live container. ContainerID is the Docker
// ID; Stdin is the attached stdin stream (/message frames flow over it for the
// container's whole life) and is nil in unit tests that exercise the tracker
// without Docker. Stdin is single-writer: callers must serialize writes per
// run — concurrent writers (e.g. webhook handlers on separate HTTP goroutines)
// would interleave frame bytes on the wire.
type Run struct {
	ContainerID string
	SessionID   string
	StartedAt   time.Time
	Stdin       io.WriteCloser
}

// Tracker is the concurrency gate and run registry. It is safe for concurrent
// use. Keys are sessionID, enforcing one container per session.
type Tracker struct {
	mu     sync.Mutex
	byKey  map[string]*Run
	max    int
	reason map[string]string
}

// NewTracker returns a Tracker that admits at most maxConcurrent runs.
func NewTracker(maxConcurrent int) *Tracker {
	return &Tracker{
		byKey:  make(map[string]*Run),
		max:    maxConcurrent,
		reason: make(map[string]string),
	}
}

// AddIfUnderLimit registers r unless the tracker is at capacity or a run with
// the same session ID is already present (one container per session). It
// returns true when the run was admitted, false when it was refused.
func (t *Tracker) AddIfUnderLimit(r *Run) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, exists := t.byKey[r.SessionID]; exists {
		return false
	}

	if len(t.byKey) >= t.max {
		return false
	}

	t.byKey[r.SessionID] = r

	return true
}

// Get returns the run for sessionID and whether it is tracked.
func (t *Tracker) Get(sessionID string) (*Run, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	r, ok := t.byKey[sessionID]

	return r, ok
}

// Remove deletes the run and all auxiliary state for sessionID. It is
// idempotent: removing an absent key is a no-op.
func (t *Tracker) Remove(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.byKey, sessionID)
	delete(t.reason, sessionID)
}

// List returns a snapshot slice of the tracked runs. Mutating the slice does
// not affect the tracker, but the *Run elements are shared pointers, not deep
// copies — mutating a Run's fields is visible to every other holder.
func (t *Tracker) List() []*Run {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]*Run, 0, len(t.byKey))
	for _, r := range t.byKey {
		out = append(out, r)
	}

	return out
}

// Count returns the number of tracked runs.
func (t *Tracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.byKey)
}

// SetReason records why a container is being terminated so waitAndCleanup can
// label container_duration_seconds. Call it before the SIGKILL it explains. It
// no-ops for an untracked run so a dead key never carries a stale reason.
func (t *Tracker) SetReason(sessionID, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.byKey[sessionID]; !ok {
		return
	}

	t.reason[sessionID] = reason
}

// Reason returns the recorded termination reason for sessionID, or "" if none
// was recorded.
func (t *Tracker) Reason(sessionID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.reason[sessionID]
}
