package chatwork

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// boardFilterWriter wraps dst and drops JSONL transcript lines whose kind is
// "tool_call" and whose data.name is a board (MCP bridge) tool. All other
// lines pass through unchanged. Write always returns len(p) so the emitter
// never sees a short-write error on filtered lines.
type boardFilterWriter struct {
	dst     io.Writer
	isBoard func(name string) bool
}

// newBoardFilterWriter returns a writer that silently drops tool_call event
// lines whose tool name isBoard accepts from the transcript.
func newBoardFilterWriter(dst io.Writer, isBoard func(name string) bool) io.Writer {
	return &boardFilterWriter{dst: dst, isBoard: isBoard}
}

// boardToolSet matches the exact names of the MCP bridge tools.
func boardToolSet(names []string) func(string) bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}

	return func(name string) bool { return m[name] }
}

// mcpServerTools matches Claude Code's qualified names for one MCP server's
// tools (mcp__<server>__<tool>).
func mcpServerTools(server string) func(string) bool {
	prefix := "mcp__" + server + "__"

	return func(name string) bool { return strings.HasPrefix(name, prefix) }
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

	return ev.Kind == "tool_call" && w.isBoard(ev.Data.Name)
}
