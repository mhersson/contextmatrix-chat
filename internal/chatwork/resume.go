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
// Config.History in the harness. CM's transcript builder puts exactly four
// roles on the wire: "user", "assistant_text", "tool_call", and
// "tool_result_summary". User and assistant turns map directly; tool calls
// and tool-result summaries fold into system orientation messages. Any other
// role is skipped (forward compatibility). Returns nil for a nil rc or when
// all turns are filtered out.
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
		case "assistant_text":
			msgs = append(msgs, llm.Message{Role: "assistant", Content: t.Content})
		case "tool_call", "tool_result_summary":
			msgs = append(msgs, llm.Message{Role: "system", Content: t.Content})
		}
	}

	if len(msgs) == 0 {
		return nil
	}

	return msgs
}

// maxResumeLine is the per-line buffer cap for LoadResume. Resume turns can
// contain long model responses, so 1 MiB gives substantial headroom over the
// bufio default (64 KiB) without unbounded allocation.
const maxResumeLine = 1 << 20 // 1 MiB

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
	scanner.Buffer(make([]byte, 0, 64*1024), maxResumeLine)

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
