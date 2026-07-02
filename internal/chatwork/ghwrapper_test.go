package chatwork

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallGHWrapper(t *testing.T) {
	t.Parallel()

	dir, err := installGHWrapper("/run/cm-secrets/env")
	require.NoError(t, err)

	path := filepath.Join(dir, "gh")

	script, err := os.ReadFile(path)
	require.NoError(t, err)

	s := string(script)
	assert.Contains(t, s, "grep '^CM_GIT_TOKEN=' '/run/cm-secrets/env'", "reads the token fresh per call")
	assert.Contains(t, s, "exec /usr/bin/gh \"$@\"", "execs the real gh by absolute path (no recursion)")
	assert.Contains(t, s, "export GH_TOKEN GH_ENTERPRISE_TOKEN", "exports the enterprise token so gh authenticates against GHES hosts")
	assert.NotContains(t, s, "ghp_", "no literal token embedded")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}
