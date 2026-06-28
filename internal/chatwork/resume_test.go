package chatwork

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeedHistory(t *testing.T) {
	t.Run("nil rc returns nil", func(t *testing.T) {
		assert.Nil(t, SeedHistory(nil))
	})

	t.Run("empty turns returns nil", func(t *testing.T) {
		assert.Nil(t, SeedHistory(&protocol.ChatResumeContext{}))
	})

	t.Run("full role set mapped in order", func(t *testing.T) {
		rc := &protocol.ChatResumeContext{
			Turns: []protocol.ChatResumeTurn{
				{Seq: 1, Role: "user", Content: "hello"},
				{Seq: 2, Role: "assistant_text", Content: "hi there"},
				{Seq: 3, Role: "assistant_thinking", Content: "thinking..."},
				{Seq: 4, Role: "tool_call", Content: `{"tool":"read_file"}`},
				{Seq: 5, Role: "tool_result", Content: "file contents"},
				{Seq: 6, Role: "system", Content: "system note"},
			},
		}

		msgs := SeedHistory(rc)

		require.Len(t, msgs, 5)
		assert.Equal(t, "user", msgs[0].Role)
		assert.Equal(t, "hello", msgs[0].Content)
		assert.Equal(t, "assistant", msgs[1].Role)
		assert.Equal(t, "hi there", msgs[1].Content)
		assert.Equal(t, "system", msgs[2].Role)
		assert.JSONEq(t, `{"tool":"read_file"}`, msgs[2].Content)
		assert.Equal(t, "system", msgs[3].Role)
		assert.Equal(t, "file contents", msgs[3].Content)
		assert.Equal(t, "system", msgs[4].Role)
		assert.Equal(t, "system note", msgs[4].Content)
	})

	t.Run("empty content turns dropped", func(t *testing.T) {
		rc := &protocol.ChatResumeContext{
			Turns: []protocol.ChatResumeTurn{
				{Seq: 1, Role: "user", Content: ""},
				{Seq: 2, Role: "assistant_text", Content: ""},
			},
		}

		assert.Nil(t, SeedHistory(rc))
	})

	t.Run("assistant_thinking and thinking skipped", func(t *testing.T) {
		rc := &protocol.ChatResumeContext{
			Turns: []protocol.ChatResumeTurn{
				{Seq: 1, Role: "assistant_thinking", Content: "internal reasoning"},
				{Seq: 2, Role: "thinking", Content: "more reasoning"},
				{Seq: 3, Role: "user", Content: "question"},
			},
		}

		msgs := SeedHistory(rc)

		require.Len(t, msgs, 1)
		assert.Equal(t, "user", msgs[0].Role)
		assert.Equal(t, "question", msgs[0].Content)
	})

	t.Run("unknown role skipped forward-compatible", func(t *testing.T) {
		rc := &protocol.ChatResumeContext{
			Turns: []protocol.ChatResumeTurn{
				{Seq: 1, Role: "future_role", Content: "some content"},
				{Seq: 2, Role: "user", Content: "hello"},
			},
		}

		msgs := SeedHistory(rc)

		require.Len(t, msgs, 1)
		assert.Equal(t, "user", msgs[0].Role)
	})

	t.Run("assistant and text aliases map to assistant", func(t *testing.T) {
		rc := &protocol.ChatResumeContext{
			Turns: []protocol.ChatResumeTurn{
				{Seq: 1, Role: "assistant", Content: "raw assistant"},
				{Seq: 2, Role: "text", Content: "raw text"},
			},
		}

		msgs := SeedHistory(rc)

		require.Len(t, msgs, 2)
		assert.Equal(t, "assistant", msgs[0].Role)
		assert.Equal(t, "raw assistant", msgs[0].Content)
		assert.Equal(t, "assistant", msgs[1].Role)
		assert.Equal(t, "raw text", msgs[1].Content)
	})

	t.Run("stderr folds to system", func(t *testing.T) {
		rc := &protocol.ChatResumeContext{
			Turns: []protocol.ChatResumeTurn{
				{Seq: 1, Role: "stderr", Content: "error output"},
			},
		}

		msgs := SeedHistory(rc)

		require.Len(t, msgs, 1)
		assert.Equal(t, "system", msgs[0].Role)
		assert.Equal(t, "error output", msgs[0].Content)
	})
}

func TestLoadResume(t *testing.T) {
	t.Run("round-trip jsonl", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "resume.jsonl")

		turns := []protocol.ChatResumeTurn{
			{Seq: 1, Role: "user", Content: "hello"},
			{Seq: 2, Role: "assistant_text", Content: "hi"},
		}

		f, err := os.Create(path)
		require.NoError(t, err)

		enc := json.NewEncoder(f)

		for _, turn := range turns {
			require.NoError(t, enc.Encode(turn))
		}

		require.NoError(t, f.Close())

		rc, err := LoadResume(path)
		require.NoError(t, err)
		require.NotNil(t, rc)
		require.Len(t, rc.Turns, 2)
		assert.Equal(t, turns[0], rc.Turns[0])
		assert.Equal(t, turns[1], rc.Turns[1])
	})

	t.Run("blank lines skipped", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "resume.jsonl")

		content := "{\"seq\":1,\"role\":\"user\",\"content\":\"first\"}\n\n{\"seq\":2,\"role\":\"assistant_text\",\"content\":\"second\"}\n"
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

		rc, err := LoadResume(path)
		require.NoError(t, err)
		require.Len(t, rc.Turns, 2)
		assert.Equal(t, "first", rc.Turns[0].Content)
		assert.Equal(t, "second", rc.Turns[1].Content)
	})

	t.Run("missing file returns error", func(t *testing.T) {
		_, err := LoadResume("/no/such/file.jsonl")
		assert.ErrorContains(t, err, "open resume file")
	})

	t.Run("malformed line returns error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "resume.jsonl")

		require.NoError(t, os.WriteFile(path, []byte("not json\n"), 0o644))

		_, err := LoadResume(path)
		assert.ErrorContains(t, err, "unmarshal resume turn")
	})

	t.Run("turn larger than 64KB succeeds", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "resume.jsonl")

		// Build a content string that exceeds the default 64 KiB scanner cap.
		largeContent := strings.Repeat("x", 80*1024) // 80 KiB

		turns := []protocol.ChatResumeTurn{
			{Seq: 1, Role: "assistant_text", Content: largeContent},
		}

		f, err := os.Create(path)
		require.NoError(t, err)

		enc := json.NewEncoder(f)
		for _, turn := range turns {
			require.NoError(t, enc.Encode(turn))
		}

		require.NoError(t, f.Close())

		rc, err := LoadResume(path)
		require.NoError(t, err)
		require.Len(t, rc.Turns, 1)
		assert.Equal(t, largeContent, rc.Turns[0].Content)
	})

	t.Run("load then seed round-trip", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "resume.jsonl")

		turns := []protocol.ChatResumeTurn{
			{Seq: 1, Role: "user", Content: "question"},
			{Seq: 2, Role: "assistant_text", Content: "answer"},
			{Seq: 3, Role: "assistant_thinking", Content: "thinking"},
		}

		f, err := os.Create(path)
		require.NoError(t, err)

		enc := json.NewEncoder(f)

		for _, turn := range turns {
			require.NoError(t, enc.Encode(turn))
		}

		require.NoError(t, f.Close())

		rc, err := LoadResume(path)
		require.NoError(t, err)

		msgs := SeedHistory(rc)
		require.Len(t, msgs, 2)
		assert.Equal(t, "user", msgs[0].Role)
		assert.Equal(t, "question", msgs[0].Content)
		assert.Equal(t, "assistant", msgs[1].Role)
		assert.Equal(t, "answer", msgs[1].Content)
	})
}
