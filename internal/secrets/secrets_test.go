package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpen parses KEY=value lines, skips blanks and comments.
func TestOpen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	content := "# comment\n\nFOO=bar\nBAZ=qux quux\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	s, err := Open(path)
	require.NoError(t, err)

	assert.Equal(t, "bar", s.Get("FOO"))
	assert.Equal(t, "qux quux", s.Get("BAZ"))
	assert.Empty(t, s.Get("MISSING"))
}

// TestOpenMissingFile returns an empty, usable Source rather than an error: a
// missing config file is a legitimate state (e.g. the gh-wrapper subcommand
// before any git-credentials config is staged), not a broken deployment.
// Every Get on the returned Source must behave exactly like an absent key in
// a file that does exist.
func TestOpenMissingFile(t *testing.T) {
	t.Parallel()

	s, err := Open("/nonexistent/path/env")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Empty(t, s.Get("CM_GIT_CREDENTIALS_TOKEN"))
}

// TestOpenOtherReadErrorStillErrors verifies Open only tolerates a missing
// file — a different filesystem error (e.g. the path is a directory, so the
// open fails with EISDIR, not ENOENT) must still be reported, not silently
// swallowed into an empty Source.
func TestOpenOtherReadErrorStillErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // a directory, not a file: os.Open + Read fails with EISDIR

	_, err := Open(dir)
	assert.Error(t, err)
}

// TestWriteEnvFile checks atomic write, modes, and round-trip.
func TestWriteEnvFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "env")

	vals := map[string]string{
		"CM_GIT_CREDENTIALS_URL":   "http://cm:8080/api/worker/git-credentials",
		"CM_GIT_CREDENTIALS_TOKEN": "sess-abc.bearer",
	}

	require.NoError(t, WriteEnvFile(path, vals))

	// File mode must be 0600.
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// Dir mode must be 0700.
	di, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm())

	// Round-trip: Open must return the same values.
	s, err := Open(path)
	require.NoError(t, err)
	assert.Equal(t, "http://cm:8080/api/worker/git-credentials", s.Get("CM_GIT_CREDENTIALS_URL"))
	assert.Equal(t, "sess-abc.bearer", s.Get("CM_GIT_CREDENTIALS_TOKEN"))
}

// TestWriteEnvFileDeterministic asserts byte-identical, sorted output across
// rewrites.
func TestWriteEnvFileDeterministic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	vals := map[string]string{
		"CM_GIT_CREDENTIALS_TOKEN": "tok123",
		"CM_GIT_CREDENTIALS_URL":   "http://cm:8080",
		"EXTRA_SECRET":             "extra-val",
		"ANOTHER_KEY":              "another-val",
	}

	require.NoError(t, WriteEnvFile(path, vals))
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, WriteEnvFile(path, vals))
	second, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
	assert.Equal(t,
		"ANOTHER_KEY=another-val\nCM_GIT_CREDENTIALS_TOKEN=tok123\nCM_GIT_CREDENTIALS_URL=http://cm:8080\nEXTRA_SECRET=extra-val\n",
		string(first))
}
