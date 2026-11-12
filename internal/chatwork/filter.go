package chatwork

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// boardFilterWriter wraps dst and drops JSONL transcript lines whose kind is
// "tool_call" and whose data.name the isBoard predicate accepts. With dropStop
// it also drops state_change lines carrying a data.stop key. All other
// lines pass through unchanged. Write always returns len(p) so the emitter
// never sees a short-write error on filtered lines.
type boardFilterWriter struct {
	dst     io.Writer
	isBoard func(name string) bool
	// dropStop drops the per-process stop line the claude session emits.
	dropStop bool
}

// newBoardFilterWriter returns a writer that silently drops tool_call event
// lines whose tool name isBoard accepts from the transcript.
func newBoardFilterWriter(dst io.Writer, isBoard func(name string) bool) io.Writer {
	return &boardFilterWriter{dst: dst, isBoard: isBoard}
}

// newClaudeCodeFilterWriter is newBoardFilterWriter that also drops the
// state_change stop line emitted at every claude process end.
func newClaudeCodeFilterWriter(dst io.Writer, isBoard func(name string) bool) io.Writer {
	return &boardFilterWriter{dst: dst, isBoard: isBoard, dropStop: true}
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

// isFiltered reports whether line is a board tool_call event or, with
// dropStop, a state_change stop line.
func (w *boardFilterWriter) isFiltered(line []byte) bool {
	if w.dropStop && bytes.Contains(line, []byte(`"stop"`)) && isStopLine(line) {
		return true
	}

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

// isStopLine reports whether line is a state_change event whose data has a
// stop key.
func isStopLine(line []byte) bool {
	var ev struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}

	if err := json.Unmarshal(bytes.TrimRight(line, "\n\r"), &ev); err != nil {
		return false
	}

	_, stop := ev.Data["stop"]

	return ev.Kind == "state_change" && stop
}
