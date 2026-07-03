package logbridge

import (
	"slices"
	"sync"

	"github.com/mhersson/contextmatrix-harness/redact"
)

// RedactorRegistry composes the log-bridge redactor from a single source of
// truth so the masked-secret set stays consistent as secrets come and go. It
// unions three groups:
//
//   - static: process-lifetime secrets (the local-config LLM key).
//   - token: the rotating GitHub installation token, replaced on each rotation.
//   - session: per-session CM-provisioned LLM keys (protocol v0.5.0), added at
//     chat-start and removed on container exit.
//
// Worker stderr and unparsable stdout (e.g. a panic stack trace) reach the
// /logs stream with only this redactor applied — the in-worker redactor covers
// tool output and events but never sees worker stderr — so a per-session key
// missing here would leak on exactly the surface bridge masking exists for.
//
// Every mutation recomposes the whole union and atomically swaps the Bridge's
// redactor. Rebuilding from the union (never patching the previous redactor) is
// what lets a token rotation and a session add/remove coexist without one
// clobbering the other. Safe for concurrent use.
type RedactorRegistry struct {
	bridge *Bridge

	mu      sync.Mutex
	static  []string
	token   string
	session map[string]string
}

// NewRedactorRegistry builds a registry over the given static secrets and
// immediately installs the initial redactor (static only; no token or session
// keys yet) on bridge. Empty and trivially short values are dropped by
// redact.New.
func NewRedactorRegistry(bridge *Bridge, static []string) *RedactorRegistry {
	r := &RedactorRegistry{
		bridge:  bridge,
		static:  slices.Clone(static),
		session: make(map[string]string),
	}
	r.rebuild()

	return r
}

// SetToken records the current rotating GitHub token and rebuilds the redactor.
// Wire it to the secrets Refresher's OnRotate hook. An empty token contributes
// nothing to the set.
func (r *RedactorRegistry) SetToken(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.token = token
	r.rebuild()
}

// AddSessionKey records sessionID's per-session secret and rebuilds the
// redactor. An empty key is ignored, so callers may register unconditionally.
func (r *RedactorRegistry) AddSessionKey(sessionID, key string) {
	if key == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.session[sessionID] = key
	r.rebuild()
}

// RemoveSessionKey forgets sessionID's per-session secret and rebuilds the
// redactor. Idempotent: removing an unregistered session is a no-op. Wire it to
// the container-exit cleanup so the session set does not grow without bound.
func (r *RedactorRegistry) RemoveSessionKey(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.session[sessionID]; !ok {
		return
	}

	delete(r.session, sessionID)
	r.rebuild()
}

// rebuild composes the union of static secrets, the rotating token, and every
// registered per-session key into a fresh redactor and swaps it onto the
// bridge. The caller holds r.mu (except NewRedactorRegistry, which runs before
// the registry is shared). redact.New drops empty and trivially short values.
func (r *RedactorRegistry) rebuild() {
	all := make([]string, 0, len(r.static)+1+len(r.session))
	all = append(all, r.static...)

	if r.token != "" {
		all = append(all, r.token)
	}

	for _, key := range r.session {
		all = append(all, key)
	}

	r.bridge.SetRedactor(redact.New(all))
}
