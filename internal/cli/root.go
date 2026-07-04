package cli

import "github.com/spf13/cobra"

// NewRootCmd builds the contextmatrix-chat CLI root.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "contextmatrix-chat",
		Short:         "ContextMatrix chat backend",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newServeCmd())
	root.AddCommand(newWorkCmd())
	root.AddCommand(newGitCredentialCmd())
	root.AddCommand(newGHWrapperCmd())

	return root
}
