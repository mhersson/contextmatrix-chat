// Package tlsca builds HTTP clients and transports that trust an operator-
// supplied extra CA in addition to the system trust store. It backs the worker
// container's ca_cert_file support: a TLS-inspecting egress proxy's CA is
// bind-mounted into the container and threaded into the harness LLM client and
// the MCP bridge so their outbound TLS validates.
package tlsca

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// HTTPClientWithCA returns an *http.Client whose TLS trust is the system pool
// plus the PEM certificate(s) at path. An empty path returns a plain client that
// uses system trust only (nil Transport). A read error, or a PEM that yields no
// certificates, is returned as an error.
func HTTPClientWithCA(path string) (*http.Client, error) {
	tr, err := transportWithCA(path)
	if err != nil {
		return nil, err
	}

	if tr == nil {
		return &http.Client{}, nil
	}

	return &http.Client{Transport: tr}, nil
}

// transportWithCA returns an *http.Transport whose TLS config trusts the system
// pool plus the PEM certificate(s) at path. It is cloned from
// http.DefaultTransport so proxy (ProxyFromEnvironment), timeouts, and
// connection pooling are preserved - a TLS-inspecting deployment usually implies
// an explicit HTTP(S) proxy too - overriding only the trust store. An empty path
// returns (nil, nil) so callers fall back to http.DefaultTransport.
func transportWithCA(path string) (*http.Transport, error) {
	if path == "" {
		return nil, nil
	}

	// Clone the system pool so we augment, not replace, the default roots. On
	// platforms where SystemCertPool is unavailable, start from an empty pool.
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ca_cert_file %q: %w", path, err)
	}

	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("ca_cert_file %q: no certificates found in PEM", path)
	}

	tlsConf := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := base.Clone()
		clone.TLSClientConfig = tlsConf

		return clone, nil
	}

	// Defensive: http.DefaultTransport is always *http.Transport in the stdlib,
	// but if that ever changes, still honour proxy env like the default does.
	return &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConf}, nil
}
