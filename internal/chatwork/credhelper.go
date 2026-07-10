package chatwork

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-chat/internal/secrets"
)

// ---- CM-provisioned per-repo credentials (protocol v0.5.2) -------------------
//
// The worker has no upfront git token at all, only a bearer for CM's
// GET /api/worker/git-credentials?host=&path= endpoint, which mints the
// correct credential for whichever repo git is about to touch — the only
// design that works for a cross-project, long-lived chat session.

// gitCredentialHelperV2ScriptName is the credential-helper script's filename
// under os.TempDir() (a writable path in the container).
const gitCredentialHelperV2ScriptName = "cm-git-credential-helper-v2.sh" //nolint:gosec // path, not a credential

// ConfigureGitCredentialHelperV2 writes a credential-helper script to
// os.TempDir() that execs selfPath's hidden "git-credential" subcommand, and
// registers it as the GLOBAL (unscoped) git credential helper with
// credential.useHttpPath=true. Unscoped is correct here: CM resolves the
// right per-project credential from the (host, path) the subcommand forwards
// on every call, so a provisioned session is multi-host by construction;
// scoping to one host would silently break every other host the session ever
// touches.
// useHttpPath=true is required so git includes the repo path in what it hands
// the helper — without it CM cannot tell which project's credential to mint.
// selfPath is never embedded with a secret: the script only names the binary
// to exec; the subcommand itself resolves the bearer from the staged config
// file (see gitCredentialsConfigPath), never from an argument.
func ConfigureGitCredentialHelperV2(ctx context.Context, selfPath string) error {
	scriptPath := filepath.Join(os.TempDir(), gitCredentialHelperV2ScriptName)

	script := fmt.Sprintf("#!/bin/sh\nexec '%s' git-credential \"$@\"\n", selfPath)

	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return fmt.Errorf("write credential helper v2 script: %w", err)
	}

	if err := exec.CommandContext(ctx, "git", "config", "--global", "credential.helper", scriptPath).Run(); err != nil {
		return fmt.Errorf("git config credential helper (global): %w", err)
	}

	if err := exec.CommandContext(ctx, "git", "config", "--global", "credential.useHttpPath", "true").Run(); err != nil {
		return fmt.Errorf("git config useHttpPath (global): %w", err)
	}

	return nil
}

// gitCredentialsConfigPath is the writable 0600 scratch file Run() stages at
// boot (with the container's full, unscrubbed environment) holding
// CM_GIT_CREDENTIALS_URL/CM_GIT_CREDENTIALS_TOKEN. RunGitCredentialHelper and
// RunGHWrapper read ONLY this file for those two values — never
// os.Getenv — because git/gh are invoked by the model through the harness
// bash tool, which execs with a SCRUBBED environment (tools.ScrubbedEnv) that
// strips everything outside a small allowlist (PATH, HOME, USER, LANG,
// LC_ALL, TMPDIR, TERM). CM_GIT_CREDENTIALS_TOKEN/URL would be invisible to a
// subcommand reading its own env from that lineage. The file lives in
// os.TempDir(), which resolves identically under a scrubbed environment too
// (TMPDIR is on the allowlist) — the same reasoning that already puts the
// credential-helper SCRIPT itself there: a fixed path beats inherited env.
func gitCredentialsConfigPath() string {
	return filepath.Join(os.TempDir(), "cm-git-credentials-config")
}

// fetchedTokensPath is a writable scratch file that RunGitCredentialHelper
// and RunGHWrapper append every freshly-fetched git token to. The
// long-running work process's redactorWatcher (see redactor.go) polls it so
// worker-fetched tokens enter the in-worker redaction set even though they
// are minted by a short-lived subcommand process that shares no memory with
// Run() — best-effort, on a multi-second poll cadence.
func fetchedTokensPath() string {
	return filepath.Join(os.TempDir(), "cm-fetched-git-tokens")
}

// recordFetchedToken appends token as a new line to fetchedTokensPath,
// creating the file if absent. A zero-length token is a no-op.
func recordFetchedToken(token string) error {
	if token == "" {
		return nil
	}

	f, err := os.OpenFile(fetchedTokensPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open fetched-tokens file: %w", err)
	}

	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(token + "\n"); err != nil {
		return fmt.Errorf("append fetched token: %w", err)
	}

	return nil
}

// recordFetchedTokenBestEffort appends token to fetchedTokensPath, logging
// (not failing) on error: a write failure here must not fail the credential
// fetch the model's git/gh operation is waiting on — the operation already has
// what it needs. Losing in-worker redaction coverage for this one token is the
// accepted downside of a best-effort, poll-based mechanism (see
// fetchedTokensPath's doc).
func recordFetchedTokenBestEffort(token string) {
	if err := recordFetchedToken(token); err != nil {
		fmt.Fprintln(os.Stderr, "cm-git-credential: record fetched token for redaction failed:", err)
	}
}

// gitCredentialRequestTimeout bounds each fetch to CM's worker
// git-credentials endpoint.
const gitCredentialRequestTimeout = 15 * time.Second

// gitCredentialDefaultMaxAttempts and gitCredentialDefaultRetryDelay bound the
// retry on a 409 (session not yet live) right after container boot: the chat
// session flips active around container start, and the worker's very first
// fetch (racing the initial clone) can land just before that.
const (
	gitCredentialDefaultMaxAttempts = 3
	gitCredentialDefaultRetryDelay  = 500 * time.Millisecond
)

// errGitCredentialSessionNotLive marks CM's 409 response — the only status
// code gitCredentialClient.fetch retries (400, 401, 404, 500, 502, etc. all
// fail immediately). The body is NOT inspected, so every 409 is treated as
// this same retryable case, including CM's other use of 409: a broken project
// credential binding, which retrying can never fix. That case still retries
// the bounded gitCredentialDefaultMaxAttempts (~1s total delay) before
// failing — acceptable today; distinguishing it by response body to fail
// immediately is a follow-up, not implemented here.
var errGitCredentialSessionNotLive = errors.New("chat session is not yet live")

// gitCredential is the decode target for CM's worker git-credentials response.
type gitCredential struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

// gitCredentialClient fetches per-repo git credentials from CM's worker
// endpoint. Timing is overridable (tests shrink retryDelay so the bounded
// retry does not slow the suite).
type gitCredentialClient struct {
	http           *http.Client
	credentialsURL string
	bearer         string
	maxAttempts    int
	retryDelay     time.Duration
}

// newGitCredentialClient builds a client with the default timeout and retry
// bounds.
func newGitCredentialClient(credentialsURL, bearer string) *gitCredentialClient {
	return &gitCredentialClient{
		http:           &http.Client{Timeout: gitCredentialRequestTimeout},
		credentialsURL: credentialsURL,
		bearer:         bearer,
		maxAttempts:    gitCredentialDefaultMaxAttempts,
		retryDelay:     gitCredentialDefaultRetryDelay,
	}
}

// fetch mints a fresh credential for (host, path), retrying up to
// c.maxAttempts times — bounded to errGitCredentialSessionNotLive only — with
// c.retryDelay between attempts. host and path may be empty (an instance-wide
// credential fetch, e.g. the gh wrapper with no origin remote).
func (c *gitCredentialClient) fetch(ctx context.Context, host, path string) (gitCredential, error) {
	var (
		cred gitCredential
		err  error
	)

	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		cred, err = c.doFetch(ctx, host, path)
		if err == nil || !errors.Is(err, errGitCredentialSessionNotLive) || attempt == c.maxAttempts {
			return cred, err
		}

		select {
		case <-ctx.Done():
			return gitCredential{}, ctx.Err()
		case <-time.After(c.retryDelay):
		}
	}

	return cred, err
}

// doFetch performs one signed-bearer GET against CM's worker git-credentials
// endpoint and decodes the response.
func (c *gitCredentialClient) doFetch(ctx context.Context, host, path string) (gitCredential, error) {
	u, err := url.Parse(c.credentialsURL)
	if err != nil {
		return gitCredential{}, fmt.Errorf("parse git-credentials url: %w", err)
	}

	q := u.Query()
	q.Set("host", host)
	q.Set("path", path)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return gitCredential{}, fmt.Errorf("create git-credentials request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.bearer)

	resp, err := c.http.Do(req) //nolint:gosec // credentialsURL is operator/CM config, not user input
	if err != nil {
		return gitCredential{}, fmt.Errorf("fetch git-credentials: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusConflict {
		return gitCredential{}, errGitCredentialSessionNotLive
	}

	if resp.StatusCode >= 400 {
		return gitCredential{}, fmt.Errorf("git-credentials returned %d", resp.StatusCode)
	}

	var gc gitCredential
	if err := json.Unmarshal(body, &gc); err != nil {
		return gitCredential{}, fmt.Errorf("parse git-credentials response: %w", err)
	}

	if gc.Token == "" {
		return gitCredential{}, fmt.Errorf("git-credentials response carried no token")
	}

	return gc, nil
}

// GitCredentialGet implements git's credential "get" operation against CM's
// worker git-credentials endpoint: it reads git's key=value stdin protocol
// from r (only protocol/host/path are used), fetches a fresh credential from
// credentialsURL, and writes username=/password= lines to w. onFetched, when
// non-nil, is called with the minted token before it is written — the caller
// uses it to register the token for in-worker redaction (see
// recordFetchedTokenBestEffort). A fetch failure writes nothing to w and
// returns an error; the caller (RunGitCredentialHelper, via the hidden CLI
// subcommand) surfaces it as a single stderr line — git then fails the git
// operation itself with its own "could not read Username" error, which is
// what actually reaches the model/user.
func GitCredentialGet(
	ctx context.Context,
	r io.Reader,
	w io.Writer,
	credentialsURL, bearer string,
	onFetched func(token string),
) error {
	return gitCredentialGetWithClient(ctx, r, w, newGitCredentialClient(credentialsURL, bearer), onFetched)
}

// gitCredentialGetWithClient is GitCredentialGet's implementation over an
// explicit client, so tests can inject fast retry timing.
func gitCredentialGetWithClient(
	ctx context.Context,
	r io.Reader,
	w io.Writer,
	client *gitCredentialClient,
	onFetched func(token string),
) error {
	host, path := parseGitCredentialInput(r)

	cred, err := client.fetch(ctx, host, path)
	if err != nil {
		return fmt.Errorf("fetch git credential (host=%q path=%q): %w", host, path, err)
	}

	if onFetched != nil {
		onFetched(cred.Token)
	}

	fmt.Fprintf(w, "username=%s\n", cred.Username)
	fmt.Fprintf(w, "password=%s\n", cred.Token)

	return nil
}

// parseGitCredentialInput reads git's credential-helper key=value protocol
// from r (terminated by EOF or a blank line) and extracts protocol/host/path;
// every other key (capability[]=, wwwauth[]=, username=/password= on a
// store/erase call) is ignored.
func parseGitCredentialInput(r io.Reader) (host, path string) {
	sc := bufio.NewScanner(r)

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		switch key {
		case "host":
			host = val
		case "path":
			path = val
		}
	}

	return host, path
}

// RunGitCredentialHelper is the entry point for the hidden "git-credential"
// CLI subcommand (see cli.newGitCredentialCmd). op is git's requested
// operation (get/store/erase, from argv); only "get" does anything — CM mints
// a fresh credential on every fetch, so there is nothing to persist ("store")
// or invalidate ("erase") locally. Returns an error only for "get"; the
// caller surfaces it as a single stderr line (git then fails the git
// operation itself with its own auth error).
func RunGitCredentialHelper(ctx context.Context, op string, r io.Reader, w io.Writer) error {
	if op != "get" {
		return nil
	}

	src, err := secrets.Open(gitCredentialsConfigPath())
	if err != nil {
		return fmt.Errorf("read git-credentials config: %w", err)
	}

	credentialsURL := src.Get("CM_GIT_CREDENTIALS_URL")
	bearer := src.Get("CM_GIT_CREDENTIALS_TOKEN")

	if credentialsURL == "" || bearer == "" {
		return fmt.Errorf("git-credential helper invoked but no provisioned git-credentials config is staged")
	}

	return GitCredentialGet(ctx, r, w, credentialsURL, bearer, recordFetchedTokenBestEffort)
}
