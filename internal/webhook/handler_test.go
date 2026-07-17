package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- signing helpers --------------------------------------------------------

const testAPIKey = "test-secret-key"

// signReq stamps r with a valid HMAC signature for the given key over
// METHOD\nURI\nTS.BODY using the supplied timestamp.
func signReq(t *testing.T, r *http.Request, key string, body []byte, ts string) {
	t.Helper()

	sig := protocol.SignPayloadWithTimestamp(key, r.Method, r.URL.RequestURI(), body, ts)
	r.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	r.Header.Set(protocol.TimestampHeader, ts)
}

// nowTS returns the current Unix second as a string.
func nowTS() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// ---- health / readyz --------------------------------------------------------

func TestReadyz_OKAndDraining(t *testing.T) {
	draining := &atomic.Bool{}
	srv := NewServer(Config{APIKey: testAPIKey, Draining: draining})

	r1 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w1 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w1, r1)
	require.Equal(t, http.StatusOK, w1.Code)

	draining.Store(true)

	r2 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w2 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w2, r2)
	require.Equal(t, http.StatusServiceUnavailable, w2.Code)
	assert.Contains(t, w2.Body.String(), "draining")
}

func TestDropSessionReclaimsStdinLock(t *testing.T) {
	t.Parallel()

	srv := NewServer(Config{})

	first := srv.stdinLock("sess-1")
	srv.DropSession("sess-1")
	second := srv.stdinLock("sess-1")

	assert.NotSame(t, first, second, "DropSession must reclaim the entry so a later lock is a fresh mutex")
}

// ---- logs -------------------------------------------------------------------

// TestLogs_MountAndAuth is a smoke test for the /logs route as mounted through
// the real Routes() mux: the backendkit suite pins the SSE mechanics
// (keepalive, filtering, ...) in isolation, but only this repo's own mux and
// middleware chain can prove /logs is actually wired behind Auth here. A
// signed GET streams the SSE preamble over a real connection (a
// ResponseRecorder cannot observe a still-streaming handler, so this needs
// httptest.NewServer); an unsigned GET is rejected outright.
func TestLogs_MountAndAuth(t *testing.T) {
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.SessionID }, nil)
	srv := NewServer(Config{APIKey: testAPIKey, Skew: protocol.DefaultMaxClockSkew, Hub: hub})

	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	defer srv.CloseSSE()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/logs", nil)
	require.NoError(t, err)

	sigTS := nowTS()
	sig := protocol.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs", nil, sigTS)
	req.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	req.Header.Set(protocol.TimestampHeader, sigTS)

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // unblocked via ctx cancel below
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	buf := make([]byte, len(": connected\n\n"))
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	assert.Equal(t, ": connected\n\n", string(buf), "body must start with the SSE connected preamble")

	cancel() // unblock the still-streaming handler so the test exits promptly

	unsigned := httptest.NewRequest(http.MethodGet, "/logs", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, unsigned)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "unsigned GET /logs must be rejected by Auth")
}
