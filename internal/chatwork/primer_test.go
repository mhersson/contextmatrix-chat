package chatwork

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatSystemPrompt_NonEmpty(t *testing.T) {
	t.Parallel()

	assert.NotEmpty(t, chatSystemPrompt)
}

func TestReadPrimer(t *testing.T) {
	t.Parallel()

	t.Run("reads and trims content", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		path := filepath.Join(dir, "primer.txt")

		require.NoError(t, os.WriteFile(path, []byte("  implement the feature\n\n"), 0o644))

		got := readPrimer(path)
		assert.Equal(t, "implement the feature", got)
	})

	t.Run("missing file returns empty string", func(t *testing.T) {
		t.Parallel()

		got := readPrimer("/no/such/primer.txt")
		assert.Empty(t, got)
	})
}
