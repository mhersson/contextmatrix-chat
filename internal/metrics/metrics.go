// Package metrics defines the Prometheus metric set exposed by the chat
// service. All metrics live on a dedicated prometheus.Registry (not the global
// default) so tests stay hermetic - each test constructs its own *Metrics.
//
// Label cardinality is bounded on purpose: no card_id / project labels;
// endpoint labels pass through NormalizeEndpoint; container outcome is a fixed
// enum; broadcaster drops are unlabeled.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

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
var endpointAllowlist = map[string]struct{}{
	"/chat/start": {},
	"/chat/end":   {},
	"/message":    {},
	"/logs":       {},
	"/health":     {},
	"/readyz":     {},
	"/metrics":    {},
}

// NormalizeEndpoint collapses an arbitrary request path to one of the chat
// service's well-known endpoints, or "other" for unknown paths.
func NormalizeEndpoint(path string) string {
	if _, ok := endpointAllowlist[path]; ok {
		return path
	}

	return "other"
}

// Metrics bundles every Prometheus collector exposed by the chat service. It
// is constructed once at serve startup and injected into the components that
// observe; components never reach for a global.
type Metrics struct {
	// Registry is the registerer these collectors live on, exposed so the admin
	// /metrics handler can be wired to the same registry.
	Registry *prometheus.Registry

	WebhookRequestsTotal   *prometheus.CounterVec
	WebhookRequestDuration *prometheus.HistogramVec
	ContainerDuration      *prometheus.HistogramVec
	RunningContainers      prometheus.Gauge
	BroadcasterDropsTotal  prometheus.Counter
}

// New registers every chat metric on a fresh registry and returns the bundle.
// The dedicated registry also carries the standard Go runtime + Process
// collectors so /metrics exposes go_* / process_* alongside the cm_chat_*
// series - the dedicated-registry shape would otherwise drop them.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	factory := promauto.With(reg)

	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return &Metrics{
		Registry: reg,

		WebhookRequestsTotal: factory.NewCounterVec(
			prometheus.CounterOpts{
				Name: "cm_chat_webhook_requests_total",
				Help: "Total webhook requests processed, labelled by endpoint, HTTP status, and a coarse outcome code.",
			},
			[]string{"endpoint", "status", "code"},
		),

		WebhookRequestDuration: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "cm_chat_webhook_request_duration_seconds",
				Help:    "Wall-clock duration of webhook requests, in seconds.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"endpoint"},
		),

		ContainerDuration: factory.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "cm_chat_container_duration_seconds",
				Help: "Wall-clock container lifetime from start to exit, in seconds.",
				Buckets: []float64{
					1, 5, 15, 30, 60,
					300, 600, 1800, 3600, 7200,
				},
			},
			[]string{"outcome"},
		),

		RunningContainers: factory.NewGauge(prometheus.GaugeOpts{
			Name: "cm_chat_running_containers",
			Help: "Number of containers currently tracked as running.",
		}),

		BroadcasterDropsTotal: factory.NewCounter(prometheus.CounterOpts{
			Name: "cm_chat_broadcaster_drops_total",
			Help: "Total log entries dropped for slow SSE subscribers. Unlabeled to keep series cardinality at O(1).",
		}),
	}
}
