package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the chat backend: host ContextMatrix chat sessions",
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("serve: not implemented")
		},
	}

	return cmd
}
