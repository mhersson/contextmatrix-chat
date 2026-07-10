package chatwork

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mhersson/contextmatrix-chat/internal/secrets"
)

// realGHPath is the apt-installed gh binary in the worker image
// (docker/Dockerfile.worker installs it from GitHub's apt source). The shim
// execs it by absolute path so it never recurses into itself.
const realGHPath = "/usr/bin/gh"

// ---- CM-provisioned per-repo credentials (protocol v0.5.2) -------------------

// installGHWrapperV2 writes a `gh` shim to a fresh dir and returns that dir
// for prepending to the bash tool's PATH. The shim delegates the per-call
// credential fetch to selfPath's hidden "gh-wrapper" subcommand, since
// provisioned mode needs an HTTP GET + JSON decode a POSIX shell cannot do
// without assuming curl/jq are present in the image. gh reads GH_TOKEN from
// the env and has no hook into a rotating credential source; git already
// rotates via the credential helper — hence the shim.
func installGHWrapperV2(selfPath string) (string, error) {
	dir, err := os.MkdirTemp("", "cm-gh-bin-v2-")
	if err != nil {
		return "", fmt.Errorf("create gh wrapper v2 dir: %w", err)
	}

	script := fmt.Sprintf("#!/bin/sh\nexec '%s' gh-wrapper \"$@\"\n", selfPath)

	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("write gh wrapper v2: %w", err)
	}

	return dir, nil
}

// execFunc is syscall.Exec by default — it replaces the current process image
// with the real gh binary so the wrapper adds no extra process layer and gh
// inherits stdio directly. Tests override it to observe the final argv/env
// without actually replacing the test binary's process image.
var execFunc = syscall.Exec

// RunGHWrapper is the entry point for the hidden "gh-wrapper" CLI subcommand
// (see cli.newGHWrapperCmd). It resolves the bearer and CM endpoint from the
// staged config file (never os.Getenv — see gitCredentialsConfigPath's doc:
// gh is invoked by the model through the harness bash tool, which execs with
// a scrubbed environment), derives the target (host, path) from dir's git
// "origin" remote (empty dir means the ambient working directory), fetches a
// fresh credential, and execs the real gh binary with args, replacing this
// process.
//
// The exec is decoupled from the fetch: a credential-fetch failure (config
// unreadable, no provisioned config staged, or the HTTP fetch itself
// failing) logs one concise stderr note — never the token, bearer, or URL
// query values — and gh STILL execs, with the ambient environment and no
// injected token. Exec'ing gh best-effort keeps repo-less gh calls
// (gh --version, gh auth status, gh api /user, gh repo list, gh repo create)
// working even when a credential can't be minted.
func RunGHWrapper(ctx context.Context, dir string, args []string) error {
	env := os.Environ()

	fetchedEnv, err := ghCredentialEnv(ctx, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh-wrapper: git credential fetch failed; running gh without an injected token")
	} else {
		env = fetchedEnv
	}

	return execFunc(realGHPath, append([]string{realGHPath}, args...), env) //nolint:gosec // G204: realGHPath is a code-fixed constant; args are the model's own gh invocation, passed through unmodified
}

// ghCredentialEnv resolves the bearer and CM endpoint from the staged config
// file, derives the target (host, path) from dir's git "origin" remote
// (empty dir means the ambient working directory), fetches a fresh
// credential, and returns the environment gh should exec with: ambient
// os.Environ() plus GH_TOKEN/GH_ENTERPRISE_TOKEN (+ GH_HOST for a
// non-github.com host — gh ignores GH_TOKEN on GitHub Enterprise Server). Any
// failure returns a nil env and a non-nil error; RunGHWrapper treats that as
// a signal to exec gh anyway, with no injected token.
func ghCredentialEnv(ctx context.Context, dir string) ([]string, error) {
	src, err := secrets.Open(gitCredentialsConfigPath())
	if err != nil {
		return nil, fmt.Errorf("read git-credentials config: %w", err)
	}

	credentialsURL := src.Get("CM_GIT_CREDENTIALS_URL")
	bearer := src.Get("CM_GIT_CREDENTIALS_TOKEN")

	if credentialsURL == "" || bearer == "" {
		return nil, fmt.Errorf("gh-wrapper invoked but no provisioned git-credentials config is staged")
	}

	host, path := originHostPath(ctx, dir)

	cred, err := newGitCredentialClient(credentialsURL, bearer).fetch(ctx, host, path)
	if err != nil {
		return nil, fmt.Errorf("fetch gh credential: %w", err)
	}

	recordFetchedTokenBestEffort(cred.Token)

	env := append(os.Environ(), "GH_TOKEN="+cred.Token, "GH_ENTERPRISE_TOKEN="+cred.Token)
	if host != "" && host != "github.com" {
		env = append(env, "GH_HOST="+host)
	}

	return env, nil
}

// originHostPath returns the bare host and owner/repo path of dir's git
// "origin" remote, or ("", "") when there is none (no origin configured, or
// the command fails) — CM then serves the instance-wide credential for an
// empty (host, path) pair. dir == "" runs git in the ambient working
// directory (production); tests pass an explicit dir to avoid mutating global
// process state via os.Chdir.
func originHostPath(ctx context.Context, dir string) (host, path string) {
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return "", ""
	}

	return hostPathFromGitURL(strings.TrimSpace(string(out)))
}

// hostPathFromGitURL extracts a bare host and an owner/repo path from a git
// remote URL. Supports https://host/owner/repo(.git), ssh://host/owner/repo(.git)
// (both via url.Parse), and the SCP-like git@host:owner/repo(.git) form.
// Returns ("", "") for an empty or unparseable URL, or one with no path
// component — callers treat that as "no origin", not an error.
func hostPathFromGitURL(raw string) (host, path string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}

	if !strings.Contains(raw, "://") {
		if _, after, ok := strings.Cut(raw, "@"); ok {
			rest := after
			if before, after, ok := strings.Cut(rest, ":"); ok {
				h := before
				p := strings.Trim(strings.TrimSuffix(after, ".git"), "/")

				if h == "" || p == "" {
					return "", ""
				}

				return h, p
			}
		}

		return "", ""
	}

	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", ""
	}

	p := strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/")
	if p == "" {
		return "", ""
	}

	return u.Hostname(), p
}
