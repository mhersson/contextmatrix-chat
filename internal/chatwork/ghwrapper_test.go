package chatwork

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-chat/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureStderr temporarily replaces os.Stderr with a pipe so a test can
// inspect what the function under test wrote there, restoring the original
// on cleanup. Must not be used by a t.Parallel() test — os.Stderr is global
// process state, and none of RunGHWrapper's tests (which also use
// t.Setenv, itself incompatible with t.Parallel()) run in parallel with each
// other or with anything else in this package.
func captureStderr(t *testing.T) func() string {
	t.Helper()

	orig := os.Stderr

	r, w, err := os.Pipe()
	require.NoError(t, err)

	os.Stderr = w

	t.Cleanup(func() { os.Stderr = orig })

	return func() string {
		_ = w.Close()

		out, _ := io.ReadAll(r)

		return string(out)
	}
}

// stubExecFunc replaces the package-level execFunc (normally syscall.Exec)
// with fn for the duration of the test, restoring the original on cleanup.
// RunGHWrapper's final step calls execFunc, which would otherwise replace the
// test binary's own process image.
func stubExecFunc(t *testing.T, fn func(argv0 string, argv, envv []string) error) func() {
	t.Helper()

	orig := execFunc
	execFunc = fn

	return func() { execFunc = orig }
}

// ---- provisioned mode (per-call fetch) -----------------------------------------

func TestInstallGHWrapperV2(t *testing.T) {
	t.Parallel()

	const selfPath = "/usr/local/bin/contextmatrix-chat"

	dir, err := installGHWrapperV2(selfPath)
	require.NoError(t, err)

	path := filepath.Join(dir, "gh")

	script, err := os.ReadFile(path)
	require.NoError(t, err)

	s := string(script)
	assert.Contains(t, s, selfPath, "execs the running binary by its resolved path")
	assert.Contains(t, s, "gh-wrapper", "invokes the hidden gh-wrapper subcommand")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestHostPathFromGitURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		wantHost string
		wantPath string
	}{
		{"https with .git", "https://github.com/owner/repo.git", "github.com", "owner/repo"},
		{"https without .git", "https://github.com/owner/repo", "github.com", "owner/repo"},
		{"https GHE", "https://ghe.example.com/owner/repo.git", "ghe.example.com", "owner/repo"},
		{"ssh URL form", "ssh://git@ghe.example.com/owner/repo.git", "ghe.example.com", "owner/repo"},
		{"scp-like form", "git@github.com:owner/repo.git", "github.com", "owner/repo"},
		{"scp-like form no .git", "git@github.com:owner/repo", "github.com", "owner/repo"},
		{"empty", "", "", ""},
		{"unparseable", "not a url at all", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host, path := hostPathFromGitURL(tc.raw)
			assert.Equal(t, tc.wantHost, host)
			assert.Equal(t, tc.wantPath, path)
		})
	}
}

// gitRepoWithOrigin creates a bare-minimum git repo under a temp dir,
// optionally configuring an "origin" remote, and returns the repo dir.
func gitRepoWithOrigin(t *testing.T, originURL string) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	dir := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", dir, "init", "-q").Run())

	if originURL != "" {
		require.NoError(t, exec.Command("git", "-C", dir, "remote", "add", "origin", originURL).Run())
	}

	return dir
}

func TestOriginHostPath(t *testing.T) {
	t.Run("no origin remote", func(t *testing.T) {
		dir := gitRepoWithOrigin(t, "")

		host, path := originHostPath(context.Background(), dir)
		assert.Empty(t, host)
		assert.Empty(t, path)
	})

	t.Run("origin remote present", func(t *testing.T) {
		dir := gitRepoWithOrigin(t, "https://ghe.example.com/acme/widgets.git")

		host, path := originHostPath(context.Background(), dir)
		assert.Equal(t, "ghe.example.com", host)
		assert.Equal(t, "acme/widgets", path)
	})
}

// TestRunGHWrapper_Success proves the full flow: derive host/path from the
// cwd's origin remote, fetch a credential from CM using the FILE-staged
// bearer (never env — same reasoning as RunGitCredentialHelper), and export
// GH_TOKEN/GH_ENTERPRISE_TOKEN/GH_HOST for the final exec of the real gh
// binary. execFunc is swapped for a recorder so the test observes the final
// argv/env without replacing the test process image.
func TestRunGHWrapper_Success(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	dir := gitRepoWithOrigin(t, "https://ghe.example.com/acme/widgets.git")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "ghe.example.com", r.URL.Query().Get("host"))
		assert.Equal(t, "acme/widgets", r.URL.Query().Get("path"))
		assert.Equal(t, "Bearer file-bearer-token", r.Header.Get("Authorization"))

		_ = json.NewEncoder(w).Encode(map[string]string{"username": "x-access-token", "token": "ghs_forgh999"})
	}))
	defer srv.Close()

	require.NoError(t, secrets.WriteEnvFile(gitCredentialsConfigPath(), map[string]string{
		"CM_GIT_CREDENTIALS_URL":   srv.URL,
		"CM_GIT_CREDENTIALS_TOKEN": "file-bearer-token",
	}))

	var gotPath string

	var gotArgv []string

	var gotEnv []string

	restore := stubExecFunc(t, func(argv0 string, argv []string, envv []string) error {
		gotPath = argv0
		gotArgv = argv
		gotEnv = envv

		return nil
	})
	defer restore()

	// args are gh's OWN arguments, as cobra's DisableFlagParsing hands them to
	// RunGHWrapper — no leading "gh" (that was already consumed by the shim's
	// "$@" / argv[0] resolution before it ever reached this function).
	err := RunGHWrapper(context.Background(), dir, []string{"pr", "create"})
	require.NoError(t, err)

	assert.Equal(t, realGHPath, gotPath)
	assert.Equal(t, []string{realGHPath, "pr", "create"}, gotArgv,
		"argv0 must be the real gh path, followed by gh's own args unmodified")

	envMap := envToSliceMap(gotEnv)
	assert.Equal(t, "ghs_forgh999", envMap["GH_TOKEN"])
	assert.Equal(t, "ghs_forgh999", envMap["GH_ENTERPRISE_TOKEN"])
	assert.Equal(t, "ghe.example.com", envMap["GH_HOST"], "non-github.com host must set GH_HOST")
}

// TestRunGHWrapper_GitHubComNoGHHost proves GH_HOST is withheld for
// github.com — gh needs it only for GitHub Enterprise Server hosts.
func TestRunGHWrapper_GitHubComNoGHHost(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	dir := gitRepoWithOrigin(t, "https://github.com/acme/widgets.git")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"username": "x-access-token", "token": "ghs_forgh999"})
	}))
	defer srv.Close()

	require.NoError(t, secrets.WriteEnvFile(gitCredentialsConfigPath(), map[string]string{
		"CM_GIT_CREDENTIALS_URL":   srv.URL,
		"CM_GIT_CREDENTIALS_TOKEN": "file-bearer-token",
	}))

	var gotEnv []string

	restore := stubExecFunc(t, func(_ string, _ []string, envv []string) error {
		gotEnv = envv

		return nil
	})
	defer restore()

	require.NoError(t, RunGHWrapper(context.Background(), dir, []string{"--version"}))

	_, hasGHHost := envToSliceMap(gotEnv)["GH_HOST"]
	assert.False(t, hasGHHost)
}

// TestRunGHWrapper_NoOriginRemote_InstanceCredentialInjected verifies that
// with no origin remote in cwd, the wrapper still fetches with an empty
// (host, path) pair — CM's worker git-credentials endpoint serves the
// instance-wide credential for that pair (rather than 400ing), so a no-origin
// directory has a real happy path. This proves the wrapper completes that
// happy path end to end — the fetched instance token actually reaches gh's
// exec'd environment.
func TestRunGHWrapper_NoOriginRemote_InstanceCredentialInjected(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	dir := gitRepoWithOrigin(t, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.URL.Query().Get("host"))
		assert.Empty(t, r.URL.Query().Get("path"))

		_ = json.NewEncoder(w).Encode(map[string]string{"username": "x-access-token", "token": "ghs_instance999"})
	}))
	defer srv.Close()

	require.NoError(t, secrets.WriteEnvFile(gitCredentialsConfigPath(), map[string]string{
		"CM_GIT_CREDENTIALS_URL":   srv.URL,
		"CM_GIT_CREDENTIALS_TOKEN": "file-bearer-token",
	}))

	var gotPath string

	var gotEnv []string

	restore := stubExecFunc(t, func(argv0 string, _ []string, envv []string) error {
		gotPath = argv0
		gotEnv = envv

		return nil
	})
	defer restore()

	require.NoError(t, RunGHWrapper(context.Background(), dir, []string{"repo", "create"}))

	assert.Equal(t, realGHPath, gotPath, "gh must actually exec")

	envMap := envToSliceMap(gotEnv)
	assert.Equal(t, "ghs_instance999", envMap["GH_TOKEN"], "instance credential must be injected")
	assert.Equal(t, "ghs_instance999", envMap["GH_ENTERPRISE_TOKEN"], "instance credential must be injected")
}

// TestRunGHWrapper_FetchFailure_StillExecsWithoutToken is the discriminating
// regression test for the review finding: RunGHWrapper used to return before
// ever calling execFunc when the credential fetch failed, breaking every
// repo-less gh call (gh --version, gh auth status, gh api /user, gh repo
// list, gh repo create) in provisioned mode. It must now still exec gh — with
// no injected token — and log exactly one concise stderr note that carries
// no token, bearer, or URL query value. On the pre-fix code, execFunc is
// never invoked and this test fails.
func TestRunGHWrapper_FetchFailure_StillExecsWithoutToken(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	dir := gitRepoWithOrigin(t, "https://github.com/acme/widgets.git")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	require.NoError(t, secrets.WriteEnvFile(gitCredentialsConfigPath(), map[string]string{
		"CM_GIT_CREDENTIALS_URL":   srv.URL,
		"CM_GIT_CREDENTIALS_TOKEN": "file-bearer-token",
	}))

	var gotPath string

	var gotArgv []string

	var gotEnv []string

	restore := stubExecFunc(t, func(argv0 string, argv []string, envv []string) error {
		gotPath = argv0
		gotArgv = argv
		gotEnv = envv

		return nil
	})
	defer restore()

	getStderr := captureStderr(t)

	err := RunGHWrapper(context.Background(), dir, []string{"--version"})
	stderrOutput := getStderr()

	require.NoError(t, err, "gh must still run even though the credential fetch failed")
	assert.Equal(t, realGHPath, gotPath, "execFunc must be called even on fetch failure")
	assert.Equal(t, []string{realGHPath, "--version"}, gotArgv)

	envMap := envToSliceMap(gotEnv)
	_, hasGHToken := envMap["GH_TOKEN"]
	_, hasGHEnterpriseToken := envMap["GH_ENTERPRISE_TOKEN"]

	assert.False(t, hasGHToken, "no token may be injected on a fetch failure")
	assert.False(t, hasGHEnterpriseToken, "no enterprise token may be injected on a fetch failure")

	assert.NotEmpty(t, stderrOutput, "must log a note that gh is running without a credential")
	assert.NotContains(t, stderrOutput, "file-bearer-token", "must not leak the bearer")
	assert.NotContains(t, stderrOutput, "Bearer ", "must not leak an Authorization header value")
	assert.NotContains(t, stderrOutput, srv.URL, "must not leak the credentials URL")
	assert.NotContains(t, stderrOutput, "host=", "must not leak URL query values")
	assert.NotContains(t, stderrOutput, "path=", "must not leak URL query values")
}

// TestRunGHWrapper_IgnoresEnvUsesConfigFile mirrors
// TestRunGitCredentialHelper_IgnoresEnvUsesConfigFile: setting
// CM_GIT_CREDENTIALS_TOKEN/URL in the process environment to deliberately
// WRONG values and proving the fetch still authenticates with the
// boot-staged config FILE's (correct) bearer shows RunGHWrapper never
// consults os.Getenv for these two values either — required since gh is
// invoked by the model through the harness bash tool, which execs with a
// scrubbed environment.
func TestRunGHWrapper_IgnoresEnvUsesConfigFile(t *testing.T) {
	t.Setenv("CM_GIT_CREDENTIALS_TOKEN", "wrong-token-from-env")
	t.Setenv("CM_GIT_CREDENTIALS_URL", "http://wrong.example/should-not-be-used")
	t.Setenv("TMPDIR", t.TempDir())

	dir := gitRepoWithOrigin(t, "https://github.com/acme/widgets.git")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer file-bearer-token", r.Header.Get("Authorization"),
			"must authenticate with the FILE's bearer, never an env value")

		_ = json.NewEncoder(w).Encode(map[string]string{"username": "x-access-token", "token": "ghs_fromfile"})
	}))
	defer srv.Close()

	require.NoError(t, secrets.WriteEnvFile(gitCredentialsConfigPath(), map[string]string{
		"CM_GIT_CREDENTIALS_URL":   srv.URL,
		"CM_GIT_CREDENTIALS_TOKEN": "file-bearer-token",
	}))

	var gotEnv []string

	restore := stubExecFunc(t, func(_ string, _ []string, envv []string) error {
		gotEnv = envv

		return nil
	})
	defer restore()

	require.NoError(t, RunGHWrapper(context.Background(), dir, []string{"--version"}))

	envMap := envToSliceMap(gotEnv)
	assert.Equal(t, "ghs_fromfile", envMap["GH_TOKEN"], "must use the FILE's minted token, never a wrong env value")
}

// envToSliceMap builds a lookup map from a "KEY=VALUE" env slice, keeping the
// LAST occurrence of a duplicate key (matching how a real process env works).
func envToSliceMap(env []string) map[string]string {
	m := make(map[string]string, len(env))

	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}

	return m
}
