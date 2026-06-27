package frames

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	var sb strings.Builder

	require.NoError(t, Write(&sb, Frame{Type: TypeUserMessage, Content: "hi there", MessageID: "m1"}))
	require.NoError(t, Write(&sb, Frame{Type: TypeClear}))

	r := NewReader(strings.NewReader(sb.String()))

	f1, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, Frame{Type: TypeUserMessage, Content: "hi there", MessageID: "m1"}, f1)

	f2, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, TypeClear, f2.Type)

	_, err = r.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestReaderSkipsGarbageAndUnknownTypes(t *testing.T) {
	in := "not json\n{\"type\":\"future_thing\"}\n{\"type\":\"user_message\",\"content\":\"ok\"}\n"
	r := NewReader(strings.NewReader(in))

	f, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, "ok", f.Content) // garbage + unknown types skipped, not fatal
}

func TestReaderOversizedLineIsFatal(t *testing.T) {
	in := strings.Repeat("a", maxLine+1) + "\n"
	r := NewReader(strings.NewReader(in))

	_, err := r.Next()
	require.Error(t, err)
	require.NotErrorIs(t, err, io.EOF)
}
