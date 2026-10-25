package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/llmproxy"
	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	"github.com/mhersson/contextmatrix-chat/internal/executor"
	"github.com/mhersson/contextmatrix-chat/internal/metrics"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func getHealth(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusOK, w.Code)

	return w
}

func TestHealth_ReportsBuildAndWorkerImageRevision(t *testing.T) {
	build := protocol.BuildInfo{
		Commit:    testSHA,
		BuildTime: "2026-10-21T08:00:00Z",
		Changes: []protocol.Change{{
			Number: 100, Kind: protocol.ChangeKindFeat, Title: "add jvm-node worker variant",
			MergedAt: "2026-10-15T10:03:00Z",
		}},
	}
	srv := NewServer(Config{
		Tracker:             executor.NewTracker(1),
		Build:               build,
		WorkerImageRevision: func(context.Context) string { return testSHA },
	})

	var hr protocol.HealthResponse
	require.NoError(t, json.Unmarshal(getHealth(t, srv).Body.Bytes(), &hr))

	require.NotNil(t, hr.Build)
	assert.Equal(t, build, *hr.Build)
	assert.Equal(t, testSHA, hr.WorkerImageRevision)
}

func TestHealth_LeavesOutAnUnknownWorkerImageRevision(t *testing.T) {
	tests := []struct {
		name string
		hook func(context.Context) string
	}{
		{"no hook", nil},
		{"image not found", func(context.Context) string { return "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer(Config{
				Tracker:             executor.NewTracker(1),
				Build:               protocol.BuildInfo{Commit: testSHA},
				WorkerImageRevision: tt.hook,
			})

			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(getHealth(t, srv).Body.Bytes(), &body))

			assert.NotContains(t, body, "worker_image_revision")
			assert.Contains(t, body, "build")
		})
	}
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

func TestAdminAuth_BearerTokenFromConfig(t *testing.T) {
	srv := NewServer(Config{APIKey: testAPIKey, MetricsToken: "scrape-token"})

	var ran bool

	h := srv.AdminAuth(func(w http.ResponseWriter, _ *http.Request) {
		ran = true

		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Header.Set("Authorization", "Bearer scrape-token")

	w := httptest.NewRecorder()
	h(w, r)

	assert.True(t, ran)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ---- LLM proxy mount ----------------------------------------------------------

func TestRoutes_LLMProxyOutsideAuthAndDrain(t *testing.T) {
	proxy := newTestLLMProxy(t)
	draining := &atomic.Bool{}
	draining.Store(true)

	srv := NewServer(Config{APIKey: testAPIKey, LLMProxy: proxy, Draining: draining})

	upstream, _ := newUpstream(t)
	token, err := proxy.Register(testSession, protocol.LLMEndpoint{Type: "openai", BaseURL: upstream.URL, APIKey: "k"})
	require.NoError(t, err)

	w := llmRequest(srv, http.MethodPost, "/llm/chat/completions", token)
	assert.Equal(t, http.StatusTeapot, w.Code, "a live token must reach upstream while draining without HMAC")
}

func TestRoutes_LLMProxyMetricsLabel(t *testing.T) {
	mx := metrics.New(llmproxy.Paths(LLMProxyPrefix)...)
	srv := NewServer(Config{APIKey: testAPIKey, LLMProxy: newTestLLMProxy(t), Metrics: mx})

	w := llmRequest(srv, http.MethodGet, "/llm/models", "unknown-token")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	assert.InDelta(t, 1, testutil.ToFloat64(mx.WebhookRequestsTotal.WithLabelValues("llm/models", "401", "error")), 0)
	assert.InDelta(t, 0, testutil.ToFloat64(mx.WebhookRequestsTotal.WithLabelValues("other", "401", "error")), 0)
}
