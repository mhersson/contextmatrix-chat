// Package metrics exposes the chat service's Prometheus metric set. The metric
// bundle is the shared backendkit collector set, namespaced "cm_chat" on a
// dedicated prometheus.Registry (not the global default) so tests stay hermetic
// - each call to New constructs its own *Metrics. Chat adds no extra collector,
// so Metrics is a direct alias of the shared bundle.
//
// Label cardinality is bounded on purpose: no card_id / project labels;
// endpoint labels pass through NormalizeEndpoint; container outcome is a fixed
// enum; broadcaster drops are unlabeled.
package metrics

import (
	"slices"

	backendkitmetrics "github.com/mhersson/contextmatrix-backendkit/metrics"
)

// Metrics is the shared backendkit metric bundle. Chat contributes no extra
// collector, so the alias is the whole wrapper: every *metrics.Metrics
// reference resolves to the shared type.
type Metrics = backendkitmetrics.Metrics

// Container-exit outcomes for cm_chat_container_duration_seconds.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeKilled  = "killed"
	OutcomeEnded   = "ended"
)

// endpointAllowlist enumerates the request paths the chat service serves. Any
// inbound path outside this set collapses to "other" so a stray probe cannot
// inflate metric label cardinality. Keep this in lockstep with
// webhook.Server.Routes() (the main mux) plus the admin /metrics path.
var endpointAllowlist = []string{
	"/chat/start",
	"/chat/end",
	"/message",
	"/logs",
	"/health",
	"/readyz",
	"/metrics",
}

// New registers the chat metric set under the cm_chat namespace on a fresh
// registry and returns the bundle.
func New() *Metrics {
	return backendkitmetrics.New("cm_chat", endpointAllowlist)
}

// NormalizeEndpoint collapses an arbitrary request path to one of the chat
// service's well-known endpoints, or "other" for unknown paths.
func NormalizeEndpoint(path string) string {
	if slices.Contains(endpointAllowlist, path) {
		return path
	}

	return "other"
}
