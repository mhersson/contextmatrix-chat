package cli

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkContextCancelsOnSIGTERM(t *testing.T) {
	ctx, stop := workContext(t.Context())
	defer stop()

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled by SIGTERM")
	}

	assert.Error(t, context.Cause(ctx))
}
