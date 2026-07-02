package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-chat/internal/metrics"
)

func TestResolveOutcome(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		exitCode int64
		want     string
	}{
		{"clean exit", "", 0, metrics.OutcomeSuccess},
		{"nonzero exit", "", 1, metrics.OutcomeFailure},
		{"killed reason", metrics.OutcomeKilled, 137, metrics.OutcomeKilled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveOutcome(tc.reason, tc.exitCode))
		})
	}
}

func TestTrackerReason(t *testing.T) {
	tr := NewTracker(2)
	run := &Run{SessionID: "session-1"}
	require.True(t, tr.AddIfUnderLimit(run))

	assert.Empty(t, tr.Reason("session-1"), "no reason recorded yet")

	tr.SetReason("session-1", metrics.OutcomeKilled)
	assert.Equal(t, metrics.OutcomeKilled, tr.Reason("session-1"))

	// Remove clears the reason.
	tr.Remove("session-1")
	assert.Empty(t, tr.Reason("session-1"))

	// SetReason on an absent run is a no-op (no dangling entry).
	tr.SetReason("ghost", metrics.OutcomeKilled)
	assert.Empty(t, tr.Reason("ghost"))
}
