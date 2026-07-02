package chatwork

import (
	"fmt"
	"os"
	"path/filepath"
)

// realGHPath is the apt-installed gh binary in the worker image
// (docker/Dockerfile.worker installs it from GitHub's apt source). The shim
// execs it by absolute path so it never recurses into itself.
const realGHPath = "/usr/bin/gh"

// installGHWrapper writes a `gh` shim to a fresh dir and returns that dir for
// prepending to the bash tool's PATH. The shim reads CM_GIT_TOKEN from
// secretsEnvPath on every invocation and exports it as both GH_TOKEN
// (github.com and ghe.com) and GH_ENTERPRISE_TOKEN (GitHub Enterprise Server
// hosts — gh ignores GH_TOKEN there) before exec'ing the real gh, so
// long-lived chat sessions pick up rotated tokens (App installation tokens
// expire ~60m). git already rotates via the credential helper; gh has no
// equivalent hook, hence the shim.
func installGHWrapper(secretsEnvPath string) (string, error) {
	dir, err := os.MkdirTemp("", "cm-gh-bin-")
	if err != nil {
		return "", fmt.Errorf("create gh wrapper dir: %w", err)
	}

	script := fmt.Sprintf(`#!/bin/sh
GH_TOKEN=$(grep '^CM_GIT_TOKEN=' '%s' | cut -d= -f2-)
GH_ENTERPRISE_TOKEN="$GH_TOKEN"
export GH_TOKEN GH_ENTERPRISE_TOKEN
exec %s "$@"
`, secretsEnvPath, realGHPath)

	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("write gh wrapper: %w", err)
	}

	return dir, nil
}
