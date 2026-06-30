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

	githubauth "github.com/mhersson/contextmatrix-githubauth"
	"github.com/mhersson/contextmatrix-harness/redact"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-chat/internal/config"
	"github.com/mhersson/contextmatrix-chat/internal/executor"
	"github.com/mhersson/contextmatrix-chat/internal/logbridge"
	"github.com/mhersson/contextmatrix-chat/internal/metrics"
	"github.com/mhersson/contextmatrix-chat/internal/secrets"
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

	provider, err := newTokenProvider(cfg.GitHub)
	if err != nil {
		return err
	}

	logger.Info("github token provider initialized", "auth_mode", cfg.GitHub.AuthMode)

	// Secrets refresher: writes <secrets_dir>/shared/env, rewritten ahead of each
	// token expiry. The worker reads /run/cm-secrets/env, which is <shared> bound
	// read-only into the container.
	sharedDir := filepath.Join(cfg.SecretsDir, "shared")
	envFile := filepath.Join(sharedDir, "env")
	refresher := secrets.NewRefresher(envFile, secrets.EndpointSecrets{
		APIKey:  cfg.LLMEndpoint.APIKey,
		BaseURL: cfg.LLMEndpoint.BaseURL,
		Type:    cfg.LLMEndpoint.Type,
	}, provider, logger)

	refreshCtx, refreshCancel := context.WithCancel(context.Background())
	defer refreshCancel()

	go func() {
		if err := refresher.Run(refreshCtx); err != nil {
			logger.Error("secrets refresher stopped with error", "error", err)
		}
	}()

	docker, err := executor.NewClient()
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}

	tracker := executor.NewTracker(cfg.MaxConcurrent)
	hub := logbridge.NewHubWithDropObserver(dropAdapter{mx: mx})
	redactor := redact.New([]string{cfg.LLMEndpoint.APIKey})
	bridge := logbridge.New(hub, redactor)

	exec := executor.NewDockerExecutor(executor.Config{
		Docker:     docker,
		Tracker:    tracker,
		PullPolicy: cfg.ImagePullPolicy,
		OnLog:      bridge.BridgeLine,
		OnExit:     chatExit(hub, cfg.ChatRunDir, logger),
		Logger:     logger,
		Metrics:    mx,
	})

	// Force-remove any chat-labeled containers left by a previous process before
	// we start serving — a labeled container in a fresh process is an orphan.
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

	// Task-skills resolver: fetches the {git_remote_url, ref} pointer from CM and
	// shallow-clones it once into a host cache dir that handleChatStart binds
	// read-only into each worker at /run/cm-skills. CM is the single source of
	// truth — chat carries no task-skills config. Uses cfg.ContextMatrixURL (the
	// host-reachable CM URL), not the container URL.
	skillsCache := filepath.Join(cfg.SecretsDir, "task-skills-cache")
	skillsResolver := taskskills.NewResolver(cfg.ContextMatrixURL, cfg.APIKey, skillsCache, provider, logger)

	srv := webhook.NewServer(webhook.Config{
		APIKey:         cfg.APIKey,
		Skew:           cfg.ReplaySkew,
		Executor:       exec,
		Tracker:        tracker,
		SkillsResolver: skillsResolver,
		Hub:            hub,
		Chat: webhook.ChatConfig{
			Image:                     cfg.BaseImage,
			MCPURL:                    composeMCPURL(base),
			SecretsHostDir:            sharedDir,
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

	adminSrv := buildAdminServer(cfg, srv, mx, logger)

	stopGauge := startRunningContainersGauge(tracker, mx, logger, 30*time.Second)
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
	refreshCancel()
	logger.Info("chat service stopped")

	return nil
}

// gracefulShutdown drains the HTTP listener and kills every tracked container.
// No status callback is made — the chat backend has no ContextMatrix reporter.
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
// per-session run directory (resume.jsonl, primer.txt) so they do not
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

// newTokenProvider selects the GitHub token provider per auth_mode, mirroring
// the runner/agent: app -> NewAppProvider, pat -> NewPATProvider.
func newTokenProvider(gh config.GitHubConfig) (secrets.TokenGenerator, error) {
	switch gh.AuthMode {
	case "app":
		p, err := githubauth.NewAppProvider(
			gh.App.AppID,
			gh.App.InstallationID,
			gh.App.PrivateKeyPath,
			githubauth.WithAPIBaseURL(gh.APIBaseURL),
		)
		if err != nil {
			return nil, fmt.Errorf("construct github app provider: %w", err)
		}

		return p, nil
	case "pat":
		p, err := githubauth.NewPATProvider(gh.PAT.Token)
		if err != nil {
			return nil, fmt.Errorf("construct github pat provider: %w", err)
		}

		return p, nil
	default:
		return nil, fmt.Errorf("unknown github auth_mode %q", gh.AuthMode)
	}
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
	if a.mx == nil {
		return
	}

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

// startRunningContainersGauge polls tracker.Count() on a ticker and publishes
// it to the running-containers gauge. Returns an idempotent stop function. A
// non-positive interval disables the poller.
func startRunningContainersGauge(
	tracker *executor.Tracker,
	mx *metrics.Metrics,
	logger *slog.Logger,
	interval time.Duration,
) func() {
	if interval <= 0 {
		logger.Warn("running-containers gauge disabled: non-positive interval", "interval", interval)

		return func() {}
	}

	stop := make(chan struct{})

	go func() {
		ticker := time.NewTicker(interval)
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
