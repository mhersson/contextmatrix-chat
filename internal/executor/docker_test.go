package executor

import (
	"bytes"
	"testing"

	"github.com/docker/docker/api/types/image"
	"github.com/mhersson/contextmatrix-backendkit/webhookcore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerConfig_StdinAndImage(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		SessionID: "sess-abc",
		Image:     "alpine:3",
	})

	assert.Equal(t, "alpine:3", cfg.Image)
	assert.True(t, cfg.OpenStdin, "OpenStdin must be set so /message frames can be written")
	assert.True(t, cfg.AttachStdin)
	assert.False(t, cfg.StdinOnce, "stdin stays open for the container's life")
	assert.False(t, cfg.Tty)
}

func TestContainerConfig_Labels(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		SessionID: "sess-abc",
		Image:     "alpine:3",
	})

	assert.Equal(t, "true", cfg.Labels[labelChat])
	assert.Equal(t, "sess-abc", cfg.Labels[labelSession])
}

func TestContainerConfig_EnvPassthrough(t *testing.T) {
	env := []string{"FOO=bar", "BAZ=qux"}

	cfg, _ := containerConfig(LaunchSpec{
		SessionID: "sess-abc",
		Image:     "alpine:3",
		Env:       env,
	})

	assert.Equal(t, env, cfg.Env)
}

func TestContainerConfig_HostConfigResourcesAndHardening(t *testing.T) {
	const (
		mem  = int64(8 * 1024 * 1024 * 1024)
		pids = int64(512)
	)

	binds := []string{"/host/primer:/run/cm-primer:ro"}

	_, host := containerConfig(LaunchSpec{
		SessionID:   "sess-abc",
		Image:       "alpine:3",
		Binds:       binds,
		MemoryBytes: mem,
		PidsLimit:   pids,
	})

	assert.Equal(t, mem, host.Memory)

	require.NotNil(t, host.PidsLimit)
	assert.Equal(t, pids, *host.PidsLimit)

	assert.Equal(t, []string{"ALL"}, []string(host.CapDrop))
	assert.Equal(t, []string{"no-new-privileges"}, host.SecurityOpt)
	assert.Equal(t, binds, host.Binds)
}

func TestContainerConfig_RunsAsNonRoot(t *testing.T) {
	cfg, _ := containerConfig(LaunchSpec{
		SessionID: "sess-abc",
		Image:     "alpine:3",
	})

	assert.Equal(t, "1000:1000", cfg.User)
}

func TestContainerConfig_NoBindsWhenEmpty(t *testing.T) {
	_, host := containerConfig(LaunchSpec{
		SessionID: "sess-abc",
		Image:     "alpine:3",
	})

	assert.Empty(t, host.Binds)
}

func TestContainerName_Sanitized(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		want      string
	}{
		{
			name:      "lowercases session id",
			sessionID: "ABC-123",
			want:      "cm-chat-abc-123",
		},
		{
			name:      "sanitizes disallowed chars",
			sessionID: "my/session@v2",
			want:      "cm-chat-my-session-v2",
		},
		{
			name:      "spaces become dashes",
			sessionID: "team alpha",
			want:      "cm-chat-team-alpha",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, containerName(tt.sessionID))
		})
	}
}

// DockerExecutor must satisfy the Executor seam.
var _ Executor = (*DockerExecutor)(nil)

func TestLineWriter_SplitsOnNewline(t *testing.T) {
	var got []string

	w := newLineWriter(func(line []byte) {
		got = append(got, string(line))
	})

	n, err := w.Write([]byte("alpha\nbeta\n"))
	require.NoError(t, err)
	assert.Equal(t, len("alpha\nbeta\n"), n)
	assert.Equal(t, []string{"alpha", "beta"}, got)
}

func TestLineWriter_PartialLineHeldUntilFlush(t *testing.T) {
	var got []string

	w := newLineWriter(func(line []byte) {
		got = append(got, string(line))
	})

	_, _ = w.Write([]byte("hel"))
	_, _ = w.Write([]byte("lo\nwor"))

	assert.Equal(t, []string{"hello"}, got, "complete line emitted, partial held")

	w.Flush()
	assert.Equal(t, []string{"hello", "wor"}, got, "flush emits the trailing partial line")
}

func TestLineWriter_FlushOnEmptyBufferIsNoop(t *testing.T) {
	called := false

	w := newLineWriter(func([]byte) { called = true })
	w.Flush()

	assert.False(t, called)
}

func TestLineWriter_TrimsCarriageReturn(t *testing.T) {
	var got []string

	w := newLineWriter(func(line []byte) {
		got = append(got, string(line))
	})

	_, _ = w.Write([]byte("windows\r\nline\r\n"))

	assert.Equal(t, []string{"windows", "line"}, got)
}

func TestLineWriter_BoundsLongLine(t *testing.T) {
	var got []byte

	w := newLineWriter(func(line []byte) {
		got = append([]byte(nil), line...)
	})

	huge := bytes.Repeat([]byte("x"), scannerBufferMax+4096)
	_, _ = w.Write(huge)
	w.Flush()

	assert.LessOrEqual(t, len(got), scannerBufferMax,
		"line buffer must not grow past the cap")
}

func TestImageSummaries_SkipsDanglingAndMapsFields(t *testing.T) {
	in := []image.Summary{
		{
			RepoTags:    []string{"contextmatrix-chat-worker:go-node"},
			RepoDigests: []string{"contextmatrix-chat-worker@sha256:abc"},
			Created:     1750000000,
			Size:        2_560_000_000,
		},
		{RepoTags: nil, RepoDigests: []string{"orphan@sha256:def"}},     // dangling: skipped
		{RepoTags: []string{"<none>:<none>"}},                           // dangling tag form: skipped
		{RepoTags: []string{"other:latest", "<none>:<none>"}, Size: 42}, // <none> pruned, image kept
	}

	got := imageSummaries(in)

	require.Len(t, got, 2)
	assert.Equal(t, webhookcore.ImageSummary{
		Tags:      []string{"contextmatrix-chat-worker:go-node"},
		Digests:   []string{"contextmatrix-chat-worker@sha256:abc"},
		CreatedAt: 1750000000,
		SizeBytes: 2_560_000_000,
	}, got[0])
	assert.Equal(t, []string{"other:latest"}, got[1].Tags)
	assert.Equal(t, int64(42), got[1].SizeBytes)
}
