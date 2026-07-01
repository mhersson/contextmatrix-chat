package tlsca

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPClientWithCA(t *testing.T) {
	// A TLS server with a self-signed cert the system trust store rejects. Its
	// own certificate, written to a PEM, is the "extra CA" under test.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	certPath := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(certPath, pemBytes, 0o600))

	t.Run("empty path returns a plain client", func(t *testing.T) {
		c, err := HTTPClientWithCA("")
		require.NoError(t, err)
		require.NotNil(t, c)
		assert.Nil(t, c.Transport, "empty path must not install a custom transport")
	})

	t.Run("valid CA is trusted", func(t *testing.T) {
		// Control: the default trust store must reject the self-signed server.
		_, err := http.Get(srv.URL) //nolint:bodyclose // request fails before a body exists
		require.Error(t, err, "control: default client must not trust the self-signed server")

		c, err := HTTPClientWithCA(certPath)
		require.NoError(t, err)
		require.NotNil(t, c.Transport, "a CA path must install a custom transport")

		resp, err := c.Get(srv.URL)
		require.NoError(t, err, "the CA client must trust the server signed by the extra CA")

		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("bad PEM errors", func(t *testing.T) {
		badPath := filepath.Join(t.TempDir(), "bad.pem")
		require.NoError(t, os.WriteFile(badPath, []byte("not a certificate"), 0o600))

		_, err := HTTPClientWithCA(badPath)
		require.Error(t, err)
	})

	t.Run("missing file errors", func(t *testing.T) {
		_, err := HTTPClientWithCA(filepath.Join(t.TempDir(), "does-not-exist.pem"))
		require.Error(t, err)
	})
}

func TestTransportWithCA(t *testing.T) {
	t.Run("empty path returns nil", func(t *testing.T) {
		tr, err := TransportWithCA("")
		require.NoError(t, err)
		assert.Nil(t, tr, "empty path must return a nil transport so callers fall back to the default")
	})

	t.Run("valid CA yields a transport with root pool", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		defer srv.Close()

		certPath := filepath.Join(t.TempDir(), "ca.pem")
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		require.NoError(t, os.WriteFile(certPath, pemBytes, 0o600))

		tr, err := TransportWithCA(certPath)
		require.NoError(t, err)
		require.NotNil(t, tr)
		require.NotNil(t, tr.TLSClientConfig)
		assert.NotNil(t, tr.TLSClientConfig.RootCAs)
		// Cloned from http.DefaultTransport, so proxy-from-environment is
		// preserved — the transport works behind an explicit HTTPS_PROXY too.
		assert.NotNil(t, tr.Proxy, "the CA transport must preserve DefaultTransport's proxy support")
	})
}
