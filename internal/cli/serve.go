package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-chat/internal/config"
	"github.com/mhersson/contextmatrix-chat/internal/executor"
	"github.com/mhersson/contextmatrix-chat/internal/metrics"
	"github.com/mhersson/contextmatrix-chat/internal/taskskills"
	"github.com/mhersson/contextmatrix-chat/internal/webhook"
)

const (
	// httpShutdownTimeout bounds the graceful HTTP drain after draining flips.
	httpShutdownTimeout = 10 * time.Second
	// containerKillTimeout bounds each per-container kill during shutdown so one
	// slow daemon response cannot starve the rest.
	containerKillTimeout = 10 * time.Second
)

func newServeCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the chat backend: host ContextMatrix chat sessions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), configPath)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", defaultServeConfigPath(),
		"path to the service config file")

	return cmd
}

// defaultServeConfigPath resolves the XDG config path
// (~/.config/contextmatrix-chat/serve.yaml). A failure to resolve the user
// config dir falls back to the bare filename so LoadService still yields
// defaults+env.
func defaultServeConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "serve.yaml"
	}

	return filepath.Join(dir, "contextmatrix-chat", "serve.yaml")
}

func runServe(ctx context.Context, configPath string) error {
	cfg, err := config.LoadService(configPath)
	if err != nil {
		return fmt.Errorf("load service config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid service config: %w", err)
	}

	logger := newServeLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	mx := metrics.New()

	docker, err := executor.NewClient()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}

	tracker := executor.NewTracker(cfg.MaxConcurrent)
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.SessionID }, dropAdapter{mx: mx})
	bridge := logbridge.NewBridge(logbridge.BridgeConfig{Hub: hub})

	// The redactor registry is the single source of truth for the log-bridge
	// redaction set: every live session's CM-provisioned secrets (LLM key,
	// git-credentials bearer - registered at chat-start, forgotten on
	// container exit). Worker stderr and unparsable stdout are bridged to
	// /logs with only this redactor applied, so every live secret must be in
	// the union.
	redactorRegistry := logbridge.NewRedactorRegistry(bridge)

	var srv *webhook.Server

	teardownRunDir := chatExit(hub, cfg.ChatRunDir, logger)

	exec := executor.NewDockerExecutor(executor.Config{
		Docker:     docker,
		Tracker:    tracker,
		PullPolicy: cfg.ImagePullPolicy,
		OnLog: func(sessionID string, line []byte, stderr bool) {
			bridge.BridgeLine(logbridge.Key{SessionID: sessionID}, line, stderr)
		},
		OnExit: func(sessionID string, code int64) {
			teardownRunDir(sessionID, code)

			if srv != nil {
				srv.DropSession(sessionID)
			}
		},
		Logger:  logger,
		Metrics: mx,
	})

	// Force-remove any chat-labeled containers left by a previous process before
	// we start serving - a labeled container in a fresh process is an orphan.
	if err := exec.CleanupOrphans(ctx); err != nil {
		logger.Warn("orphan cleanup failed", "error", err)
	}

	var draining atomic.Bool

	replay := webhook.NewReplayCache(cfg.ReplaySkew, cfg.ReplayCacheSize)
	dedup := webhook.NewDedupCache(cfg.MessageDedupTTL, cfg.MessageDedupCacheSize)

	base := cfg.ContainerContextMatrixURL
	if base == "" {
		base = cfg.ContextMatrixURL
	}

	// Pre-retirement deployments staged local credentials at
	// <secrets_dir>/shared/env - in PAT mode a long-lived token. The refresher
	// that owned that file is gone; remove the residue best-effort so it does
	// not linger on a persistent secrets_dir.
	_ = os.RemoveAll(filepath.Join(cfg.SecretsDir, "shared"))

	// Task-skills resolver: fetches the {git_remote_url, ref} pointer from CM and
	// shallow-clones it once into a host cache dir that handleChatStart binds
	// read-only into each worker at /run/cm-skills. CM is the single source of
	// truth - chat carries no task-skills config. Uses cfg.ContextMatrixURL (the
	// host-reachable CM URL), not the container URL.
	skillsCache := filepath.Join(cfg.SecretsDir, "task-skills-cache")
	skillsResolver := taskskills.NewResolver(cfg.ContextMatrixURL, cfg.APIKey, skillsCache, logger)

	srv = webhook.NewServer(webhook.Config{
		APIKey:           cfg.APIKey,
		Skew:             cfg.ReplaySkew,
		Executor:         exec,
		Tracker:          tracker,
		SkillsResolver:   skillsResolver,
		SessionSecrets:   redactorRegistry,
		Images:           exec,
		ImageListFilters: cfg.ImageListFilters,
		Hub:              hub,
		Chat: webhook.ChatConfig{
			Image:                     cfg.BaseImage,
			MCPURL:                    composeMCPURL(base),
			ChatRunDirBase:            cfg.ChatRunDir,
			MemoryBytes:               cfg.ContainerMemoryBytes,
			PidsLimit:                 cfg.ContainerPidsLimit,
			MaxConcurrent:             cfg.MaxConcurrent,
			ToolOutputMaxBytes:        cfg.ToolOutputMaxBytes,
			CompactionThreshold:       cfg.Compaction.Threshold,
			CompactionKeepRecentTurns: cfg.Compaction.KeepRecentTurns,
			BashTimeoutMaxSeconds:     cfg.BashTimeoutMaxSeconds,
			WorkerExtraEnv:            cfg.WorkerExtraEnv,
			ReasoningEffort:           cfg.ReasoningEffort,
			CACertFile:                cfg.CACertFile,
			GitCredentialsURL:         composeGitCredentialsURL(base),
		},
		Replay:   replay,
		Dedup:    dedup,
		Draining: &draining,
		Logger:   logger,
		Metrics:  mx,
	})

	// The replay janitor sweeps expired entries on the cache's own interval.
	stopJanitor := replay.StartJanitor()
	defer stopJanitor()

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Unblock in-flight /logs SSE streams when Shutdown starts; otherwise
	// http.Server.Shutdown waits the full httpShutdownTimeout on a stream that
	// never goes idle. (Mirror this in contextmatrix-agent's handleLogs.)
	httpServer.RegisterOnShutdown(srv.CloseSSE)

	adminSrv := buildAdminServer(cfg, srv, mx, logger)

	stopGauge := startRunningContainersGauge(tracker, mx)
	defer stopGauge()

	serverErr := make(chan error, 1)

	go func() {
		logger.Info("chat service listening", "addr", httpServer.Addr)

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	if adminSrv != nil {
		go func() {
			logger.Info("admin server listening", "addr", adminSrv.Addr)

			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("admin server error", "error", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server error: %w", err)
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", "signal", sig.String())
	case <-ctx.Done():
		logger.Info("context cancelled, shutting down")
	}

	gracefulShutdown(httpServer, adminSrv, exec, tracker, &draining, logger)
	logger.Info("chat service stopped")

	return nil
}

// gracefulShutdown drains the HTTP listener and kills every tracked container.
// No status callback is made - the chat backend has no ContextMatrix reporter.
//  1. flip draining so /readyz returns 503 and mutating routes refuse new work
//  2. Shutdown the HTTP server with a bounded budget
//  3. Shutdown the admin server if enabled
//  4. for each tracked run: Kill the container (ignore ErrNotFound)
func gracefulShutdown(
	httpServer *http.Server,
	adminServer *http.Server,
	exec executor.Executor,
	tracker *executor.Tracker,
	draining *atomic.Bool,
	logger *slog.Logger,
) {
	draining.Store(true)
	logger.Info("draining: readyz now returns 503, mutating routes refuse new work")

	httpCtx, httpCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer httpCancel()

	if err := httpServer.Shutdown(httpCtx); err != nil {
		logger.Error("http server shutdown error", "error", err)
	}

	if adminServer != nil {
		adminCtx, adminCancel := context.WithTimeout(context.Background(), httpShutdownTimeout)

		if err := adminServer.Shutdown(adminCtx); err != nil {
			logger.Error("admin server shutdown error", "error", err)
		}

		adminCancel()
	}

	for _, run := range tracker.List() {
		logger.Info("killing container on shutdown", "session_id", run.SessionID)

		killCtx, killCancel := context.WithTimeout(context.Background(), containerKillTimeout)
		if err := exec.Kill(killCtx, run.SessionID); err != nil &&
			!errors.Is(err, executor.ErrNotFound) {
			logger.Warn("failed to kill container on shutdown",
				"session_id", run.SessionID, "error", err)
		}

		killCancel()
	}
}

// chatExit builds the executor OnExit hook. It publishes a terminal "system"
// log entry so /logs SSE subscribers see the container exit, then removes the
// per-session run directory (resume.jsonl) so it does not
// accumulate unbounded on the host.
func chatExit(hub *logbridge.Hub, chatRunDirBase string, logger *slog.Logger) func(sessionID string, exitCode int64) {
	return func(sessionID string, exitCode int64) {
		hub.Publish(protocol.LogEntry{
			Timestamp: time.Now(),
			SessionID: sessionID,
			Type:      "system",
			Content:   fmt.Sprintf("container exited (code %d)", exitCode),
		})

		runDir := filepath.Join(chatRunDirBase, sessionID)
		if err := os.RemoveAll(runDir); err != nil {
			logger.Warn("chatExit: failed to remove run dir",
				"session_id", sessionID, "dir", runDir, "error", err)
		}
	}
}

// composeMCPURL builds the full MCP endpoint URL the worker connects to:
// <base>/mcp, with any trailing slash on the base trimmed so we never emit a
// double slash.
func composeMCPURL(base string) string {
	return strings.TrimRight(base, "/") + "/mcp"
}

// composeGitCredentialsURL builds CM's worker git-credentials endpoint URL:
// <base>/api/worker/git-credentials, with any trailing slash on base trimmed.
// Forwarded to the worker as CM_GIT_CREDENTIALS_URL alongside the chat-start
// payload's GitCredentialsToken (protocol v0.5.2) so the worker can fetch
// fresh, per-repo git credentials on demand.
func composeGitCredentialsURL(base string) string {
	return strings.TrimRight(base, "/") + "/api/worker/git-credentials"
}

// newServeLogger builds a JSON slog logger at the level named by lvl
// (debug|info|warn|error; default info on an empty or unrecognised value).
func newServeLogger(lvl string) *slog.Logger {
	var level slog.Level

	switch strings.ToLower(lvl) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// dropAdapter bridges logbridge.DropObserver to the Prometheus broadcaster-drops
// counter without forcing logbridge to import Prometheus.
type dropAdapter struct{ mx *metrics.Metrics }

func (a dropAdapter) ObserveDrop() {
	a.mx.BroadcasterDropsTotal.Inc()
}

// buildAdminServer returns the loopback admin HTTP server serving Prometheus
// /metrics behind HMAC, or nil when admin_port is 0. Bound to 127.0.0.1 so the
// metrics surface is never exposed on a public interface.
func buildAdminServer(
	cfg *config.ServiceConfig,
	srv *webhook.Server,
	mx *metrics.Metrics,
	logger *slog.Logger,
) *http.Server {
	if cfg.AdminPort == 0 {
		logger.Info("admin endpoints disabled (admin_port=0)")

		return nil
	}

	mux := http.NewServeMux()
	metricsHandler := promhttp.HandlerFor(mx.Registry, promhttp.HandlerOpts{})
	mux.HandleFunc("GET /metrics", srv.AdminAuth(metricsHandler.ServeHTTP))

	logger.Info("admin endpoints registered", "port", cfg.AdminPort, "metrics_auth", "hmac")

	return &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.AdminPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// runningContainersGaugeInterval is the poll interval for the
// running-containers gauge.
const runningContainersGaugeInterval = 30 * time.Second

// startRunningContainersGauge polls tracker.Count() on a ticker and publishes
// it to the running-containers gauge. Returns an idempotent stop function.
func startRunningContainersGauge(tracker *executor.Tracker, mx *metrics.Metrics) func() {
	stop := make(chan struct{})

	go func() {
		ticker := time.NewTicker(runningContainersGaugeInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				mx.RunningContainers.Set(float64(tracker.Count()))
			}
		}
	}()

	var once sync.Once

	return func() { once.Do(func() { close(stop) }) }
}
