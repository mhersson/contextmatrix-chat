package chatwork

import (
	_ "embed"
)

// chatSystemPrompt is injected as the system turn for every chat session. It
// orients the model as an interactive coding and board-aware assistant.
const chatSystemPrompt = `You are an interactive coding and conversation partner working in /workspace.
You have filesystem tools (read, write, edit, grep, glob, git, bash) rooted at /workspace,
and board tools for interacting with the ContextMatrix task board.
Work collaboratively: answer questions, explore code, implement changes, and update
board state when the human asks. Be concise and precise.`

// chatPrimer is the ContextMatrix orientation injected as the first user turn
// of every epoch - on cold open, on resume, and again after each /clear. It
// lives here, next to the environment it describes (tool set, /workspace
// paths), so the two cannot drift apart; the host sends nothing.
//
//go:embed primer.md
var chatPrimer string
