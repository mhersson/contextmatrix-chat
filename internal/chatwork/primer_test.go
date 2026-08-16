package chatwork

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestChatPrimer_OrientsToWorkspace pins the environment-coupled facts the
// embedded primer exists to keep in sync with the code: the tool root and the
// clone-target convention (see cloneTarget). A primer that drifts from these
// sends the model to the wrong directory on its first tool call.
func TestChatPrimer_OrientsToWorkspace(t *testing.T) {
	t.Parallel()

	assert.NotEmpty(t, chatPrimer)
	assert.Contains(t, chatPrimer, "`/workspace`", "the primer must name the real tool root")
	assert.Contains(t, chatPrimer, "`/workspace/<project>`", "clone guidance must match cloneTarget's convention")
}

// TestChatPrimer_GreetsOnFreshStart pins the opening-turn contract: a cold
// open or post-/clear epoch (no prior conversation) must produce a real
// reply, not a scripted non-response, while a resume (prior conversation
// present) must still route through the rehydration summary tool rather than
// a plain-text reply.
func TestChatPrimer_GreetsOnFreshStart(t *testing.T) {
	t.Parallel()

	assert.NotContains(t, chatPrimer, "Acknowledge silently", "a fresh-start epoch must greet the user instead of staying silent")
	assert.NotContains(t, chatPrimer, "Read it silently", "a fresh-start epoch must greet the user instead of staying silent")
	assert.Contains(t, chatPrimer, "chat_rehydration_complete", "resume must still route through the rehydration summary tool, not a plain reply")
}
