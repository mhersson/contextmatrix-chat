package webhook

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- health / readyz --------------------------------------------------------

func TestReadyz_OKAndDraining(t *testing.T) {
	srv := NewServer(Config{APIKey: testAPIKey})

	r1 := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w1 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w1, r1)
	require.Equal(t, http.StatusOK, w1.Code)

	srv.draining.Store(true)

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
