package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeImageLister struct {
	summaries []webhookcore.ImageSummary
	err       error
}

func (f *fakeImageLister) ListImages(_ context.Context) ([]webhookcore.ImageSummary, error) {
	return f.summaries, f.err
}

// newImagesServer builds a minimal Server with the images dependency wired.
func newImagesServer(images webhookcore.ImageLister) *Server {
	return NewServer(Config{
		APIKey:           testAPIKey,
		Images:           images,
		ImageListFilters: []string{"contextmatrix-chat"},
	})
}

func signedGet(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, target, nil)
	signReq(t, r, testAPIKey, nil, nowTS())

	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	return w
}

func TestImages_FiltersPerTagAndMaps(t *testing.T) {
	srv := newImagesServer(&fakeImageLister{summaries: []webhookcore.ImageSummary{
		{
			Tags:      []string{"contextmatrix-chat-worker:go-node"},
			Digests:   []string{"contextmatrix-chat-worker@sha256:abc"},
			CreatedAt: 1750000000,
			SizeBytes: 2_560_000_000,
		},
		{Tags: []string{"contextmatrix-agent-worker:dev"}}, // agent image: no chat match, dropped
		{Tags: []string{"contextmatrix-chat-worker:dev", "unrelated:tag"}},
	}})

	w := signedGet(t, srv, "/images")
	require.Equal(t, http.StatusOK, w.Code)

	var resp protocol.ListImagesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.True(t, resp.OK)
	require.Len(t, resp.Images, 2)
	assert.Equal(t, []string{"contextmatrix-chat-worker:go-node"}, resp.Images[0].Tags)
	assert.Equal(t, int64(1750000000), resp.Images[0].Created)
	assert.Equal(t, []string{"contextmatrix-chat-worker:dev"}, resp.Images[1].Tags)
}

func TestImages_DockerErrorReturns502Generic(t *testing.T) {
	srv := newImagesServer(&fakeImageLister{err: errors.New("daemon exploded: secret detail")})

	w := signedGet(t, srv, "/images")
	require.Equal(t, http.StatusBadGateway, w.Code)

	var resp protocol.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, protocol.CodeUpstreamFailure, resp.Code)
	assert.NotContains(t, resp.Message, "secret detail")
}

func TestImages_RequiresSignature(t *testing.T) {
	srv := newImagesServer(&fakeImageLister{})

	r := httptest.NewRequest(http.MethodGet, "/images", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestImages_NilListerReturns500(t *testing.T) {
	srv := NewServer(Config{APIKey: testAPIKey})

	w := signedGet(t, srv, "/images")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
