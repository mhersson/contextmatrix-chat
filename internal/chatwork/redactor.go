package chatwork

import (
	"bufio"
	"context"
	"os"
	"sync/atomic"
	"time"

	"github.com/mhersson/contextmatrix-harness/redact"
)

// redactorPollInterval bounds how quickly a worker-fetched git credential
// (see fetchedTokensPath) is picked up by the worker's redactor after the
// file it lives in changes. Cheap: one os.Stat call per tick on a small,
// local file.
const redactorPollInterval = 5 * time.Second

// redactorWatcher holds the live *redact.Redactor behind an atomic pointer so
// RedactToolOutput always masks the current secrets, even after the
// credential-helper/gh-wrapper subcommands fetch a fresh per-repo git token
// mid-session. Mirrors the gh wrapper's per-call fresh credential fetch.
type redactorWatcher struct {
	ptr                 atomic.Pointer[redact.Redactor]
	mcpKey              string
	llmKey              string
	gitCredentialsToken string
	pollInterval        time.Duration

	// fetchedPath is the writable scratch file (see fetchedTokensPath) the
	// credential-helper/gh-wrapper subcommands append each freshly-fetched git
	// token to. Empty disables this source entirely (no CM-provisioned git
	// credentials this session). fetchedLastMod is the mtime observed by the
	// most recent successful reload. Set synchronously in newRedactorWatcher
	// (before the watch goroutine starts) and thereafter only touched by the
	// single watch goroutine — never read or written concurrently, so no lock
	// is needed. Establishing it at construction time (rather than lazily on
	// watch's first tick) avoids a race against a file rewrite that lands
	// between "go w.watch(ctx)" being called and the goroutine actually
	// running its first os.Stat.
	fetchedPath    string
	fetchedLastMod time.Time
}

// newRedactorWatcher builds the initial redactor. mcpKey, llmKey, and
// gitCredentialsToken are captured once — CM_MCP_API_KEY, LLM_API_KEY, and
// CM_GIT_CREDENTIALS_TOKEN are static per-session values delivered via
// container env. fetchedPath, when non-empty, is polled for worker-fetched
// git tokens (see fetchedTokensPath) — a missing or unreadable fetchedPath is
// not an error, just "nothing fetched yet".
func newRedactorWatcher(mcpKey, llmKey, gitCredentialsToken, fetchedPath string) *redactorWatcher {
	w := &redactorWatcher{
		mcpKey:              mcpKey,
		llmKey:              llmKey,
		gitCredentialsToken: gitCredentialsToken,
		fetchedPath:         fetchedPath,
		pollInterval:        redactorPollInterval,
	}
	w.reload()

	return w
}

// reload stats fetchedPath FIRST to capture the pre-read mtime, then re-reads
// it and rebuilds the redactor, and only then assigns that pre-read mtime as
// the new fetchedLastMod baseline for watch's change detection. Stat-first
// closes a TOCTOU window: if a rewrite lands between the stat and the read,
// reload still picks up the NEW content (since the read happens after the
// rewrite) but stamps the OLD mtime, so the next tick sees the mtime has
// moved again and re-reloads — a redundant but idempotent no-op. The
// alternative order (read then stat) can stamp the NEW mtime while the
// redactor was built from the OLD content, silently missing a fetched token
// until the next one. A missing fetchedPath contributes nothing (not an
// error) — the common case when no credential has been fetched yet, or CM
// never provisioned git credentials this session.
func (w *redactorWatcher) reload() {
	all := []string{w.llmKey, w.mcpKey, w.gitCredentialsToken}

	var fetchedPreReadMod time.Time

	if w.fetchedPath != "" {
		if fi, err := os.Stat(w.fetchedPath); err == nil { //nolint:gosec // G703: w.fetchedPath is the code-fixed fetchedTokensPath constant, not user input
			fetchedPreReadMod = fi.ModTime()
		}

		if tokens, err := readLines(w.fetchedPath); err == nil {
			all = append(all, tokens...)
		}
		// A missing or unreadable fetchedPath contributes nothing; see doc above.
	}

	w.ptr.Store(redact.New(all))
	w.fetchedLastMod = fetchedPreReadMod
}

// readLines returns every non-empty line of path. Used for fetchedPath, which
// is a plain list of tokens (one per line), not a KEY=value file.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // G703: path is the code-fixed fetchedTokensPath constant, not user input
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	var lines []string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, line)
		}
	}

	return lines, sc.Err()
}

// Apply masks all currently-known secrets. Safe for concurrent use; matches
// the func(string) string shape harness.Config.RedactToolOutput expects.
func (w *redactorWatcher) Apply(s string) string {
	return w.ptr.Load().Apply(s)
}

// watch polls fetchedPath's mtime every pollInterval and reloads on change,
// starting from the baseline established by the constructor's initial reload.
// Returns when ctx is done. It is launched with Run's top-level context
// (process lifetime), not the per-epoch context, so it intentionally survives
// /clear and only stops on session shutdown.
func (w *redactorWatcher) watch(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.changed() {
				w.reload()
			}
		}
	}
}

// changed reports whether fetchedPath's mtime has moved since the last
// successful reload. A stat failure is not itself a change — reload's own
// readLines call handles a genuinely missing file gracefully.
func (w *redactorWatcher) changed() bool {
	if w.fetchedPath == "" {
		return false
	}

	fi, err := os.Stat(w.fetchedPath) //nolint:gosec // G703: code-fixed constant
	if err != nil {
		return false
	}

	return !fi.ModTime().Equal(w.fetchedLastMod)
}
