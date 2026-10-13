package chatwork

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// will silently leave it unsubstituted.
func TestChatPrimer_HasSessionPlaceholder(t *testing.T) {
	t.Parallel()

	assert.Contains(t, chatPrimer, "{{SESSION_ID}}", "the raw embed must contain the {{SESSION_ID}} placeholder")
}

// TestRenderPrimer_SubstitutesSessionID verifies that renderPrimer replaces
// the placeholder with the provided session ID and leaves no surviving token.
func TestRenderPrimer_SubstitutesSessionID(t *testing.T) {
	t.Parallel()

	result := renderPrimer("SOMEID")
	assert.NotContains(t, result, "{{SESSION_ID}}", "rendered primer must have no placeholder tokens")
	assert.Contains(t, result, "SOMEID", "rendered primer must contain the substituted session id")
}

// TestRenderPrimer_EmptySessionID verifies that an empty session ID leaves no
// placeholder token in the rendered primer (strings.ReplaceAll with an empty
// replacement still removes the token).
func TestRenderPrimer_EmptySessionID(t *testing.T) {
	t.Parallel()

	result := renderPrimer("")
	assert.NotContains(t, result, "{{SESSION_ID}}", "even with an empty session id, no placeholder token may survive")
}
