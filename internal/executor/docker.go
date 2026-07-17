package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	"github.com/mhersson/contextmatrix-chat/internal/metrics"
)

// Container labels. The chat label marks every container this executor owns so
// the boot-time orphan sweep can find them by filter.
const (
	labelChat    = "contextmatrix.chat"
	labelSession = "contextmatrix.session"
)

// scannerBufferMax bounds the per-line buffer of the stdout/stderr pump so a
// pathological container cannot pin the host heap with one unbounded line.
const scannerBufferMax = 1 << 20 // 1 MiB

// Image pull policies.
const (
	PullNever        = "never"
	PullIfNotPresent = "if-not-present"
	PullAlways       = "always"
)

// Sentinel errors callers match with errors.Is.
var (
	// ErrCapacity is returned by Launch when the tracker is already at its
	// concurrency limit. The created container is removed before returning.
	ErrCapacity = errors.New("executor: at capacity")
	// ErrNotFound is returned by Kill when no run is tracked for the key.
	ErrNotFound = errors.New("executor: container not found")
)

// LaunchSpec is the fully-resolved description of one container to launch. The
// caller has already applied any image override and assembled Env and Binds.
// The webhook handler populates the chat-specific env and mounts;
// no chat-specific values are hardcoded here.
type LaunchSpec struct {
	SessionID   string
	Image       string // image already applied by the caller
	Env         []string
	Binds       []string // raw Docker bind specs, e.g. "/host/dir:/container/dir:ro"
	MemoryBytes int64
	PidsLimit   int64

	// MCPURL is the CM MCP endpoint the worker connects to. Its hostname is
	// pinned into the container's /etc/hosts (see buildExtraHosts) so a name
	// that only resolves on the host stays reachable inside the container.
	MCPURL string

	// Cmd overrides the image entrypoint command. Used by integration tests to
	// run a deterministic command against a stock image; harmless in production
	// where it is left nil and the worker image's own entrypoint runs.
	Cmd []string
}

// Executor is the interface for container lifecycle backends. Implementations
// call Launch to register runs on the shared *Tracker (the run registry holding
// each run's ContainerID and attached Stdin). The serve/webhook layer depends on
// both the Executor interface and this shared Tracker for /message delivery and
// container-ID handoff.
type Executor interface {
	Launch(ctx context.Context, spec LaunchSpec) error
	Stop(ctx context.Context, sessionID string) error
	Kill(ctx context.Context, sessionID string) error
}

var containerNameRe = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// containerName builds a Docker-legal container name from sessionID.
// The session ID is lowercased and any character outside Docker's allowed set
// [a-zA-Z0-9_.-] is replaced with a dash.
func containerName(sessionID string) string {
	name := strings.ToLower("cm-chat-" + sessionID)

	return containerNameRe.ReplaceAllString(name, "-")
}

// containerConfig is the pure mapping from a LaunchSpec to the Docker create
// configs. It performs no I/O so it is fully unit-testable without a daemon.
func containerConfig(spec LaunchSpec) (*container.Config, *container.HostConfig) {
	labels := map[string]string{
		labelChat:    "true",
		labelSession: spec.SessionID,
	}

	cfg := &container.Config{
		Image:       spec.Image,
		Env:         spec.Env,
		Labels:      labels,
		Cmd:         spec.Cmd,
		User:        "1000:1000",
		OpenStdin:   true,
		AttachStdin: true,
		StdinOnce:   false,
		// Tty defaults to false; the stdcopy demux below requires a multiplexed
		// (non-TTY) stream.
	}

	pidsLimit := spec.PidsLimit

	host := &container.HostConfig{
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		Resources: container.Resources{
			Memory:    spec.MemoryBytes,
			PidsLimit: &pidsLimit,
		},
		Binds: spec.Binds,
	}

	return cfg, host
}

// DockerExecutor launches one container per chat session via the Docker SDK.
// It owns the tracker and the per-run supervision goroutines (output pump,
// wait + cleanup). Dependencies are injected; there is no global state.
type DockerExecutor struct {
	docker     client.APIClient
	tracker    *Tracker
	pullPolicy string

	// resolver resolves the MCP hostname into the container's ExtraHosts.
	// Defaulted to net.DefaultResolver; swappable in tests.
	resolver hostResolver

	onExit func(sessionID string, exitCode int64)
	onLog  func(sessionID string, line []byte, stderr bool)

	logger  *slog.Logger
	metrics *metrics.Metrics
}

// Config carries the DockerExecutor dependencies. onExit and onLog may be nil
// (the executor no-ops them) but the serve layer wires both.
type Config struct {
	Docker     client.APIClient
	Tracker    *Tracker
	PullPolicy string

	OnExit func(sessionID string, exitCode int64)
	OnLog  func(sessionID string, line []byte, stderr bool)

	Logger *slog.Logger
	// Metrics is the Prometheus bundle. Nil disables container-duration
	// observation.
	Metrics *metrics.Metrics
}

func NewDockerExecutor(cfg Config) *DockerExecutor {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &DockerExecutor{
		docker:     cfg.Docker,
		tracker:    cfg.Tracker,
		pullPolicy: cfg.PullPolicy,
		resolver:   net.DefaultResolver,
		onExit:     cfg.OnExit,
		onLog:      cfg.OnLog,
		logger:     logger,
		metrics:    cfg.Metrics,
	}
}

// NewClient builds a Docker API client from the environment with API version
// negotiation. Returned as the concrete *client.Client; consumers depend on the
// client.APIClient interface.
func NewClient() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("new docker client: %w", err)
	}

	return cli, nil
}

// Launch pulls (per policy), creates, attaches stdin+stdout+stderr, admits the
// run to the tracker, starts the container, and spawns the supervision
// goroutines. On any failure after create, the container is removed so nothing
// leaks. ErrCapacity is returned when the tracker is full.
func (e *DockerExecutor) Launch(ctx context.Context, spec LaunchSpec) error {
	log := e.logger.With("session_id", spec.SessionID)

	if err := e.pull(ctx, spec.Image, log); err != nil {
		return fmt.Errorf("pull image %q: %w", spec.Image, err)
	}

	cfg, host := containerConfig(spec)
	host.ExtraHosts = buildExtraHosts(e.resolver, spec.MCPURL, log)
	name := containerName(spec.SessionID)

	resp, err := e.docker.ContainerCreate(ctx, cfg, host, &network.NetworkingConfig{}, &ocispec.Platform{}, name)
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	// Attach BEFORE start so no early output is missed and stdin is ready for
	// /message frames the serve layer writes.
	attach, err := e.docker.ContainerAttach(ctx, resp.ID, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		e.removeContainer(resp.ID, log)

		return fmt.Errorf("attach container: %w", err)
	}

	run := &Run{
		ContainerID: resp.ID,
		SessionID:   spec.SessionID,
		StartedAt:   time.Now(),
		Stdin:       attach.Conn,
	}

	if !e.tracker.AddIfUnderLimit(run) {
		attach.Close()
		e.removeContainer(resp.ID, log)

		return ErrCapacity
	}

	if err := e.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		e.tracker.Remove(spec.SessionID)
		attach.Close()
		e.removeContainer(resp.ID, log)

		return fmt.Errorf("start container: %w", err)
	}

	go e.pump(spec.SessionID, attach.Reader, log)
	// waitAndCleanup deliberately runs on a detached context: the container's
	// supervision must outlive the request ctx that triggered Launch, otherwise
	// a returned webhook handler would cancel a still-running container's wait
	// and cleanup. Chat containers are long-lived; there is no per-container
	// deadline - the container runs until /chat/end closes stdin.
	//nolint:gosec // G118: detached ctx is intentional; container outlives the request
	go e.waitAndCleanup(spec.SessionID, resp.ID, run.StartedAt, attach, log)

	log.Info("container launched", "container_id", truncateID(resp.ID), "name", name)

	return nil
}

// pump demultiplexes the attach reader into stdout/stderr line streams, calling
// onLog for every completed line.
func (e *DockerExecutor) pump(sessionID string, r io.Reader, log *slog.Logger) {
	stdoutW := newLineWriter(func(line []byte) {
		if e.onLog != nil {
			e.onLog(sessionID, line, false)
		}
	})
	stderrW := newLineWriter(func(line []byte) {
		if e.onLog != nil {
			e.onLog(sessionID, line, true)
		}
	})

	if _, err := stdcopy.StdCopy(stdoutW, stderrW, r); err != nil &&
		!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		log.Debug("output pump ended", "error", err)
	}

	stdoutW.Flush()
	stderrW.Flush()
}

// waitAndCleanup blocks on ContainerWait with no deadline - chat containers are
// long-lived and run until /chat/end closes stdin so the work process exits
// naturally. It then force-removes the container, observes its duration by
// outcome, clears the tracker entry, closes the attach connection, and fires
// onExit.
func (e *DockerExecutor) waitAndCleanup(
	sessionID, containerID string,
	startedAt time.Time,
	attach types.HijackedResponse,
	log *slog.Logger,
) {
	defer attach.Close()

	exitCode := int64(0)

	waitCh, errCh := e.docker.ContainerWait(context.Background(), containerID, container.WaitConditionNotRunning)

	select {
	case res := <-waitCh:
		exitCode = res.StatusCode
		if res.Error != nil {
			log.Warn("container wait reported error", "error", res.Error.Message)
		}
	case err := <-errCh:
		log.Warn("container wait failed, killing", "error", err)
		e.kill(containerID, log)

		exitCode = -1
	}

	rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rmCancel()

	if err := e.docker.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true}); err != nil {
		log.Warn("failed to remove container", "container_id", truncateID(containerID), "error", err)
	}

	if e.metrics != nil {
		outcome := resolveOutcome(e.tracker.Reason(sessionID), exitCode)
		e.metrics.ContainerDuration.WithLabelValues(outcome).Observe(time.Since(startedAt).Seconds())
	}

	log.Info("container exited", "exit_code", exitCode)

	// Run onExit (chatExit's run-dir teardown) BEFORE releasing the tracker slot.
	// While the session is still tracked, a concurrent same-session /chat/start
	// stays 409-blocked (chat.go conflict check), so it cannot recreate the run
	// dir and have its freshly written primer/resume deleted by this cleanup.
	if e.onExit != nil {
		e.onExit(sessionID, exitCode)
	}

	e.tracker.Remove(sessionID)
}

// Stop gracefully stops the tracked container for sessionID (SIGTERM, then
// SIGKILL after the daemon grace period). It records the "ended" outcome so the
// container_duration metric distinguishes an operator-ended session from a crash
// or a kill. Removal and tracker cleanup are handled by waitAndCleanup once the
// container transitions to not-running. Returns ErrNotFound when no run is
// tracked. This is what actually ends a chat session: with StdinOnce=false,
// closing the attach connection does not EOF the worker, so the container must be
// stopped explicitly or it runs until /chat/end stops it (or serve shutdown
// kills it); there is no idle reaper.
func (e *DockerExecutor) Stop(ctx context.Context, sessionID string) error {
	run, ok := e.tracker.Get(sessionID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, sessionID)
	}

	e.tracker.SetReason(sessionID, metrics.OutcomeEnded)

	timeout := 10 // seconds grace before the daemon escalates to SIGKILL
	if err := e.docker.ContainerStop(ctx, run.ContainerID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stop container %s: %w", sessionID, err)
	}

	return nil
}

// Kill sends SIGKILL to the tracked container for sessionID. Removal is
// handled by waitAndCleanup. Returns ErrNotFound when no run is tracked.
func (e *DockerExecutor) Kill(ctx context.Context, sessionID string) error {
	run, ok := e.tracker.Get(sessionID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, sessionID)
	}

	e.tracker.SetReason(sessionID, metrics.OutcomeKilled)

	if err := e.docker.ContainerKill(ctx, run.ContainerID, "SIGKILL"); err != nil {
		return fmt.Errorf("kill container %s: %w", sessionID, err)
	}

	return nil
}

// CleanupOrphans force-removes every chat-labeled container found at boot.
// Anything matching is orphaned by definition - the tracker is empty in a fresh
// process, so a labeled container is a leftover from a previous run. This
// assumes exclusive ownership of contextmatrix.chat-labeled containers on the
// daemon; a second executor process sharing the Docker daemon would have its
// live containers swept.
func (e *DockerExecutor) CleanupOrphans(ctx context.Context) error {
	containers, err := e.docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelChat+"=true")),
	})
	if err != nil {
		return fmt.Errorf("list orphan containers: %w", err)
	}

	for _, ctr := range containers {
		log := e.logger.With(
			"container_id", truncateID(ctr.ID),
			"session_id", ctr.Labels[labelSession],
		)
		log.Info("removing orphan container")

		if err := e.docker.ContainerRemove(ctx, ctr.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Warn("failed to remove orphan container", "error", err)
		}
	}

	return nil
}

// ListImages returns the tagged images present in the node's local image
// store. Dangling images (no repo tags) are skipped. The executor-neutral
// webhookcore.ImageSummary is the wire-shape the webhook layer filters and
// maps.
func (e *DockerExecutor) ListImages(ctx context.Context) ([]webhookcore.ImageSummary, error) {
	summaries, err := e.docker.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("image list: %w", err)
	}

	return imageSummaries(summaries), nil
}

// imageSummaries maps Docker image summaries to webhookcore.ImageSummary,
// dropping dangling images and the "<none>:<none>" placeholder tag Docker
// reports for them.
func imageSummaries(in []image.Summary) []webhookcore.ImageSummary {
	out := make([]webhookcore.ImageSummary, 0, len(in))

	for _, s := range in {
		tags := make([]string, 0, len(s.RepoTags))

		for _, tag := range s.RepoTags {
			if tag != "<none>:<none>" {
				tags = append(tags, tag)
			}
		}

		if len(tags) == 0 {
			continue
		}

		out = append(out, webhookcore.ImageSummary{
			Tags:      tags,
			Digests:   s.RepoDigests,
			CreatedAt: s.Created,
			SizeBytes: s.Size,
		})
	}

	return out
}

// pull applies the executor's image pull policy: never skips, if-not-present
// pulls only when the image is absent locally, always pulls unconditionally.
func (e *DockerExecutor) pull(ctx context.Context, img string, log *slog.Logger) error {
	switch e.pullPolicy {
	case PullNever:
		return nil
	case PullIfNotPresent:
		if _, err := e.docker.ImageInspect(ctx, img); err == nil {
			log.Debug("image present locally, skipping pull", "image", img)

			return nil
		}
	case PullAlways:
		// Fall through to pull.
	default:
		return fmt.Errorf("unknown image pull policy %q", e.pullPolicy)
	}

	reader, err := e.docker.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image pull: %w", err)
	}

	defer func() { _ = reader.Close() }()

	// Drain the progress stream so the pull completes before create.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("drain image pull: %w", err)
	}

	return nil
}

// kill best-effort SIGKILLs a container by ID using a bounded detached context.
func (e *DockerExecutor) kill(containerID string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := e.docker.ContainerKill(ctx, containerID, "SIGKILL"); err != nil {
		log.Warn("failed to kill container", "container_id", truncateID(containerID), "error", err)
	}
}

// removeContainer force-removes a created-but-not-supervised container on a
// launch failure path, using a bounded detached context so a cancelled launch
// ctx cannot turn cleanup into a no-op.
func (e *DockerExecutor) removeContainer(containerID string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := e.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		log.Warn("failed to remove container after launch failure",
			"container_id", truncateID(containerID), "error", err)
	}
}

func truncateID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}

// lineWriter is an io.Writer that splits its input on newlines and invokes emit
// once per complete line (without the trailing newline). The final unterminated
// line is delivered by Flush. Per-line length is bounded by scannerBufferMax.
type lineWriter struct {
	emit func(line []byte)
	buf  bytes.Buffer
}

func newLineWriter(emit func(line []byte)) *lineWriter {
	return &lineWriter{emit: emit}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)

	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			w.appendBounded(p)

			break
		}

		w.appendBounded(p[:idx])
		w.flushLine()

		p = p[idx+1:]
	}

	return n, nil
}

// appendBounded appends to the line buffer, dropping bytes past the cap so one
// runaway line cannot grow the buffer without bound.
func (w *lineWriter) appendBounded(p []byte) {
	if room := scannerBufferMax - w.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}

		w.buf.Write(p)
	}
}

func (w *lineWriter) flushLine() {
	line := make([]byte, w.buf.Len())
	copy(line, w.buf.Bytes())
	w.buf.Reset()

	line = bytes.TrimRight(line, "\r")
	w.emit(line)
}

// Flush emits any buffered partial line. Safe to call when the buffer is empty.
func (w *lineWriter) Flush() {
	if w.buf.Len() > 0 {
		w.flushLine()
	}
}
