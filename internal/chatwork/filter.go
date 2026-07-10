package chatwork

import (
	"bytes"
	"encoding/json"
	"io"
)

// boardFilterWriter wraps dst and drops JSONL transcript lines whose kind is
// "tool_call" and whose data.name is a board (MCP bridge) tool. All other
// lines pass through unchanged. Write always returns len(p) so the emitter
// never sees a short-write error on filtered lines.
type boardFilterWriter struct {
	dst   io.Writer
	board map[string]bool
}

// newBoardFilterWriter returns a writer that silently drops board tool_call
// event lines from the transcript — the MCP bridge tools, named mcp__*.
func newBoardFilterWriter(dst io.Writer, boardToolNames []string) io.Writer {
	m := make(map[string]bool, len(boardToolNames))
	for _, n := range boardToolNames {
		m[n] = true
	}

	return &boardFilterWriter{dst: dst, board: m}
}

// Write splits p on newlines and writes each non-filtered line to dst.
func (w *boardFilterWriter) Write(p []byte) (int, error) {
	lines := bytes.SplitAfter(p, []byte{'\n'})

	for _, line := range lines {
		if len(line) == 0 {
			continue
		}

		if w.isFiltered(line) {
			continue
		}

		if _, err := w.dst.Write(line); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

// isFiltered reports whether line is a tool_call event for a board tool.
func (w *boardFilterWriter) isFiltered(line []byte) bool {
	if !bytes.Contains(line, []byte(`"tool_call"`)) {
		return false
	}

	var ev struct {
		Kind string `json:"kind"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}

	if err := json.Unmarshal(bytes.TrimRight(line, "\n\r"), &ev); err != nil {
		return false
	}

	return ev.Kind == "tool_call" && w.board[ev.Data.Name]
}
