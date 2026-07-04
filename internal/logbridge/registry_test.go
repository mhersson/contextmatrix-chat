package logbridge_test

import (
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-chat/internal/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
)

// recvEntry drains one published entry or fails on timeout.
func recvEntry(t *testing.T, ch <-chan protocol.LogEntry) protocol.LogEntry {
	t.Helper()

	select {
	case e := <-ch:
		return e
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected a bridged entry but got none (timeout)")

		return protocol.LogEntry{}
	}
}

// TestRedactorRegistry_SessionKeyMaskedThroughRotation is the regression guard
// for the CM-provisioned-key masking hole. Worker stderr and unparsable stdout
// are bridged host-side with ONLY the log-bridge redactor applied (the in-worker
// redactor never sees them), so a per-session LLM key must be masked there — and
// it must SURVIVE a GitHub-token rotation. The pre-fix OnRotate rebuilt the
// bridge redactor from config/token only and clobbered any registered session
// key; step (2) is that clobber regression.
func TestRedactorRegistry_SessionKeyMaskedThroughRotation(t *testing.T) {
	t.Parallel()

	const (
		configKey  = "config-llm-key-000000"
		sessionKey = "sk-session-provisioned-111111"
		gitToken   = "ghs-rotated-token-222222"
	)

	hub := logbridge.NewHub()
	_, ch := hub.Subscribe("")
	bridge := logbridge.New(hub, nil)

	registry := logbridge.NewRedactorRegistry(bridge, []string{configKey})

	// Before any session key is registered, the payload key is NOT masked.
	bridge.BridgeLine(testSession, []byte("boot "+sessionKey), true)
	assert.Contains(t, recvEntry(t, ch).Content, sessionKey,
		"an unregistered session key must not be masked yet")

	// (1) After chat-start registers the key, a stderr-path line is masked.
	registry.AddSessionKey(testSession, sessionKey)
	bridge.BridgeLine(testSession, []byte("panic: leaked "+sessionKey), true)

	got := recvEntry(t, ch)
	assert.NotContains(t, got.Content, sessionKey,
		"a registered session key must be masked on the stderr path")
	assert.Contains(t, got.Content, "[REDACTED]")

	// (2) After a token rotation the session key is STILL masked (clobber regression).
	registry.SetToken(gitToken)
	bridge.BridgeLine(testSession, []byte("after rotate "+sessionKey+" and "+gitToken), true)

	got = recvEntry(t, ch)
	assert.NotContains(t, got.Content, sessionKey,
		"a session key must survive a token rotation, not be clobbered by OnRotate")
	assert.NotContains(t, got.Content, gitToken,
		"the rotated token must also be masked")

	// The static config key is masked throughout.
	bridge.BridgeLine(testSession, []byte("cfg "+configKey), true)
	assert.NotContains(t, recvEntry(t, ch).Content, configKey)

	// (3) After session end the key is forgotten: a later line is NOT masked.
	registry.RemoveSessionKey(testSession)
	bridge.BridgeLine(testSession, []byte("ended "+sessionKey), true)
	assert.Contains(t, recvEntry(t, ch).Content, sessionKey,
		"a session key must no longer be masked after removal (registry no longer holds it)")

	// The rotating token remains masked after the session ends.
	bridge.BridgeLine(testSession, []byte("still "+gitToken), true)
	assert.NotContains(t, recvEntry(t, ch).Content, gitToken)
}

// TestRedactorRegistry_EmptyKeyIgnored verifies AddSessionKey ignores an empty
// key (a non-nil LLMEndpoint carrying no APIKey) so it never tracks a value that
// would widen nothing, and that a remove of an unregistered session is a no-op.
func TestRedactorRegistry_EmptyKeyIgnored(t *testing.T) {
	t.Parallel()

	hub := logbridge.NewHub()
	_, ch := hub.Subscribe("")
	bridge := logbridge.New(hub, nil)
	registry := logbridge.NewRedactorRegistry(bridge, nil)

	registry.AddSessionKey(testSession, "")
	registry.RemoveSessionKey(testSession) // unregistered → no-op, must not panic

	bridge.BridgeLine(testSession, []byte("plain line"), true)
	assert.Equal(t, "plain line", recvEntry(t, ch).Content)
}

// TestRedactorRegistry_MultipleSessionKeysBothMasked is the regression guard
// for the multi-secret-per-session clobber: a chat session commonly carries
// BOTH a CM-provisioned LLM key and a CM-provisioned git-credentials bearer
// (two independent multi-user-mode features), registered under the SAME
// session ID via two separate AddSessionKey calls. The second call must not
// silently displace the first — both must stay masked until the session ends,
// and RemoveSessionKey(sessionID) must forget both together.
func TestRedactorRegistry_MultipleSessionKeysBothMasked(t *testing.T) {
	t.Parallel()

	const (
		llmKey    = "sk-llm-session-key-000000"
		gitBearer = "sess1.git-credentials-bearer-111111"
	)

	hub := logbridge.NewHub()
	_, ch := hub.Subscribe("")
	bridge := logbridge.New(hub, nil)
	registry := logbridge.NewRedactorRegistry(bridge, nil)

	registry.AddSessionKey(testSession, llmKey)
	registry.AddSessionKey(testSession, gitBearer)

	bridge.BridgeLine(testSession, []byte("keys: "+llmKey+" and "+gitBearer), true)

	got := recvEntry(t, ch)
	assert.NotContains(t, got.Content, llmKey,
		"the first-registered key must still be masked after a second key is registered for the same session")
	assert.NotContains(t, got.Content, gitBearer,
		"the second-registered key must also be masked")

	registry.RemoveSessionKey(testSession)
	bridge.BridgeLine(testSession, []byte("after removal: "+llmKey+" and "+gitBearer), true)

	got = recvEntry(t, ch)
	assert.Contains(t, got.Content, llmKey, "removal must forget both keys, not just the last one")
	assert.Contains(t, got.Content, gitBearer, "removal must forget both keys, not just the last one")
}
