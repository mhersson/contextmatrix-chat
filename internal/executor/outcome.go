package executor

import "github.com/mhersson/contextmatrix-chat/internal/metrics"

// resolveOutcome maps the way a container ended to a container_duration outcome
// label. Precedence: a recorded kill reason (killed); otherwise the exit code
// (0 = success, any other = failure). Chat containers are never timed out by
// the executor.
func resolveOutcome(reason string, exitCode int64) string {
	switch {
	case reason != "":
		return reason
	case exitCode == 0:
		return metrics.OutcomeSuccess
	default:
		return metrics.OutcomeFailure
	}
}
