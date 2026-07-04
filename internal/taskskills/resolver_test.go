package taskskills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeGen struct{}

func (fakeGen) GenerateToken(context.Context) (string, time.Time, error) {
	return "tok", time.Now().Add(time.Hour), nil
}

// recordingGen counts GenerateToken calls so a test can assert that the
// self-mint path was never reached (e.g. because CM provisioned a
// task-skills clone token, and the resolver must prefer it).
type recordingGen struct {
	calls int32
}

func (g *recordingGen) GenerateToken(context.Context) (string, time.Time, error) {
	atomic.AddInt32(&g.calls, 1)

	return "self-minted-tok", time.Now().Add(time.Hour), nil
}

// discardLogger returns a *slog.Logger that writes nowhere. Most tests below
// exercise the self-mint fallback, which now logs a once-per-process
// deprecation warning; a discard logger keeps `go test -v` output focused on
// genuine failures.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestResolveFetchesPointerClonesAndCaches(t *testing.T) {
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		assert.Equal(t, "/api/chat/task-skills-source", r.URL.Path)
		assert.NotEmpty(t, r.Header.Get("X-Signature-256"), "the GET is HMAC-signed")

		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
		})
	}))
	defer srv.Close()

	var gotURL, gotRef, gotDest, gotTok string

	cloner := func(_ context.Context, url, ref, dest, token string) error {
		gotURL, gotRef, gotDest, gotTok = url, ref, dest, token

		return nil
	}

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, discardLogger())
	r.cloner = cloner

	dir, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, dir)
	assert.Equal(t, "https://example.test/skills.git", gotURL)
	assert.Equal(t, "abc123", gotRef)
	assert.Equal(t, dir, gotDest)
	assert.Equal(t, "tok", gotTok)

	// Second call is cached: no second pointer fetch, no second clone.
	dir2, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, dir, dir2)
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "pointer fetched once; result cached")
}

func TestResolveEmptyPointerYieldsNoSkills(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"git_remote_url": "", "ref": ""})
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, nil)
	r.cloner = func(context.Context, string, string, string, string) error { return nil }

	_, err := r.Resolve(context.Background())
	require.Error(t, err, "an empty remote URL means there is no skills source")
}

// TestGitCloneRejectsDashLeadingRef verifies that gitClone rejects a URL or
// ref beginning with '-' BEFORE invoking git. The "before exec" property is
// verified by checking that no .git directory exists in dest after the call:
// git init is the first exec step, so its absence proves git was never called.
// Without the validation guard, git init runs and creates .git/ before the
// fetch step fails on the bad ref/URL, so the test fails pre-fix.
func TestGitCloneRejectsDashLeadingRef(t *testing.T) {
	t.Parallel()

	r := NewResolver("http://localhost", "key", t.TempDir(), fakeGen{}, nil)

	ctx := context.Background()

	// ref begins with '-': rejected before exec, so .git must not be created.
	dest1 := t.TempDir()

	err := r.gitClone(ctx, "https://example.test/repo.git", "-branch", dest1, "tok")
	require.Error(t, err, "a ref starting with '-' must be rejected")

	_, statErr := os.Stat(filepath.Join(dest1, ".git"))
	assert.True(t, os.IsNotExist(statErr), "git must not be invoked: .git must not exist when ref is rejected")

	// URL begins with '-': also rejected before exec.
	dest2 := t.TempDir()

	err = r.gitClone(ctx, "--upload-pack=evil", "main", dest2, "tok")
	require.Error(t, err, "a URL starting with '-' must be rejected")

	_, statErr = os.Stat(filepath.Join(dest2, ".git"))
	assert.True(t, os.IsNotExist(statErr), "git must not be invoked: .git must not exist when URL is rejected")
}

func TestResolveDoesNotCacheFailure(t *testing.T) {
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"git_remote_url": "https://example.test/s.git", "ref": "r"})
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, discardLogger())
	r.cloner = func(context.Context, string, string, string, string) error { return nil }

	_, err := r.Resolve(context.Background())
	require.Error(t, err)

	_, err = r.Resolve(context.Background())
	require.NoError(t, err, "a prior failure is not cached; the next call retries")
}

// ---- CM-provisioned task-skills clone token --------------------------------

// TestResolveUsesCMProvisionedToken verifies that when the task-skills-source
// response carries a token, Resolve clones with it directly and never calls
// the local token generator — the recording generator's zero call count is
// the proof, not just the token value reaching the cloner.
func TestResolveUsesCMProvisionedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url":   "https://example.test/skills.git",
			"ref":              "abc123",
			"token":            "cm-provisioned-tok",
			"token_expires_at": "2026-07-04T00:00:00Z",
		})
	}))
	defer srv.Close()

	var gotTok string

	gen := &recordingGen{}

	r := NewResolver(srv.URL, "key", t.TempDir(), gen, discardLogger())
	r.cloner = func(_ context.Context, _, _, _, token string) error {
		gotTok = token

		return nil
	}

	_, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "cm-provisioned-tok", gotTok, "the CM-provisioned token must be used to clone")
	assert.Equal(t, int32(0), atomic.LoadInt32(&gen.calls), "the local token generator must not be called when CM provisions a token")
}

// TestResolveSelfMintsWhenTokenAbsent verifies the compat-window fallback:
// when the task-skills-source response carries no token, Resolve still
// self-mints via the local generator, keeping today's path intact.
func TestResolveSelfMintsWhenTokenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
		})
	}))
	defer srv.Close()

	var gotTok string

	gen := &recordingGen{}

	r := NewResolver(srv.URL, "key", t.TempDir(), gen, discardLogger())
	r.cloner = func(_ context.Context, _, _, _, token string) error {
		gotTok = token

		return nil
	}

	_, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "self-minted-tok", gotTok, "the self-minted token must be used to clone when CM provisions none")
	assert.Equal(t, int32(1), atomic.LoadInt32(&gen.calls), "the local token generator must be called exactly once")
}

// TestResolveNilGenAndNoTokenReturnsClearError pins the fail-closed guard: when
// CM's task-skills-source response carries no clone token AND the chat service
// has no local github config (gen is nil, github block unconfigured), Resolve
// must return a clear error rather than panic on a nil-interface method call,
// and must never reach the cloner.
func TestResolveNilGenAndNoTokenReturnsClearError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
		})
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "key", t.TempDir(), nil, discardLogger())
	r.cloner = func(context.Context, string, string, string, string) error {
		t.Fatal("cloner must not be called when there is no token source at all")

		return nil
	}

	_, err := r.Resolve(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no local github config")
}

// TestResolveNilGenWithCMTokenSucceeds proves a nil gen only matters when local
// minting would actually be needed: a CM-provisioned token must still work.
func TestResolveNilGenWithCMTokenSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
			"token":          "cm-provisioned-token",
		})
	}))
	defer srv.Close()

	var gotTok string

	r := NewResolver(srv.URL, "key", t.TempDir(), nil, discardLogger())
	r.cloner = func(_ context.Context, _, _, _, token string) error {
		gotTok = token

		return nil
	}

	_, err := r.Resolve(context.Background())
	require.NoError(t, err, "a nil gen must not block a CM-provisioned token")
	assert.Equal(t, "cm-provisioned-token", gotTok)
}

// TestResolveSelfMintDeprecationWarnsOncePerProcess verifies that the
// self-mint fallback warning logs once per Resolver (a serve process
// constructs exactly one, per NewResolver's call site), not once per
// self-mint attempt. A failed clone is not cached (see
// TestResolveDoesNotCacheFailure), so a real long-lived serve process can
// genuinely re-enter the self-mint path multiple times across chat/start
// requests from a CM version that predates provisioned clone tokens — the
// warning must not spam the log on every retry.
func TestResolveSelfMintDeprecationWarnsOncePerProcess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
		})
	}))
	defer srv.Close()

	var logBuf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, logger)

	var cloneAttempts int32

	r.cloner = func(context.Context, string, string, string, string) error {
		if atomic.AddInt32(&cloneAttempts, 1) <= 2 {
			return errors.New("simulated clone failure")
		}

		return nil
	}

	const wantMsg = "CM did not provision a task-skills clone token; self-minting via local github config is deprecated"

	for range 3 {
		_, _ = r.Resolve(context.Background())
	}

	require.Equal(t, int32(3), atomic.LoadInt32(&cloneAttempts),
		"each retry must reach the cloner: no caching on a prior clone failure")
	assert.Equal(t, 1, strings.Count(logBuf.String(), wantMsg),
		"deprecation warning must be logged exactly once per Resolver instance, even across repeated self-mint attempts")
}
