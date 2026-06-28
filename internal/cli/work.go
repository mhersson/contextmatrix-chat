package cli

import (
	"github.com/mhersson/contextmatrix-chat/internal/chatwork"
	"github.com/spf13/cobra"
)

func newWorkCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "work",
		Short:  "Container entrypoint: execute one chat session under ContextMatrix control",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return chatwork.Run(cmd.Context())
		},
	}
}
