package chatwork

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	require.NoError(t, ConfigureGitCredentialHelper(ctx, secretsEnvPath))

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

	// Verify git config was applied to the temp gitconfig, not ~/.gitconfig.
	helperOut, err := exec.Command("git", "config", "--global", "--get", "credential.helper").Output()
	require.NoError(t, err)
	assert.Equal(t, scriptPath, strings.TrimSpace(string(helperOut)))

	useHTTPPathOut, err := exec.Command("git", "config", "--global", "--get", "credential.https://github.com.useHttpPath").Output()
	require.NoError(t, err)
	assert.Equal(t, "false", strings.TrimSpace(string(useHTTPPathOut)))
}
