package secrets

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGenerator simulates a TokenGenerator that returns pre-configured tokens.
type fakeGenerator struct {
	calls []fakeCall
	idx   int
}

type fakeCall struct {
	token     string
	expiresAt time.Time
}

func (f *fakeGenerator) GenerateToken(_ context.Context) (string, time.Time, error) {
	if f.idx >= len(f.calls) {
		return "", time.Time{}, nil
	}

	c := f.calls[f.idx]
	f.idx++

	return c.token, c.expiresAt, nil
}

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
// missing secrets file is now a legitimate state (a fully CM-provisioned
// session with no local github/llm_endpoint config at all never writes the
// shared secrets file — see cli.newTokenProvider's nil-provider path), not a
// broken deployment. Every Get on the returned Source must behave exactly
// like an absent key in a file that does exist.
func TestOpenMissingFile(t *testing.T) {
	t.Parallel()

	s, err := Open("/nonexistent/path/env")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Empty(t, s.Get("CM_GIT_TOKEN"))
	assert.Empty(t, s.Get("LLM_API_KEY"))
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
		"CM_GIT_TOKEN": "tok123",
		"LLM_API_KEY":  "llm-key",
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
	assert.Equal(t, "tok123", s.Get("CM_GIT_TOKEN"))
	assert.Equal(t, "llm-key", s.Get("LLM_API_KEY"))
}

// TestWriteEnvFileNeutralKeys checks that the three LLM endpoint keys are
// written under provider-neutral names and that empty values are omitted.
func TestWriteEnvFileNeutralKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	vals := map[string]string{
		"CM_GIT_TOKEN": "tok-git",
		"LLM_API_KEY":  "sk-test-key",
		"LLM_BASE_URL": "https://your-llm-endpoint.example/v1",
		"LLM_TYPE":     "openai",
	}

	require.NoError(t, WriteEnvFile(path, vals))

	s, err := Open(path)
	require.NoError(t, err)

	assert.Equal(t, "tok-git", s.Get("CM_GIT_TOKEN"))
	assert.Equal(t, "sk-test-key", s.Get("LLM_API_KEY"))
	assert.Equal(t, "https://your-llm-endpoint.example/v1", s.Get("LLM_BASE_URL"))
	assert.Equal(t, "openai", s.Get("LLM_TYPE"))
}

// TestWriteEnvFileDeterministic asserts byte-identical output across rewrites
// even with keys beyond the fixed-order pair.
func TestWriteEnvFileDeterministic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	vals := map[string]string{
		"CM_GIT_TOKEN": "tok123",
		"LLM_API_KEY":  "llm-key",
		"EXTRA_SECRET": "extra-val",
		"ANOTHER_KEY":  "another-val",
	}

	require.NoError(t, WriteEnvFile(path, vals))
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, WriteEnvFile(path, vals))
	second, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
	assert.Equal(t,
		"LLM_API_KEY=llm-key\nCM_GIT_TOKEN=tok123\nANOTHER_KEY=another-val\nEXTRA_SECRET=extra-val\n",
		string(first))
}

// TestRefresherWritesAndRefreshes exercises the Refresher end-to-end.
func TestRefresherWritesAndRefreshes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	now := time.Now()
	firstExpiry := now.Add(60 * time.Millisecond)
	secondExpiry := now.Add(10 * time.Second)

	gen := &fakeGenerator{
		calls: []fakeCall{
			{token: "tok1", expiresAt: firstExpiry},
			{token: "tok2", expiresAt: secondExpiry},
		},
	}

	r := NewRefresher(path, EndpointSecrets{APIKey: "llm-static-key"}, gen, nil)
	// Shrink timing constants so the test completes quickly.
	r.refreshBefore = 20 * time.Millisecond
	r.minSleep = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)

	go func() { errCh <- r.Run(ctx) }()

	// After Run starts, the file must contain tok1.
	require.Eventually(t, func() bool {
		s, err := Open(path)
		if err != nil {
			return false
		}

		return s.Get("CM_GIT_TOKEN") == "tok1"
	}, 2*time.Second, 10*time.Millisecond, "expected tok1 in env file")

	// After the refresh window passes, the file must contain tok2.
	require.Eventually(t, func() bool {
		s, err := Open(path)
		if err != nil {
			return false
		}

		return s.Get("CM_GIT_TOKEN") == "tok2"
	}, 2*time.Second, 10*time.Millisecond, "expected tok2 in env file")

	// LLM_API_KEY must persist across rewrites.
	s, err := Open(path)
	require.NoError(t, err)
	assert.Equal(t, "llm-static-key", s.Get("LLM_API_KEY"))
	// LLM_BASE_URL and LLM_TYPE were not set → must be absent.
	assert.Empty(t, s.Get("LLM_BASE_URL"))
	assert.Empty(t, s.Get("LLM_TYPE"))
	// Strengthen: the keys must also be absent from the raw file bytes.
	data, _ := os.ReadFile(path)
	assert.NotContains(t, string(data), "LLM_BASE_URL")
	assert.NotContains(t, string(data), "LLM_TYPE")

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// countingPATGenerator always returns the PAT-mode year-9999 sentinel expiry,
// so every loop iteration would otherwise take the expiry-derived
// (~8000-year) sleep.
type countingPATGenerator struct{ calls int }

func (g *countingPATGenerator) GenerateToken(_ context.Context) (string, time.Time, error) {
	g.calls++

	return "tok", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), nil
}

// TestRefresherRetriesOnWriteFailure ensures a failed env-file write retries
// on the short backoff instead of sleeping until token expiry.
func TestRefresherRetriesOnWriteFailure(t *testing.T) {
	t.Parallel()

	// A regular file where WriteEnvFile needs a directory makes MkdirAll fail
	// on every attempt — a persistent staging failure (bind-mount not ready,
	// ENOSPC, permission race).
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	path := filepath.Join(blocker, "env") // parent is a file -> MkdirAll errors

	gen := &countingPATGenerator{}
	r := NewRefresher(path, EndpointSecrets{}, gen, nil)
	r.retryBackoff = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_ = r.Run(ctx)

	// With the expiry-derived sleep the refresher would call GenerateToken
	// exactly once, then sleep ~8000 years. Retrying on the short backoff
	// lands many attempts inside the 200ms window.
	assert.Greater(t, gen.calls, 1,
		"write failure must retry on the short backoff, not sleep until token expiry")
}

// TestRefresherInvokesOnRotateOnEveryWrite verifies OnRotate fires with the
// freshly minted token after each successful env-file write, including the
// very first — this is what lets the serve-side log-bridge redactor learn a
// live GitHub token without a restart.
func TestRefresherInvokesOnRotateOnEveryWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	now := time.Now()
	gen := &fakeGenerator{
		calls: []fakeCall{
			{token: "tok1", expiresAt: now.Add(60 * time.Millisecond)},
			{token: "tok2", expiresAt: now.Add(10 * time.Second)},
		},
	}

	r := NewRefresher(path, EndpointSecrets{}, gen, nil)
	r.refreshBefore = 20 * time.Millisecond
	r.minSleep = 5 * time.Millisecond

	var mu sync.Mutex

	var rotated []string

	r.SetOnRotate(func(token string) {
		mu.Lock()

		rotated = append(rotated, token)
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)

	go func() { errCh <- r.Run(ctx) }()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(rotated) >= 2 && rotated[0] == "tok1" && rotated[1] == "tok2"
	}, 2*time.Second, 10*time.Millisecond, "expected OnRotate to fire with tok1 then tok2 in order")

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
