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
