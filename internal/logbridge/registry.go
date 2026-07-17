package logbridge

import (
	"sync"

	"github.com/mhersson/contextmatrix-harness/redact"
)

// RedactorRegistry composes the log-bridge redactor from a single source of
// truth so the masked-secret set stays consistent as secrets come and go: every
// per-session CM-provisioned secret (the LLM key, protocol v0.5.0; the
// git-credentials bearer, protocol v0.5.2), added at chat-start and removed on
// container exit. A session can register more than one secret - both features
// are independent and commonly coexist - so each session ID maps to a SET of
// keys, not a single value; a second AddSessionKey call for the same session
// must not displace the first.
//
// Worker stderr and unparsable stdout (e.g. a panic stack trace) reach the
// /logs stream with only this redactor applied - the in-worker redactor covers
// tool output and events but never sees worker stderr - so a per-session key
// missing here would leak on exactly the surface bridge masking exists for.
//
// Every mutation recomposes the whole union and atomically swaps the Bridge's
// redactor, so a concurrent add/remove for different sessions cannot clobber
// each other. Safe for concurrent use.
type RedactorRegistry struct {
	bridge *Bridge

	mu      sync.Mutex
	session map[string][]string
}

// NewRedactorRegistry builds a registry and immediately installs the initial
// (empty) redactor on bridge. Empty and trivially short values are dropped by
// redact.New.
func NewRedactorRegistry(bridge *Bridge) *RedactorRegistry {
	r := &RedactorRegistry{
		bridge:  bridge,
		session: make(map[string][]string),
	}
	r.rebuild()

	return r
}

// AddSessionKey appends key to sessionID's set of per-session secrets and
// rebuilds the redactor. An empty key is ignored, so callers may register
// unconditionally. Appending (not overwriting) is what lets a session register
// both an LLM key and a git-credentials bearer without the second call
// displacing the first from the redaction set.
func (r *RedactorRegistry) AddSessionKey(sessionID, key string) {
	if key == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.session[sessionID] = append(r.session[sessionID], key)
	r.rebuild()
}

// RemoveSessionKey forgets every secret registered for sessionID and rebuilds
// the redactor. Idempotent: removing an unregistered session is a no-op. Wire
// it to the container-exit cleanup so the session set does not grow without
// bound.
func (r *RedactorRegistry) RemoveSessionKey(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.session[sessionID]; !ok {
		return
	}

	delete(r.session, sessionID)
	r.rebuild()
}

// rebuild composes every registered per-session key into a fresh redactor and
// swaps it onto the bridge. The caller holds r.mu (except NewRedactorRegistry,
// which runs before the registry is shared). redact.New drops empty and
// trivially short values.
func (r *RedactorRegistry) rebuild() {
	all := make([]string, 0, len(r.session))
	for _, keys := range r.session {
		all = append(all, keys...)
	}

	r.bridge.SetRedactor(redact.New(all))
}
