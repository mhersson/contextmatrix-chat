package executor

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTracker_AddIfUnderLimit_CapacityBoundary(t *testing.T) {
	tr := NewTracker(2)

	assert.True(t, tr.AddIfUnderLimit(&Run{SessionID: "session-a"}))
	assert.True(t, tr.AddIfUnderLimit(&Run{SessionID: "session-b"}))
	assert.Equal(t, 2, tr.Count())

	// Third distinct add is refused at capacity.
	assert.False(t, tr.AddIfUnderLimit(&Run{SessionID: "session-c"}))
	assert.Equal(t, 2, tr.Count())
}

func TestTracker_AddIfUnderLimit_DuplicateKeyRefused(t *testing.T) {
	tr := NewTracker(5)

	require.True(t, tr.AddIfUnderLimit(&Run{SessionID: "session-a"}))
	// Re-adding the same session ID is refused (one container per session).
	assert.False(t, tr.AddIfUnderLimit(&Run{SessionID: "session-a"}))
	assert.Equal(t, 1, tr.Count())
}

func TestTracker_Get(t *testing.T) {
	tr := NewTracker(5)
	r := &Run{SessionID: "session-a", ContainerID: "cid"}

	require.True(t, tr.AddIfUnderLimit(r))

	got, ok := tr.Get("session-a")
	require.True(t, ok)
	assert.Equal(t, "cid", got.ContainerID)

	_, ok = tr.Get("missing")
	assert.False(t, ok)
}

func TestTracker_Remove_Idempotent(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{SessionID: "session-a"}))

	tr.Remove("session-a")
	assert.Equal(t, 0, tr.Count())

	// Removing again is a no-op, not a panic.
	tr.Remove("session-a")
	tr.Remove("never-existed")
	assert.Equal(t, 0, tr.Count())
}

func TestTracker_RemoveClearsReason(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{SessionID: "session-a"}))

	tr.SetReason("session-a", "killed")
	tr.Remove("session-a")

	// After removal the reason map is cleared too.
	assert.Empty(t, tr.Reason("session-a"))

	// A fresh add reuses the slot without stale reason state.
	require.True(t, tr.AddIfUnderLimit(&Run{SessionID: "session-a"}))
	assert.Empty(t, tr.Reason("session-a"))
}

func TestTracker_List_Snapshot(t *testing.T) {
	tr := NewTracker(5)
	require.True(t, tr.AddIfUnderLimit(&Run{SessionID: "session-a"}))
	require.True(t, tr.AddIfUnderLimit(&Run{SessionID: "session-b"}))

	list := tr.List()
	assert.Len(t, list, 2)

	// Mutating the returned slice must not affect the tracker.
	list[0] = nil

	assert.Equal(t, 2, tr.Count())
	assert.Len(t, tr.List(), 2)
}

func TestTracker_ConcurrentAdds_RaceClean(t *testing.T) {
	const limit = 50

	tr := NewTracker(limit)

	var wg sync.WaitGroup

	var (
		mu       sync.Mutex
		accepted int
	)

	for i := range 200 {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()

			r := &Run{SessionID: sessionName(n)}
			if tr.AddIfUnderLimit(r) {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	assert.Equal(t, limit, accepted)
	assert.Equal(t, limit, tr.Count())
}

func sessionName(n int) string {
	const digits = "0123456789"

	if n == 0 {
		return "session-0"
	}

	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}

	return "session-" + string(b)
}
