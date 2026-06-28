package chatwork

import (
	"log/slog"
	"os"
	"strings"
)

// chatSystemPrompt is injected as the system turn for every chat session. It
// orients the model as an interactive coding and board-aware assistant.
const chatSystemPrompt = `You are an interactive coding and conversation partner working in /workspace.
You have filesystem tools (read, write, edit, grep, glob, git, bash) rooted at /workspace,
and board tools for interacting with the ContextMatrix task board.
Work collaboratively: answer questions, explore code, implement changes, and update
board state when the human asks. Be concise and precise.`

// readPrimer reads the primer file at path and returns its trimmed content.
// A missing or unreadable file is logged and treated as an empty task — it
// must not kill the session.
func readPrimer(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("read primer failed; using empty task", "path", path, "error", err)

		return ""
	}

	return strings.TrimSpace(string(data))
}
