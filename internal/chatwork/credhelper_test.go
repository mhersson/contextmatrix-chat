package chatwork

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-chat/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigureGitCredentialHelper(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// os.TempDir() honors TMPDIR on Linux; redirect it to a per-test temp dir
	// so the credential helper script is auto-cleaned and doesn't persist in /tmp.
	t.Setenv("TMPDIR", t.TempDir())

	dir := t.TempDir()
	secretsEnvPath := filepath.Join(dir, "secrets.env")
	gitconfigPath := filepath.Join(dir, "gitconfig")

	// Redirect global git config to a temp file — prevents polluting ~/.gitconfig.
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfigPath)
	// Override HOME so any fallback git config resolution stays in the temp dir.
	t.Setenv("HOME", dir)

	require.NoError(t, os.WriteFile(secretsEnvPath, []byte("CM_GIT_TOKEN=tok1\n"), 0o600))

	ctx := context.Background()
	require.NoError(t, ConfigureGitCredentialHelper(ctx, secretsEnvPath, ""))

	scriptPath := filepath.Join(os.TempDir(), "cm-git-credential-helper.sh")

	runHelper := func(t *testing.T) string {
		t.Helper()

		out, err := exec.Command(scriptPath, "get").Output()
		require.NoError(t, err)

		return string(out)
	}

	// Initial token.
	out := runHelper(t)
	assert.Contains(t, out, "username=x-access-token")
	assert.Contains(t, out, "password=tok1")

	// Rotate: rewrite secrets env; helper must re-read fresh each invocation.
	require.NoError(t, os.WriteFile(secretsEnvPath, []byte("CM_GIT_TOKEN=tok2\n"), 0o600))

	out = runHelper(t)
	assert.Contains(t, out, "username=x-access-token")
	assert.Contains(t, out, "password=tok2")

	// Verify git config was applied to the temp gitconfig, not ~/.gitconfig, and
	// is scoped to the default host (empty host param -> github.com), not global.
	helperOut, err := exec.Command("git", "config", "--global", "--get", "credential.https://github.com.helper").Output()
	require.NoError(t, err)
	assert.Equal(t, scriptPath, strings.TrimSpace(string(helperOut)))

	useHTTPPathOut, err := exec.Command("git", "config", "--global", "--get", "credential.https://github.com.useHttpPath").Output()
	require.NoError(t, err)
	assert.Equal(t, "false", strings.TrimSpace(string(useHTTPPathOut)))
}

func TestConfigureGitCredentialHelperIsHostScoped(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))

	ctx := context.Background()
	require.NoError(t, ConfigureGitCredentialHelper(ctx, "/run/cm-secrets/env", "acme.ghe.com"))

	got, err := exec.CommandContext(ctx, "git", "config", "--global", "--get",
		"credential.https://acme.ghe.com.helper").Output()
	require.NoError(t, err)
	assert.Contains(t, string(got), "cm-git-credential-helper.sh")

	// The unscoped global credential.helper must NOT be set (it would answer for any host).
	err = exec.CommandContext(ctx, "git", "config", "--global", "--get", "credential.helper").Run()
	assert.Error(t, err, "helper must be host-scoped, not global")
}

// ---- v2: provisioned mode (CM_GIT_CREDENTIALS_TOKEN) -------------------------

func TestConfigureGitCredentialHelperV2_RegistersGlobally(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("HOME", dir)

	ctx := context.Background()
	require.NoError(t, ConfigureGitCredentialHelperV2(ctx, "/usr/local/bin/contextmatrix-chat"))

	scriptPath := filepath.Join(os.TempDir(), "cm-git-credential-helper-v2.sh")

	helperOut, err := exec.Command("git", "config", "--global", "--get", "credential.helper").Output()
	require.NoError(t, err)
	assert.Equal(t, scriptPath, strings.TrimSpace(string(helperOut)),
		"provisioned mode registers the helper UNSCOPED — applies to every host")

	useHTTPPathOut, err := exec.Command("git", "config", "--global", "--get", "credential.useHttpPath").Output()
	require.NoError(t, err)
	assert.Equal(t, "true", strings.TrimSpace(string(useHTTPPathOut)),
		"CM needs the path component to resolve the correct per-project credential")

	// The v1 host-scoped key must NOT also be set by v2.
	err = exec.Command("git", "config", "--global", "--get", "credential.https://github.com.helper").Run()
	assert.Error(t, err, "v2 must not additionally register a host-scoped entry")
}

func TestConfigureGitCredentialHelperV2_ScriptExecsSelfWithSubcommand(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("HOME", dir)

	const selfPath = "/usr/local/bin/contextmatrix-chat"

	ctx := context.Background()
	require.NoError(t, ConfigureGitCredentialHelperV2(ctx, selfPath))

	scriptPath := filepath.Join(os.TempDir(), "cm-git-credential-helper-v2.sh")

	script, err := os.ReadFile(scriptPath)
	require.NoError(t, err)

	s := string(script)
	assert.Contains(t, s, selfPath, "the script execs the running binary by its resolved path")
	assert.Contains(t, s, "git-credential", "the script invokes the hidden git-credential subcommand")

	info, err := os.Stat(scriptPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// ---- git-credential protocol: fetch + retry -----------------------------------

func TestGitCredentialGet_Success(t *testing.T) {
	var gotHost, gotPath, gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.URL.Query().Get("host")
		gotPath = r.URL.Query().Get("path")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"username": "x-access-token",
			"token":    "ghs_minted123",
		})
	}))
	defer srv.Close()

	// capability[]= and wwwauth[]= lines are part of git's real protocol but
	// carry no protocol/host/path information — the parser must ignore them.
	stdin := strings.NewReader("capability[]=authtype\nprotocol=https\nhost=github.com\npath=owner/repo.git\nwwwauth[]=Basic\n\n")

	var stdout strings.Builder

	var fetched string

	err := GitCredentialGet(context.Background(), stdin, &stdout, srv.URL, "sess1.bearer-token",
		func(tok string) { fetched = tok })
	require.NoError(t, err)

	assert.Equal(t, "github.com", gotHost)
	assert.Equal(t, "owner/repo.git", gotPath)
	assert.Equal(t, "Bearer sess1.bearer-token", gotAuth)
	assert.Contains(t, stdout.String(), "username=x-access-token")
	assert.Contains(t, stdout.String(), "password=ghs_minted123")
	assert.Equal(t, "ghs_minted123", fetched, "onFetched must receive the minted token")
}

func TestGitCredentialGet_RetriesOn409ThenSucceeds(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusConflict)

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"username": "x-access-token", "token": "ghs_afterretry"})
	}))
	defer srv.Close()

	client := newGitCredentialClient(srv.URL, "bearer")
	client.retryDelay = time.Millisecond

	stdin := strings.NewReader("protocol=https\nhost=example.test\npath=owner/repo\n\n")

	var stdout strings.Builder

	err := gitCredentialGetWithClient(context.Background(), stdin, &stdout, client, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&attempts), "the 409-not-yet-live retry must be bounded, not unbounded")
	assert.Contains(t, stdout.String(), "password=ghs_afterretry")
}

func TestGitCredentialGet_FailsAfterMaxRetriesOn409(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	client := newGitCredentialClient(srv.URL, "bearer")
	client.retryDelay = time.Millisecond

	stdin := strings.NewReader("protocol=https\nhost=example.test\npath=owner/repo\n\n")

	var stdout strings.Builder

	err := gitCredentialGetWithClient(context.Background(), stdin, &stdout, client, nil)
	require.Error(t, err)
	assert.Equal(t, int32(client.maxAttempts), atomic.LoadInt32(&attempts))
	assert.Empty(t, stdout.String(), "no stdout on failure — git surfaces its own auth error")
}

func TestGitCredentialGet_NonRetryableErrorFailsImmediately(t *testing.T) {
	var attempts int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newGitCredentialClient(srv.URL, "bearer")
	client.retryDelay = time.Millisecond

	stdin := strings.NewReader("protocol=https\nhost=example.test\npath=owner/repo\n\n")

	var stdout strings.Builder

	err := gitCredentialGetWithClient(context.Background(), stdin, &stdout, client, nil)
	require.Error(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&attempts), "a non-409 error must not be retried")
	assert.Empty(t, stdout.String())
}

// ---- RunGitCredentialHelper: the hidden CLI subcommand's entry point ----------

func TestRunGitCredentialHelper_GetSuccess(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer file-bearer-token", r.Header.Get("Authorization"))

		_ = json.NewEncoder(w).Encode(map[string]string{"username": "x-access-token", "token": "ghs_fromfile"})
	}))
	defer srv.Close()

	require.NoError(t, secrets.WriteEnvFile(gitCredentialsConfigPath(), map[string]string{
		"CM_GIT_CREDENTIALS_URL":   srv.URL,
		"CM_GIT_CREDENTIALS_TOKEN": "file-bearer-token",
	}))

	stdin := strings.NewReader("protocol=https\nhost=example.test\npath=owner/repo\n\n")

	var stdout strings.Builder

	err := RunGitCredentialHelper(context.Background(), "get", stdin, &stdout)
	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "password=ghs_fromfile")
}

func TestRunGitCredentialHelper_StoreAndEraseAreNoops(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	var hit atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer srv.Close()

	require.NoError(t, secrets.WriteEnvFile(gitCredentialsConfigPath(), map[string]string{
		"CM_GIT_CREDENTIALS_URL":   srv.URL,
		"CM_GIT_CREDENTIALS_TOKEN": "tok",
	}))

	for _, op := range []string{"store", "erase"} {
		var stdout strings.Builder

		stdin := strings.NewReader("protocol=https\nhost=h\npath=p\nusername=x-access-token\npassword=ghs_fromfile\n\n")
		err := RunGitCredentialHelper(context.Background(), op, stdin, &stdout)
		require.NoError(t, err, "op=%s", op)
		assert.Empty(t, stdout.String(), "op=%s", op)
	}

	assert.False(t, hit.Load(), "store/erase must never call CM — CM mints fresh tokens per get, nothing to persist or invalidate locally")
}

func TestRunGitCredentialHelper_NoConfigFileReturnsError(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // fresh temp dir: no config file staged

	stdin := strings.NewReader("protocol=https\nhost=example.test\npath=owner/repo\n\n")

	var stdout strings.Builder

	err := RunGitCredentialHelper(context.Background(), "get", stdin, &stdout)
	require.Error(t, err)
	assert.Empty(t, stdout.String())
}

// TestRunGitCredentialHelper_IgnoresEnvUsesConfigFile is the required proof
// that the helper never depends on inherited process environment. The model
// invokes git through the harness bash tool, which execs with a SCRUBBED
// environment (tools.ScrubbedEnv) stripping everything outside a small
// allowlist (PATH, HOME, USER, LANG, LC_ALL, TMPDIR, TERM) — so
// CM_GIT_CREDENTIALS_TOKEN/URL would be invisible there if read via
// os.Getenv. Setting them here to WRONG values and proving the fetch still
// authenticates with the FILE's (correct) bearer is a stronger proof than
// merely clearing the environment: it shows the function never consults the
// env for these two values at all, so it behaves identically however the
// caller's environment happens to be scrubbed.
func TestRunGitCredentialHelper_IgnoresEnvUsesConfigFile(t *testing.T) {
	t.Setenv("CM_GIT_CREDENTIALS_TOKEN", "wrong-token-from-env")
	t.Setenv("CM_GIT_CREDENTIALS_URL", "http://wrong.example/should-not-be-used")
	t.Setenv("TMPDIR", t.TempDir())

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

	stdin := strings.NewReader("protocol=https\nhost=example.test\npath=owner/repo\n\n")

	var stdout strings.Builder

	err := RunGitCredentialHelper(context.Background(), "get", stdin, &stdout)
	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "password=ghs_fromfile")
}

func TestRunGitCredentialHelper_RecordsFetchedTokenForRedaction(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"username": "x-access-token", "token": "ghs_forredaction999"})
	}))
	defer srv.Close()

	require.NoError(t, secrets.WriteEnvFile(gitCredentialsConfigPath(), map[string]string{
		"CM_GIT_CREDENTIALS_URL":   srv.URL,
		"CM_GIT_CREDENTIALS_TOKEN": "bearer",
	}))

	stdin := strings.NewReader("protocol=https\nhost=example.test\npath=owner/repo\n\n")

	var stdout strings.Builder

	require.NoError(t, RunGitCredentialHelper(context.Background(), "get", stdin, &stdout))

	fetched, err := os.ReadFile(fetchedTokensPath())
	require.NoError(t, err)
	assert.Contains(t, string(fetched), "ghs_forredaction999")
}
