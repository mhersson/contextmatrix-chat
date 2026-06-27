package webhook

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/mhersson/contextmatrix-chat/internal/logbridge"
	"github.com/mhersson/contextmatrix-chat/internal/metrics"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const (
	// maxRequestBodyBytes caps the body the auth middleware reads before HMAC
	// verification. A larger body is a misbehaving or hostile client.
	maxRequestBodyBytes = 1 << 20 // 1 MiB
)

// Config carries the dependencies NewServer needs. Pointers may be shared with
// the serve layer; the server does not take ownership of their lifecycles.
type Config struct {
	APIKey string
	Skew   time.Duration

	Hub *logbridge.Hub

	Replay *ReplayCache
	Dedup  *DedupCache

	Draining *atomic.Bool

	// KeepaliveInterval overrides the SSE heartbeat period. Zero uses the
	// package default; tests shrink it.
	KeepaliveInterval time.Duration

	// Metrics is the Prometheus bundle. Nil disables request instrumentation.
	Metrics *metrics.Metrics

	Logger *slog.Logger
}

// Server is the chat backend's HTTP surface. It owns no goroutines; the replay
// janitor lives in its owner. Chat lifecycle handlers (POST /chat/start,
// POST /chat/end, POST /message) are added in 2.7b and will reference the
// Hub, Dedup, and an executor injected at that time.
type Server struct {
	apiKey string
	skew   time.Duration

	hub *logbridge.Hub

	replay *ReplayCache
	dedup  *DedupCache

	draining *atomic.Bool

	// keepaliveInterval is the SSE comment heartbeat period. Zero means the
	// package default. Tests shrink it; production leaves it unset.
	keepaliveInterval time.Duration

	metrics *metrics.Metrics

	logger *slog.Logger
}

// NewServer wires a Server from its dependencies. The replay cache, dedup
// cache, and draining flag are created if the caller leaves them nil so a bare
// Config still yields a usable server (tests rely on this).
func NewServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	skew := cfg.Skew
	if skew == 0 {
		skew = protocol.DefaultMaxClockSkew
	}

	replay := cfg.Replay
	if replay == nil {
		replay = NewReplayCache(skew, 4096)
	}

	dedup := cfg.Dedup
	if dedup == nil {
		dedup = NewDedupCache(10*time.Minute, 4096)
	}

	draining := cfg.Draining
	if draining == nil {
		draining = &atomic.Bool{}
	}

	return &Server{
		apiKey:            cfg.APIKey,
		skew:              skew,
		hub:               cfg.Hub,
		replay:            replay,
		dedup:             dedup,
		draining:          draining,
		keepaliveInterval: cfg.KeepaliveInterval,
		metrics:           cfg.Metrics,
		logger:            logger,
	}
}

// Routes returns the mux with the infra routes mounted. Chat lifecycle routes
// (POST /chat/start, POST /chat/end, POST /message) are added in 2.7b once
// the executor and session tracker are available.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /logs", s.recordMetrics(s.auth(s.handleLogs)))
	mux.HandleFunc("GET /health", s.recordMetrics(s.handleHealth))
	mux.HandleFunc("GET /readyz", s.recordMetrics(s.handleReadyz))

	return mux
}

// AdminAuth exposes the HMAC verifier for the admin /metrics endpoint, which
// the serve layer mounts on a separate loopback listener. It reuses the same
// signed-GET verification, replay cache, and skew as the webhook routes — the
// agent-backend signed-GET HMAC is real auth, preserved here.
func (s *Server) AdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return s.auth(next)
}

// ---- health / readyz --------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// RunningContainers and MaxConcurrent are wired in 2.7b once the session
	// tracker is available.
	writeJSON(w, http.StatusOK, protocol.HealthResponse{OK: true})
}

// readyResponse is the /readyz body. It is a custom shape (not ErrorResponse)
// so the readiness probe stays self-describing for orchestrators.
type readyResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if s.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, readyResponse{OK: false, Reason: "draining"})

		return
	}

	writeJSON(w, http.StatusOK, readyResponse{OK: true})
}

// ---- write helpers ----------------------------------------------------------

// writeJSON marshals v and writes it with the given status. A marshal failure
// falls back to a fixed internal-error body so the client always gets
// well-formed JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")

	body, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"code":"internal","message":"response marshal failed"}`))

		return
	}

	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError serialises a protocol.ErrorResponse. msg must be a fixed,
// client-safe string, never raw err.Error() text.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, protocol.ErrorResponse{OK: false, Code: code, Message: msg})
}
