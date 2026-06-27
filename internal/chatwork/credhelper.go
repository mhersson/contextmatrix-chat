package chatwork

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ConfigureGitCredentialHelper writes a credential-helper script alongside
// the secrets env file and registers it as the global git credential helper.
// The script reads CM_GIT_TOKEN from secretsEnvPath on every git auth call,
// so token rotation is transparent — the token is never embedded in the
// script or git config; only the path to the env file is baked in.
func ConfigureGitCredentialHelper(ctx context.Context, secretsEnvPath string) error {
	scriptPath := filepath.Join(filepath.Dir(secretsEnvPath), "cm-git-credential-helper.sh")

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

	if err := exec.CommandContext(ctx, "git", "config", "--global", "credential.helper", scriptPath).Run(); err != nil {
		return fmt.Errorf("git config credential.helper: %w", err)
	}

	if err := exec.CommandContext(ctx, "git", "config", "--global", "credential.https://github.com.useHttpPath", "false").Run(); err != nil {
		return fmt.Errorf("git config useHttpPath: %w", err)
	}

	return nil
}
