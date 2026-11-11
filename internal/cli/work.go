package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/mhersson/contextmatrix-chat/internal/chatwork"
	"github.com/spf13/cobra"
)

func newWorkCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "work",
		Short:  "Container entrypoint: execute one chat session under ContextMatrix control",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := workContext(cmd.Context())
			defer stop()

			return chatwork.Run(ctx)
		},
	}
}

// workContext is cancelled by SIGTERM or SIGINT so a container stop unwinds
// the session and its children instead of killing the worker outright.
func workContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
}
