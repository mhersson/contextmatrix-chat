package webhook

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDedup_CheckAndRecordThenRollback(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	// First call is fresh: it records the pair and reports no prior presence.
	// A second call now sees the just-recorded pair and reports a duplicate.
	assert.False(t, d.CheckAndRecord("sess-1", "m1"))
	assert.True(t, d.CheckAndRecord("sess-1", "m1"))

	// Rollback undoes the record, so a retry after a failed delivery is not
	// silenced: the next call is fresh again, not a duplicate.
	d.Rollback("sess-1", "m1")
	assert.False(t, d.CheckAndRecord("sess-1", "m1"))
	assert.True(t, d.CheckAndRecord("sess-1", "m1"))
}

func TestDedup_EmptyMessageIDNeverDedups(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	// Empty message_id is never deduped: every call is treated as fresh and
	// nothing is ever recorded for it.
	assert.False(t, d.CheckAndRecord("sess-1", ""))
	assert.False(t, d.CheckAndRecord("sess-1", ""))
	assert.False(t, d.CheckAndRecord("sess-1", ""))
}

func TestDedup_KeyedByBothFields(t *testing.T) {
	d := NewDedupCache(time.Minute, 16)

	assert.False(t, d.CheckAndRecord("sess-1", "m1"))
	// Same message_id under the same session is now a duplicate...
	assert.True(t, d.CheckAndRecord("sess-1", "m1"))
	// ...but under a different session it is a distinct, fresh entry.
	assert.False(t, d.CheckAndRecord("sess-2", "m1"))
}

func TestDedup_CapacityEvictsOldest(t *testing.T) {
	d := NewDedupCache(time.Hour, 2)

	assert.False(t, d.CheckAndRecord("s", "a"))
	assert.False(t, d.CheckAndRecord("s", "b"))
	assert.False(t, d.CheckAndRecord("s", "x")) // evicts "a"

	// "b" and "x" are still remembered. Assert them before "a": a fresh result
	// for "a" re-records it, which would itself evict the oldest survivor and
	// disturb these assertions if checked first.
	assert.True(t, d.CheckAndRecord("s", "b"))
	assert.True(t, d.CheckAndRecord("s", "x"))
	// "a" was evicted, so it is no longer remembered: this call reports it
	// fresh again.
	assert.False(t, d.CheckAndRecord("s", "a"))
}

func TestDedup_TTLExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	d := NewDedupCache(time.Minute, 16, withDedupClock(func() time.Time { return now }))

	assert.False(t, d.CheckAndRecord("s", "m1"))
	require.True(t, d.CheckAndRecord("s", "m1"))

	now = now.Add(2 * time.Minute)

	// Expired: the pair is reaped and reported fresh again.
	assert.False(t, d.CheckAndRecord("s", "m1"))
}
