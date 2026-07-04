package cli

import (
	"github.com/mhersson/contextmatrix-chat/internal/chatwork"
	"github.com/spf13/cobra"
)

// newGitCredentialCmd wires the hidden git-credential helper subcommand: git
// invokes it as `contextmatrix-chat git-credential <get|store|erase>`,
// speaking git's credential-helper key=value protocol on stdin/stdout. Never
// invoked by a human — chatwork.ConfigureGitCredentialHelperV2 registers a
// tiny shell script that execs this subcommand as the GLOBAL git
// credential.helper when CM provisions per-session git credentials (protocol
// v0.5.2).
func newGitCredentialCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-credential <get|store|erase>",
		Short:  "Container-internal git credential helper (invoked by git, not humans)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return chatwork.RunGitCredentialHelper(cmd.Context(), args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
