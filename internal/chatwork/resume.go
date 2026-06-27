// Package chatwork implements the work-loop for contextmatrix-chat container
// entrypoints.
package chatwork

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// SeedHistory maps a bounded ChatResumeContext into []llm.Message for seeding
// Config.History in the harness. Thinking turns are skipped; tool calls,
// tool results, stderr, and system turns fold into orientation messages.
// Returns nil for a nil rc or when all turns are filtered out.
func SeedHistory(rc *protocol.ChatResumeContext) []llm.Message {
	if rc == nil || len(rc.Turns) == 0 {
		return nil
	}

	msgs := make([]llm.Message, 0, len(rc.Turns))

	for _, t := range rc.Turns {
		if t.Content == "" {
			continue
		}

		switch t.Role {
		case "user":
			msgs = append(msgs, llm.Message{Role: "user", Content: t.Content})
		case "assistant_text", "assistant", "text":
			msgs = append(msgs, llm.Message{Role: "assistant", Content: t.Content})
		case "tool_call", "tool_result", "system", "stderr":
			msgs = append(msgs, llm.Message{Role: "system", Content: t.Content})
		}
	}

	if len(msgs) == 0 {
		return nil
	}

	return msgs
}

// LoadResume reads a newline-delimited JSON file (one ChatResumeTurn per line,
// as written by json.Encoder) and returns the assembled ChatResumeContext.
// Blank lines are skipped; a malformed line returns an error. A missing file
// is returned as an error so the caller can decide whether it is fatal.
func LoadResume(path string) (*protocol.ChatResumeContext, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open resume file %q: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	var turns []protocol.ChatResumeTurn

	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var t protocol.ChatResumeTurn

		if err := json.Unmarshal([]byte(line), &t); err != nil {
			return nil, fmt.Errorf("unmarshal resume turn: %w", err)
		}

		turns = append(turns, t)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan resume file: %w", err)
	}

	return &protocol.ChatResumeContext{Turns: turns}, nil
}
