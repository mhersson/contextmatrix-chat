package cli

import "github.com/spf13/cobra"

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
