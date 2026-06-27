package webhook

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDedup_RecordThenContains(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	// Not recorded yet → absent. Recording makes a later Contains report true.
	assert.False(t, d.Contains("sess-1", "m1"))
	d.Record("sess-1", "m1")
	assert.True(t, d.Contains("sess-1", "m1"))
}

func TestDedup_ContainsIsPureRead(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	// Contains must never record: repeated checks stay false until Record runs.
	assert.False(t, d.Contains("sess-1", "m1"))
	assert.False(t, d.Contains("sess-1", "m1"))
	assert.False(t, d.Contains("sess-1", "m1"))
}

func TestDedup_EmptyMessageIDNeverDedups(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	// Empty message_id is never deduped: Record is a no-op, Contains is false.
	d.Record("sess-1", "")
	assert.False(t, d.Contains("sess-1", ""))
	assert.False(t, d.Contains("sess-1", ""))
}

func TestDedup_KeyedByBothFields(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	d.Record("sess-1", "m1")
	// Same message_id under a different session is a distinct entry.
	assert.True(t, d.Contains("sess-1", "m1"))
	assert.False(t, d.Contains("sess-2", "m1"))
}

func TestDedup_CapacityEvictsOldest(t *testing.T) {
	d := NewDedupCache(time.Hour, 2)

	d.Record("s", "a")
	d.Record("s", "b")
	d.Record("s", "x") // evicts "a"

	// "a" was evicted, so it is no longer remembered.
	assert.False(t, d.Contains("s", "a"))
	assert.True(t, d.Contains("s", "b"))
	assert.True(t, d.Contains("s", "x"))
}

func TestDedup_TTLExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	d := NewDedupCache(time.Minute, 16, withDedupClock(func() time.Time { return now }))

	d.Record("s", "m1")
	require.True(t, d.Contains("s", "m1"))

	now = now.Add(2 * time.Minute)

	// Expired: Contains is false and reaps the stale entry.
	assert.False(t, d.Contains("s", "m1"))
}
