package cli

import (
	"github.com/mhersson/contextmatrix-chat/internal/chatwork"
	"github.com/spf13/cobra"
)

// newGHWrapperCmd wires the hidden gh-wrapper subcommand: the `gh` PATH shim
// chatwork.installGHWrapperV2 writes execs
// `contextmatrix-chat gh-wrapper <gh's own args>`. It fetches a fresh per-repo
// git credential from CM and execs the real gh binary, replacing this
// process. Never invoked by a human. DisableFlagParsing is required - args
// are gh's own flags (e.g. -R, --repo), not this subcommand's.
func newGHWrapperCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "gh-wrapper",
		Short:              "Container-internal gh credential wrapper (invoked by the gh shim, not humans)",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return chatwork.RunGHWrapper(cmd.Context(), "", args)
		},
	}
}
