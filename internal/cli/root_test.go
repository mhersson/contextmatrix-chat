package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRootCmd(t *testing.T) {
	cmd := NewRootCmd()

	require.NotNil(t, cmd)
	assert.Equal(t, "contextmatrix-chat", cmd.Use)

	names := make(map[string]bool)

	for _, sub := range cmd.Commands() {
		names[sub.Use] = true
	}

	assert.True(t, names["serve"], "expected serve subcommand")
	assert.True(t, names["work"], "expected work subcommand")
}
