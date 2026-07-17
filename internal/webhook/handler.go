// Package webhook is the chat backend's HTTP surface: the HMAC-verified
// lifecycle endpoints (chat/start, chat/end, message), the SSE /logs stream,
// and the health/readiness probes ContextMatrix drives the backend through.
// The embedded *webhookcore.Core supplies the shared transport surface (HMAC
// auth, the drain gate, request metrics, the SSE /logs stream, and the
// health/readiness/images probes); this package adds the chat-specific
// lifecycle handlers. It implements the contextmatrix-protocol wire contract.
package webhook

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	"github.com/mhersson/contextmatrix-chat/internal/executor"
	"github.com/mhersson/contextmatrix-chat/internal/metrics"
)

// SkillsResolver fetches the task-skills pointer from CM, clones it once, and
// returns the host directory to bind read-only into each worker container. The
// webhook server calls it on each chat/start. *taskskills.Resolver satisfies it.
type SkillsResolver interface {
	Resolve(ctx context.Context) (string, error)
}

// SessionSecretRegistry records and forgets a session's CM-provisioned secrets
// (the LLM key, protocol v0.5.0; the git-credentials bearer, protocol v0.5.2 -
// a session may register either, both, or neither) so the host-side log-bridge
// redactor masks them in bridged worker stderr and unparsable stdout (the only
// masking those surfaces get). handleChatStart registers each key before the
// container starts; DropSession (and the launch-failure path) forgets all of
// a session's keys in one call so the set stays bounded.
// *logbridge.RedactorRegistry satisfies it.
type SessionSecretRegistry interface {
	AddSessionKey(sessionID, key string)
	RemoveSessionKey(sessionID string)
}

// ChatConfig carries the static, per-process chat backend settings. All fields
// are set at serve startup; they are not reloaded at runtime.
type ChatConfig struct {
	// Image is the worker container image to launch.
	Image string
	// MCPURL is the CM MCP endpoint forwarded to each container as CM_MCP_URL.
	MCPURL string
	// ChatRunDirBase is the host root under which per-session run directories
	// (resume.jsonl) are created and mounted at /run/cm-chat.
	ChatRunDirBase string
	// MemoryBytes and PidsLimit are the per-container resource caps.
	MemoryBytes int64
	PidsLimit   int64
	// MaxConcurrent is the concurrency cap enforced before Launch is attempted.
	MaxConcurrent int
	// ToolOutputMaxBytes caps tool output fed into the model context.
	// 0 disables the cap; config default is 131072.
	ToolOutputMaxBytes int
	// CompactionThreshold and CompactionKeepRecentTurns control in-window
	// compaction, forwarded to the worker as CMX_COMPACTION_* env vars.
	CompactionThreshold       float64
	CompactionKeepRecentTurns int
	// BashTimeoutMaxSeconds is the per-command ceiling forwarded to the worker
	// as CMX_BASH_TIMEOUT_MAX_SECONDS.
	BashTimeoutMaxSeconds int
	// WorkerExtraEnv is operator-supplied KEY=VALUE pairs appended to the
	// container env after the CM_*/CMX_* system vars.
	WorkerExtraEnv map[string]string
	// ReasoningEffort is the static reasoning effort forwarded as
	// CMX_REASONING_EFFORT to every worker container. Empty disables it.
	ReasoningEffort string
	// CACertFile is an optional host path to a PEM of extra CA certificate(s).
	// When set, handleChatStart bind-mounts it read-only into each worker at
	// caCertMountPath and points the worker's TLS (harness LLM client, MCP
	// bridge) and git at it. Empty disables it.
	CACertFile string
	// GitCredentialsURL is CM's worker git-credentials endpoint
	// (<container_contextmatrix_url>/api/worker/git-credentials), forwarded to
	// the worker as CM_GIT_CREDENTIALS_URL alongside the payload's
	// GitCredentialsToken so the worker can fetch fresh, per-repo credentials
	// on demand.
	GitCredentialsURL string
}

// Config carries the dependencies NewServer needs. Pointers may be shared with
// the serve layer; the server does not take ownership of their lifecycles.
type Config struct {
	APIKey string
	Skew   time.Duration

	// Executor and Tracker drive the chat container lifecycle. Wired at serve
	// startup; nil in minimal test servers that exercise infra routes only.
	Executor executor.Executor
	Tracker  *executor.Tracker

	// SkillsResolver fetches + clones CM's task-skills onto the host and returns
	// the dir to bind read-only into each worker. nil disables task-skills.
	SkillsResolver SkillsResolver

	// SessionSecrets records each session's CM-provisioned secrets (LLM key,
	// git-credentials bearer) so the host-side log-bridge redactor masks them
	// in bridged worker logs. nil disables per-session registration (bare test
	// servers leave it unset).
	SessionSecrets SessionSecretRegistry

	// Images lists the node's tagged images for GET /images. Nil disables the
	// endpoint (500 internal).
	Images webhookcore.ImageLister

	// ImageListFilters are the per-tag substring filters applied to GET
	// /images responses. The serve layer always supplies at least the family
	// default; an empty slice yields an empty list, never "everything".
	ImageListFilters []string

	// Chat carries the static per-process chat backend settings.
	Chat ChatConfig

	Hub *logbridge.Hub

	Replay *webhookcore.ReplayCache
	Dedup  *DedupCache

	Draining *atomic.Bool

	// KeepaliveInterval overrides the SSE heartbeat period. Zero uses the
	// package default; tests shrink it.
	KeepaliveInterval time.Duration

	// Metrics is the Prometheus bundle. Nil disables request instrumentation.
	Metrics *metrics.Metrics

	Logger *slog.Logger
}

// Server is the chat backend's HTTP surface. The embedded *webhookcore.Core
// provides the shared transport surface (HMAC auth, the drain gate, request
// metrics, the SSE /logs stream, and the health/readiness/images probes); the
// Server adds the chat-specific lifecycle handlers. It owns no goroutines
// beyond the per-session supervision goroutines spawned by the executor; the
// replay janitor lives in its owner.
type Server struct {
	*webhookcore.Core

	executor executor.Executor
	tracker  *executor.Tracker

	// sessionSecrets registers/unregisters per-session CM-provisioned secrets
	// (LLM key, git-credentials bearer) with the host-side log-bridge redactor.
	// nil in minimal test servers.
	sessionSecrets SessionSecretRegistry

	// chat config (populated from ChatConfig at NewServer time)
	image                     string
	mcpURL                    string
	skillsResolver            SkillsResolver
	chatRunDirBase            string
	memBytes                  int64
	pidsLimit                 int64
	maxConcurrent             int
	toolOutputMaxBytes        int
	compactionThreshold       float64
	compactionKeepRecentTurns int
	bashTimeoutMaxSeconds     int
	workerExtraEnv            map[string]string
	reasoningEffort           string
	caCertFile                string
	gitCredentialsURL         string

	dedup *DedupCache

	logger *slog.Logger

	// stdinMu serializes control-frame writes and stdin closes per session. The
	// executor documents Run.Stdin as single-writer; webhook handlers run on
	// independent HTTP goroutines, so a per-session mutex keeps frame bytes from
	// interleaving on the wire. Entries are reclaimed by DropSession once the
	// session's container exits; see that method's doc for why the earlier
	// retain-forever design is no longer needed.
	stdinMu sync.Map // map[string]*sync.Mutex

	// llmOverrideWarnOnce guards the "worker_extra_env overrides CM-provisioned
	// llm credentials" warning so it logs once per server process - a static
	// worker_extra_env produces the same override on every session, and
	// repeating it per request would spam a long-lived server's log.
	llmOverrideWarnOnce sync.Once
}

// NewServer wires a Server from its dependencies. It builds the transport Core
// first (which defaults the skew, replay cache, and draining flag when the
// caller leaves them nil) then wires the chat-specific fields. The dedup cache
// is defaulted here so a bare Config still yields a usable server (tests rely
// on this).
func NewServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	coreCfg := webhookcore.CoreConfig{
		APIKey:            cfg.APIKey,
		Skew:              cfg.Skew,
		Replay:            cfg.Replay,
		Draining:          cfg.Draining,
		KeepaliveInterval: cfg.KeepaliveInterval,
		Metrics:           cfg.Metrics,
		Logger:            logger,
		Hub:               cfg.Hub,
		LogsFilterParam:   "session_id",
		LogsFilterAttr:    "session_id",
		Tracker:           cfg.Tracker,
		MaxConcurrent:     cfg.Chat.MaxConcurrent,
		Images:            cfg.Images,
		ImageListFilters:  cfg.ImageListFilters,
	}

	dedup := cfg.Dedup
	if dedup == nil {
		dedup = NewDedupCache(10*time.Minute, 4096)
	}

	return &Server{
		Core:                      webhookcore.NewCore(coreCfg),
		executor:                  cfg.Executor,
		tracker:                   cfg.Tracker,
		sessionSecrets:            cfg.SessionSecrets,
		image:                     cfg.Chat.Image,
		mcpURL:                    cfg.Chat.MCPURL,
		skillsResolver:            cfg.SkillsResolver,
		chatRunDirBase:            cfg.Chat.ChatRunDirBase,
		memBytes:                  cfg.Chat.MemoryBytes,
		pidsLimit:                 cfg.Chat.PidsLimit,
		maxConcurrent:             cfg.Chat.MaxConcurrent,
		toolOutputMaxBytes:        cfg.Chat.ToolOutputMaxBytes,
		compactionThreshold:       cfg.Chat.CompactionThreshold,
		compactionKeepRecentTurns: cfg.Chat.CompactionKeepRecentTurns,
		bashTimeoutMaxSeconds:     cfg.Chat.BashTimeoutMaxSeconds,
		workerExtraEnv:            cfg.Chat.WorkerExtraEnv,
		reasoningEffort:           cfg.Chat.ReasoningEffort,
		caCertFile:                cfg.Chat.CACertFile,
		gitCredentialsURL:         cfg.Chat.GitCredentialsURL,
		dedup:                     dedup,
		logger:                    logger,
	}
}

// stdinLock returns the per-session mutex for sessionID, creating it on first
// use. Callers Lock/Unlock around frames.Write and Stdin.Close to honour
// Run.Stdin's single-writer contract.
func (s *Server) stdinLock(sessionID string) *sync.Mutex {
	v, _ := s.stdinMu.LoadOrStore(sessionID, &sync.Mutex{})

	return v.(*sync.Mutex)
}

// DropSession removes the per-session stdin mutex and forgets the session's
// CM-provisioned secrets (the LLM key, the git-credentials bearer - a session
// may register either, both, or neither) from the log-bridge redactor once the
// session's container has exited (wired into the executor OnExit hook). After
// the container is gone no writer can hold the lock, so the delete/recreate
// race the retained-entry design guarded against no longer applies, and both
// the mutex map and the redaction set stop growing without bound over the
// process lifetime.
func (s *Server) DropSession(sessionID string) {
	s.stdinMu.Delete(sessionID)

	// Forget the session's CM-provisioned secrets now the container has
	// exited: no more of its log lines can arrive, so they never need masking
	// again, and dropping them keeps the redaction set bounded over the
	// process lifetime. A single RemoveSessionKey call forgets every secret
	// registered under this session ID (see logbridge.RedactorRegistry).
	if s.sessionSecrets != nil {
		s.sessionSecrets.RemoveSessionKey(sessionID)
	}
}

// Routes returns the mux with every webhook route mounted. The mutating
// lifecycle routes are gated on drain; /logs, /images, /health, and /readyz
// stay reachable during shutdown so operators can read state. The transport
// middlewares and the health/readiness/images/logs handlers are promoted from
// the embedded Core.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /chat/start", s.RecordMetrics(s.Auth(s.DrainGate(s.handleChatStart))))
	mux.HandleFunc("POST /chat/end", s.RecordMetrics(s.Auth(s.handleChatEnd)))
	mux.HandleFunc("POST /message", s.RecordMetrics(s.Auth(s.DrainGate(s.handleMessage))))
	mux.HandleFunc("GET /logs", s.RecordMetrics(s.Auth(s.HandleLogs)))
	mux.HandleFunc("GET /images", s.RecordMetrics(s.Auth(s.HandleImages)))
	mux.HandleFunc("GET /health", s.RecordMetrics(s.HandleHealth))
	mux.HandleFunc("GET /readyz", s.RecordMetrics(s.HandleReadyz))

	return mux
}
