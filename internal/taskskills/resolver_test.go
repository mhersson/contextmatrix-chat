package taskskills

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardLogger returns a *slog.Logger that writes nowhere, keeping
// `go test -v` output focused on genuine failures.
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
			"token":          "tok",
		})
	}))
	defer srv.Close()

	var gotURL, gotRef, gotDest, gotTok string

	cloner := func(_ context.Context, url, ref, dest, token string) error {
		gotURL, gotRef, gotDest, gotTok = url, ref, dest, token

		return nil
	}

	r := NewResolver(srv.URL, "key", t.TempDir(), discardLogger())
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

	r := NewResolver(srv.URL, "key", t.TempDir(), nil)
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

	r := NewResolver("http://localhost", "key", t.TempDir(), nil)

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

		_ = json.NewEncoder(w).Encode(map[string]string{"git_remote_url": "https://example.test/s.git", "ref": "r", "token": "tok"})
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "key", t.TempDir(), discardLogger())
	r.cloner = func(context.Context, string, string, string, string) error { return nil }

	_, err := r.Resolve(context.Background())
	require.Error(t, err)

	_, err = r.Resolve(context.Background())
	require.NoError(t, err, "a prior failure is not cached; the next call retries")
}

// ---- CM-provisioned task-skills clone token --------------------------------

// TestResolveNoTokenFails pins the fail-closed guard: when CM's
// task-skills-source response carries no clone token, Resolve must return a
// clear error and never reach the cloner — the CM-provisioned token is the
// only clone credential.
func TestResolveNoTokenFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
		})
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "key", t.TempDir(), discardLogger())
	r.cloner = func(context.Context, string, string, string, string) error {
		t.Fatal("cloner must not be called without a CM-provisioned token")

		return nil
	}

	_, err := r.Resolve(context.Background())
	require.Error(t, err)
	assert.Equal(t, "CM did not provision a task-skills clone token", err.Error())
}
