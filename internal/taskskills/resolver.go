// Package taskskills resolves ContextMatrix's task-skills onto the chat serve
// host: it fetches a {git_remote_url, ref} pointer from CM and shallow-clones it
// once into a cache dir the executor binds read-only into worker containers. The
// chat service carries no task-skills config — CM is the single source of truth.
package taskskills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

const requestTimeout = 15 * time.Second

// tokenGen mints a GitHub token for the clone. secrets.TokenGenerator satisfies it.
type tokenGen interface {
	GenerateToken(ctx context.Context) (token string, expiresAt time.Time, err error)
}

// Resolver fetches the task-skills pointer from CM and clones it once, caching
// the resolved host dir for the process. Safe for concurrent use.
type Resolver struct {
	cmURL    string
	apiKey   string
	cacheDir string
	gen      tokenGen
	http     *http.Client
	logger   *slog.Logger

	// cloner is the clone implementation; overridable in tests. Production uses
	// gitClone (a shallow git fetch+checkout with the minted token).
	cloner func(ctx context.Context, gitURL, ref, dest, token string) error

	mu       sync.Mutex
	resolved string // cached host dir once a clone has succeeded

	// selfMintWarnOnce guards the "CM did not provision a task-skills clone
	// token" fallback warning so it logs once per Resolver (a serve process
	// constructs exactly one), not once per self-mint attempt. A failed clone
	// is not cached, so a long-lived process can genuinely re-enter the
	// self-mint path across many chat/start requests.
	selfMintWarnOnce sync.Once
}

// NewResolver builds a Resolver. cmURL is ContextMatrix's base URL, apiKey the
// chat backend HMAC key, cacheDir a host directory the clone lands in (and the
// executor binds), gen the GitHub token source.
func NewResolver(cmURL, apiKey, cacheDir string, gen tokenGen, logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}

	r := &Resolver{
		cmURL:    strings.TrimRight(cmURL, "/"),
		apiKey:   apiKey,
		cacheDir: cacheDir,
		gen:      gen,
		http:     &http.Client{Timeout: requestTimeout},
		logger:   logger,
	}
	r.cloner = r.gitClone

	return r
}

// Resolve returns the host dir holding the task-skills, cloning on first use and
// caching the result. An error means "no skills this run"; failures are not
// cached, so the next start retries.
func (r *Resolver) Resolve(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.resolved != "" {
		return r.resolved, nil
	}

	p, err := r.fetchPointer(ctx)
	if err != nil {
		return "", err
	}

	if p.GitRemoteURL == "" {
		return "", fmt.Errorf("task-skills source has no git_remote_url")
	}

	// Prefer the CM-provisioned clone token when present. Absent means a CM
	// version that predates provisioned task-skills clone tokens: fall back to
	// self-minting via the local GitHub token source, plus a once-per-process
	// deprecation warning (compat-window rule).
	token := p.Token
	if token == "" {
		if r.gen == nil {
			return "", fmt.Errorf("CM did not provision a task-skills clone token and no local github config exists")
		}

		var terr error

		token, _, terr = r.gen.GenerateToken(ctx)
		if terr != nil {
			return "", fmt.Errorf("mint skills clone token: %w", terr)
		}

		r.selfMintWarnOnce.Do(func() {
			r.logger.Warn("CM did not provision a task-skills clone token; self-minting via local github config is deprecated")
		})
	}

	dest := filepath.Join(r.cacheDir, "task-skills")
	if err := os.RemoveAll(dest); err != nil {
		return "", fmt.Errorf("clear skills cache: %w", err)
	}

	if err := r.cloner(ctx, p.GitRemoteURL, p.Ref, dest, token); err != nil {
		return "", fmt.Errorf("clone task-skills: %w", err)
	}

	r.resolved = dest

	return dest, nil
}

// pointer is this resolver's local decode target for the task-skills-source
// response. There is no shared contextmatrix-protocol DTO for this endpoint,
// so the wire contract (including the token/token_expires_at fields below) is
// mirrored here rather than imported.
type pointer struct {
	GitRemoteURL string `json:"git_remote_url"`
	Ref          string `json:"ref"`
	// Token is a CM-provisioned short-lived token for cloning GitRemoteURL.
	// When present, Resolve clones with it directly instead of self-minting
	// via gen. Absent means a CM version that predates provisioned task-skills
	// clone tokens (the compat-window fallback applies).
	Token string `json:"token,omitempty"`
	// TokenExpiresAt is the RFC3339 expiry of Token. Decoded for wire-contract
	// fidelity; currently unused — Resolve caches the clone for the process
	// lifetime and never re-checks token freshness after a successful clone.
	TokenExpiresAt string `json:"token_expires_at,omitempty"`
}

// fetchPointer does a signed GET to /api/chat/task-skills-source.
func (r *Resolver) fetchPointer(ctx context.Context) (pointer, error) {
	const path = "/api/chat/task-skills-source"

	uri, perr := requestURI(r.cmURL + path)
	if perr != nil {
		return pointer{}, perr
	}

	sig, ts := protocol.SignRequestHeaders(r.apiKey, http.MethodGet, uri, nil)

	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, r.cmURL+path, nil)
	if rerr != nil {
		return pointer{}, fmt.Errorf("create task-skills-source request: %w", rerr)
	}

	req.Header.Set(protocol.SignatureHeader, sig)
	req.Header.Set(protocol.TimestampHeader, ts)

	resp, derr := r.http.Do(req) //nolint:gosec // cmURL is operator config
	if derr != nil {
		return pointer{}, fmt.Errorf("fetch task-skills-source: %w", derr)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return pointer{}, fmt.Errorf("task-skills-source returned %d", resp.StatusCode)
	}

	var p pointer
	if uerr := json.Unmarshal(body, &p); uerr != nil {
		return pointer{}, fmt.Errorf("parse task-skills-source: %w", uerr)
	}

	return p, nil
}

// gitClone shallow-fetches ref (a SHA, branch, or tag) into dest using the
// token as an http.extraheader (mirrors the worker's credEnv pattern). When ref
// is empty it fetches the remote's default branch (HEAD).
func (r *Resolver) gitClone(ctx context.Context, gitURL, ref, dest, token string) error {
	// Reject dash-leading args before any exec: git interprets them as flags.
	// Both gitURL and ref are CM-sourced config that must not be option strings.
	if strings.HasPrefix(gitURL, "-") || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("git clone: url and ref must not begin with '-' (got url=%q ref=%q)", gitURL, ref)
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("mkdir skills dest: %w", err)
	}

	fetchRef := ref
	if fetchRef == "" {
		fetchRef = "HEAD"
	}

	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", gitURL},
		// '--' separates options from the ref spec so a ref that looks like a
		// flag is passed as a positional argument. Mirrors run.go's clone guard.
		{"fetch", "--depth", "1", "origin", "--", fetchRef},
		{"checkout", "-q", "FETCH_HEAD"},
	}

	for _, args := range steps {
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are code-fixed; gitURL/ref are CM-sourced config
		cmd.Dir = dest
		cmd.Env = gitAuthEnv(token)

		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
	}

	return nil
}

func requestURI(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", raw, err)
	}

	if u.Path == "" {
		u.Path = "/"
	}

	return u.RequestURI(), nil
}
