package cli

import (
	"errors"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/spf13/cobra"
)

func newWorkCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "work",
		Short:  "Container entrypoint: execute one chat session under ContextMatrix control",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if _, err := buildWorkConfig(); err != nil {
				return err
			}

			return errors.New("work: not implemented")
		},
	}
}

// buildWorkConfig constructs the harness.Config from the container environment.
func buildWorkConfig() (harness.Config, error) {
	return harness.Config{}, errors.New("buildWorkConfig: not implemented")
}
