package chatwork

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ConfigureGitCredentialHelper writes a credential-helper script to os.TempDir()
// and registers it as the git credential helper scoped to host (defaulting to
// github.com when host is empty).
// The script reads CM_GIT_TOKEN from secretsEnvPath on every git auth call,
// so token rotation is transparent — the token is never embedded in the
// script or git config; only the path to the env file is baked in.
// The script is written to os.TempDir() (not alongside secretsEnvPath) because
// the secrets mount is read-only in the container.
func ConfigureGitCredentialHelper(ctx context.Context, secretsEnvPath, host string) error {
	if host == "" {
		host = "github.com"
	}

	scriptPath := filepath.Join(os.TempDir(), "cm-git-credential-helper.sh")

	// Path is baked in; token is read fresh on each git auth call.
	script := fmt.Sprintf(`#!/bin/sh
SECRETS_ENV='%s'

case "$1" in
    get)
        token=$(grep '^CM_GIT_TOKEN=' "$SECRETS_ENV" | cut -d= -f2-)
        echo "username=x-access-token"
        echo "password=$token"
        ;;
esac
`, secretsEnvPath)

	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return fmt.Errorf("write credential helper script: %w", err)
	}

	// Scope the helper to the expected host only. A global credential.helper
	// would offer the token to ANY https host git contacts (malicious submodule
	// URL, redirect), leaking a live installation token off-platform.
	scope := "credential.https://" + host
	if err := exec.CommandContext(ctx, "git", "config", "--global", scope+".helper", scriptPath).Run(); err != nil { //nolint:gosec // G204: host is derived from the operator-supplied CM_CHAT_REPO_URL (or defaults to github.com)
		return fmt.Errorf("git config credential helper: %w", err)
	}

	if err := exec.CommandContext(ctx, "git", "config", "--global", scope+".useHttpPath", "false").Run(); err != nil { //nolint:gosec // G204: host is derived from the operator-supplied CM_CHAT_REPO_URL (or defaults to github.com)
		return fmt.Errorf("git config useHttpPath: %w", err)
	}

	return nil
}
