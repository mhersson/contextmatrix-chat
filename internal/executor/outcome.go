package executor

import "github.com/mhersson/contextmatrix-chat/internal/metrics"

// resolveOutcome maps the way a container ended to a container_duration outcome
// label. Precedence: a recorded kill reason (killed); otherwise the exit code
// (0 = success, any other = failure). Chat containers are never timed out by
// the executor — timedOut is always false; the parameter is retained so the
// signature stays parallel to the agent implementation.
func resolveOutcome(timedOut bool, reason string, exitCode int64) string {
	switch {
	case timedOut:
		return metrics.OutcomeTimeout
	case reason != "":
		return reason
	case exitCode == 0:
		return metrics.OutcomeSuccess
	default:
		return metrics.OutcomeFailure
	}
}
