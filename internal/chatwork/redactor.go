package chatwork

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/mhersson/contextmatrix-chat/internal/secrets"
	"github.com/mhersson/contextmatrix-harness/redact"
)

// redactorPollInterval bounds how quickly a rotated token is picked up by the
// worker's redactor after the host rewrites the secrets env file. Cheap: a
// single os.Stat per tick on a bind-mounted file.
const redactorPollInterval = 5 * time.Second

// redactorWatcher holds the live *redact.Redactor behind an atomic pointer so
// RedactToolOutput always masks the current secrets, even after the host-side
// Refresher rotates the GitHub token and rewrites the secrets env file mid-
// session. Mirrors the credential helper's and gh wrapper's per-call fresh
// read of the same bind-mounted file.
type redactorWatcher struct {
	ptr          atomic.Pointer[redact.Redactor]
	path         string
	mcpKey       string
	llmKey       string
	pollInterval time.Duration
	// lastMod is the mtime observed by the most recent successful reload. Set
	// synchronously in newRedactorWatcher (before the watch goroutine starts)
	// and thereafter only touched by the single watch goroutine — never read
	// or written concurrently, so no lock is needed. Establishing it at
	// construction time (rather than lazily on watch's first tick) avoids a
	// race against a file rewrite that lands between "go w.watch(ctx)" being
	// called and the goroutine actually running its first os.Stat.
	lastMod time.Time
}

// newRedactorWatcher builds the initial redactor from path. mcpKey and llmKey
// are captured once — CM_MCP_API_KEY and the resolved LLM_API_KEY (env-first-
// then-file; see envOrSecret) are static per-session values, not part of the
// rotating secrets file. llmKey covers the case where a CM-provisioned
// llm_endpoint (protocol v0.5.0) delivers the key only via container env,
// never writing it to path — without this, that key would never enter the
// redaction set and could leak into tool output, events, or logs.
func newRedactorWatcher(path, mcpKey, llmKey string) (*redactorWatcher, error) {
	w := &redactorWatcher{path: path, mcpKey: mcpKey, llmKey: llmKey, pollInterval: redactorPollInterval}
	if err := w.reload(); err != nil {
		return nil, err
	}

	return w, nil
}

// reload stats path FIRST to capture the pre-read mtime, then re-reads path
// and rebuilds the redactor, and only then assigns that pre-read mtime as the
// new lastMod baseline for watch's change detection. Stat-first closes a
// TOCTOU window: if a rewrite lands between the stat and the read, reload
// still picks up the NEW content (since the read happens after the rewrite)
// but stamps the OLD mtime, so the next tick sees the mtime has moved again
// and re-reloads — a redundant but idempotent no-op. The alternative order
// (read then stat) can stamp the NEW mtime while the redactor was built from
// the OLD content, silently missing a rotation until the next one (~1h
// later). On a transient read error (e.g. the host is mid-rewrite) the
// previous redactor and baseline are both kept so the next tick retries.
func (w *redactorWatcher) reload() error {
	var preReadMod time.Time
	if fi, err := os.Stat(w.path); err == nil { //nolint:gosec // G703: w.path is the code-fixed secretsEnvPath constant, not user input
		preReadMod = fi.ModTime()
	}

	src, err := secrets.Open(w.path)
	if err != nil {
		return err
	}

	w.ptr.Store(redact.New([]string{src.Get("LLM_API_KEY"), w.llmKey, src.Get("CM_GIT_TOKEN"), w.mcpKey}))
	w.lastMod = preReadMod

	return nil
}

// Apply masks all currently-known secrets. Safe for concurrent use; matches
// the func(string) string shape harness.Config.RedactToolOutput expects.
func (w *redactorWatcher) Apply(s string) string {
	return w.ptr.Load().Apply(s)
}

// watch polls path's mtime every pollInterval and reloads on change, starting
// from the baseline established by the constructor's initial reload. Returns
// when ctx is done. It is launched with Run's top-level context (process
// lifetime), not the per-epoch context, so it intentionally survives /clear
// and only stops on session shutdown.
func (w *redactorWatcher) watch(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := os.Stat(w.path) //nolint:gosec // G703: w.path is the code-fixed secretsEnvPath constant, not user input
			if err != nil || fi.ModTime().Equal(w.lastMod) {
				continue
			}

			if err := w.reload(); err != nil {
				slog.Warn("redactor reload failed; keeping previous redactor", "error", err)
			}
		}
	}
}
