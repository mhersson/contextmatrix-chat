package logbridge_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-chat/internal/logbridge"
	"github.com/mhersson/contextmatrix-harness/redact"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSession = "sess-abc123"

// makeEvent encodes one worker stdout JSONL line for a given kind and data.
func makeEvent(kind string, data map[string]any) []byte {
	ev := map[string]any{
		"seq":  1,
		"kind": kind,
		"time": time.Now().Format(time.RFC3339),
	}
	if data != nil {
		ev["data"] = data
	}

	b, _ := json.Marshal(ev)

	return b
}

// TestThinkingCase is the TDD centerpiece: a {"kind":"thinking",...} line must
// publish a protocol.LogEntry{Type:"thinking", Content:"hmm", SessionID:testSession}.
func TestThinkingCase(t *testing.T) {
	t.Parallel()

	hub := logbridge.NewHub()
	_, ch := hub.Subscribe("")
	bridge := logbridge.New(hub, nil)

	line := makeEvent("thinking", map[string]any{"content": "hmm"})
	bridge.BridgeLine(testSession, line, false)

	var got protocol.LogEntry
	select {
	case got = <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected thinking entry but got none (timeout)")
	}

	assert.Equal(t, "thinking", got.Type)
	assert.Equal(t, "hmm", got.Content)
	assert.Equal(t, testSession, got.SessionID)
	assert.False(t, got.Timestamp.IsZero(), "Timestamp must be set")
}

// TestMappingTable covers every row of the kind→LogEntry spec.
func TestMappingTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		line         []byte
		wantType     string // empty = expect skip
		wantModel    string
		wantToolID   string
		wantUsage    *protocol.LogTokenUsage
		checkContent func(t *testing.T, content string)
	}{
		{
			name: "model_response → text with content and model",
			line: makeEvent("model_response", map[string]any{
				"content": "Hello, world!",
				"model":   "test-model",
			}),
			wantType:  "text",
			wantModel: "test-model",
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Equal(t, "Hello, world!", content)
			},
		},
		{
			name: "thinking → thinking with content",
			line: makeEvent("thinking", map[string]any{
				"content": "let me think about this",
			}),
			wantType: "thinking",
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Equal(t, "let me think about this", content)
			},
		},
		{
			name: "tool_call → tool_call with id and formatted content",
			line: makeEvent("tool_call", map[string]any{
				"id":       "call_abc123",
				"name":     "bash",
				"raw_args": `{"cmd":"ls"}`,
			}),
			wantType:   "tool_call",
			wantToolID: "call_abc123",
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Equal(t, `bash({"cmd":"ls"})`, content)
			},
		},
		{
			name: "tool_call truncated at 200 chars",
			line: (func() []byte {
				bigArgs := `{"x":"` + strings.Repeat("a", 300) + `"}`

				return makeEvent("tool_call", map[string]any{
					"id":       "call_trunc",
					"name":     "bash",
					"raw_args": bigArgs,
				})
			})(),
			wantType:   "tool_call",
			wantToolID: "call_trunc",
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.LessOrEqual(t, len(content), 200)
			},
		},
		{
			name: "tool_call truncation is rune-safe on multi-byte content",
			line: (func() []byte {
				// "é" is 2 bytes; the prefix `bash({"x":"` is 11 bytes, so the
				// 200-byte cut lands mid-rune unless truncation backs off.
				bigArgs := `{"x":"` + strings.Repeat("é", 300) + `"}`

				return makeEvent("tool_call", map[string]any{
					"id":       "call_mb",
					"name":     "bash",
					"raw_args": bigArgs,
				})
			})(),
			wantType:   "tool_call",
			wantToolID: "call_mb",
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.LessOrEqual(t, len(content), 200)
				assert.True(t, utf8.ValidString(content), "truncated content must be valid UTF-8")
			},
		},
		{
			name: "state_change summary truncation is rune-safe on multi-byte content",
			line: makeEvent("state_change", map[string]any{
				// "世" is 3 bytes; the JSON prefix `{"warning":"` is 12 bytes,
				// so the 200-byte cut lands mid-rune.
				"warning": strings.Repeat("世", 150),
			}),
			wantType: "system",
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.LessOrEqual(t, len(content), 200)
				assert.True(t, utf8.ValidString(content), "summarized content must be valid UTF-8")
			},
		},
		{
			name: "usage → usage with token counts and model",
			line: makeEvent("usage", map[string]any{
				"prompt_tokens":     float64(100),
				"completion_tokens": float64(50),
				"model":             "usage-model",
			}),
			wantType:  "usage",
			wantModel: "usage-model",
			wantUsage: &protocol.LogTokenUsage{
				InputTokens:  100,
				OutputTokens: 50,
			},
		},
		{
			name: "state_change awaiting_human → skipped (normal idle between chat turns)",
			line: makeEvent("state_change", map[string]any{
				"state": "awaiting_human",
				"turns": float64(3),
			}),
			wantType: "", // skipped
		},
		{
			name: "state_change other → system",
			line: makeEvent("state_change", map[string]any{
				"stop":  "done",
				"turns": float64(5),
			}),
			wantType: "system",
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.NotEmpty(t, content)
			},
		},
		{
			name: "context_limit → system",
			line: makeEvent("context_limit", map[string]any{
				"prompt_tokens":  float64(80000),
				"context_window": float64(100000),
				"ratio":          float64(0.8),
				"threshold":      float64(0.85),
			}),
			wantType: "system",
		},
		{
			name: "error → stderr",
			line: makeEvent("error", map[string]any{
				"error": "something went wrong",
			}),
			wantType: "stderr",
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Contains(t, content, "something went wrong")
			},
		},
		{
			name:     "model_request → skipped",
			line:     makeEvent("model_request", map[string]any{"turn": float64(1)}),
			wantType: "",
		},
		{
			name:     "tool_result → skipped",
			line:     makeEvent("tool_result", map[string]any{"id": "call_x"}),
			wantType: "",
		},
		{
			name:     "tool_repair → skipped",
			line:     makeEvent("tool_repair", map[string]any{"id": "call_x"}),
			wantType: "",
		},
		{
			name:     "user_input → skipped",
			line:     makeEvent("user_input", map[string]any{"message_id": "m1"}),
			wantType: "",
		},
		{
			name:     "verification → skipped",
			line:     makeEvent("verification", map[string]any{}),
			wantType: "",
		},
		{
			name:     "unknown kind → skipped",
			line:     makeEvent("future_kind", map[string]any{"x": "y"}),
			wantType: "",
		},
		{
			name:     "unparsable line → stderr passthrough",
			line:     []byte("goroutine 1 [running]: panic: something bad happened"),
			wantType: "stderr",
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Equal(t, "goroutine 1 [running]: panic: something bad happened", content)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hub := logbridge.NewHub()
			_, ch := hub.Subscribe("")
			bridge := logbridge.New(hub, nil)

			bridge.BridgeLine(testSession, tt.line, false)

			if tt.wantType == "" {
				// Expect no publish.
				select {
				case e := <-ch:
					t.Errorf("expected skip but got entry with type=%q", e.Type)
				case <-time.After(30 * time.Millisecond):
				}

				return
			}

			var got protocol.LogEntry
			select {
			case got = <-ch:
			case <-time.After(100 * time.Millisecond):
				t.Fatal("expected entry but got none (timeout)")
			}

			assert.Equal(t, tt.wantType, got.Type, "Type mismatch")
			assert.Equal(t, testSession, got.SessionID, "SessionID mismatch")
			assert.False(t, got.Timestamp.IsZero(), "Timestamp must be set")

			if tt.wantModel != "" {
				assert.Equal(t, tt.wantModel, got.Model)
			}

			if tt.wantToolID != "" {
				assert.Equal(t, tt.wantToolID, got.ToolUseID)
			}

			if tt.wantUsage != nil {
				require.NotNil(t, got.Usage)
				assert.Equal(t, tt.wantUsage.InputTokens, got.Usage.InputTokens)
				assert.Equal(t, tt.wantUsage.OutputTokens, got.Usage.OutputTokens)
			}

			if tt.checkContent != nil {
				tt.checkContent(t, got.Content)
			}
		})
	}
}

// TestStderrStream verifies that isStderr=true produces a stderr frame with
// the raw line redacted.
func TestStderrStream(t *testing.T) {
	t.Parallel()

	const secret = "supersecrettoken"

	hub := logbridge.NewHub()
	_, ch := hub.Subscribe("")
	red := redact.New([]string{secret})
	bridge := logbridge.New(hub, red)

	rawLine := []byte("error: auth failed with " + secret)
	bridge.BridgeLine(testSession, rawLine, true)

	var got protocol.LogEntry
	select {
	case got = <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected entry but got none")
	}

	assert.Equal(t, "stderr", got.Type)
	assert.Equal(t, testSession, got.SessionID)
	assert.NotContains(t, got.Content, secret, "secret must be redacted")
	assert.Contains(t, got.Content, "[REDACTED]")
}

// TestRedaction ensures secrets never appear in bridged frames.
func TestRedaction(t *testing.T) {
	t.Parallel()

	const secret = "my-secret-api-key"

	hub := logbridge.NewHub()
	_, ch := hub.Subscribe("")
	red := redact.New([]string{secret})
	bridge := logbridge.New(hub, red)

	t.Run("model_response content redacted", func(t *testing.T) {
		line := makeEvent("model_response", map[string]any{
			"content": "The key is " + secret + " and it works",
			"model":   "m",
		})
		bridge.BridgeLine(testSession, line, false)

		select {
		case got := <-ch:
			assert.NotContains(t, got.Content, secret)
			assert.Contains(t, got.Content, "[REDACTED]")
		case <-time.After(100 * time.Millisecond):
			t.Fatal("expected entry")
		}
	})

	t.Run("thinking content redacted", func(t *testing.T) {
		line := makeEvent("thinking", map[string]any{
			"content": "should I use " + secret + " here?",
		})
		bridge.BridgeLine(testSession, line, false)

		select {
		case got := <-ch:
			assert.Equal(t, "thinking", got.Type)
			assert.NotContains(t, got.Content, secret)
			assert.Contains(t, got.Content, "[REDACTED]")
		case <-time.After(100 * time.Millisecond):
			t.Fatal("expected entry")
		}
	})

	t.Run("raw stderr redacted", func(t *testing.T) {
		bridge.BridgeLine(testSession, []byte("fatal: "+secret), true)

		select {
		case got := <-ch:
			assert.NotContains(t, got.Content, secret)
			assert.Contains(t, got.Content, "[REDACTED]")
		case <-time.After(100 * time.Millisecond):
			t.Fatal("expected entry")
		}
	})
}

// TestHubSubscribers verifies fan-out, session filtering, drop-on-full,
// and Unsubscribe.
func TestHubSubscribers(t *testing.T) {
	t.Parallel()

	t.Run("all-session subscriber receives entries from any session", func(t *testing.T) {
		t.Parallel()

		hub := logbridge.NewHub()
		_, ch := hub.Subscribe("") // empty = all
		hub.Publish(protocol.LogEntry{Type: "text", SessionID: "sess-a", Content: "hello"})

		select {
		case got := <-ch:
			assert.Equal(t, "sess-a", got.SessionID)
		case <-time.After(100 * time.Millisecond):
			t.Fatal("expected entry")
		}
	})

	t.Run("session-filtered subscriber only receives matching session", func(t *testing.T) {
		t.Parallel()

		hub := logbridge.NewHub()
		_, chA := hub.Subscribe("sess-a")
		_, chB := hub.Subscribe("sess-b")

		hub.Publish(protocol.LogEntry{Type: "text", SessionID: "sess-a", Content: "for a"})

		select {
		case got := <-chA:
			assert.Equal(t, "sess-a", got.SessionID)
		case <-time.After(100 * time.Millisecond):
			t.Fatal("sess-a subscriber did not receive")
		}

		// sess-b should not receive sess-a's entry.
		select {
		case e := <-chB:
			t.Errorf("sess-b received unexpected entry: %+v", e)
		case <-time.After(30 * time.Millisecond):
			// correct: no delivery
		}
	})

	t.Run("full subscriber drops without stalling", func(t *testing.T) {
		t.Parallel()

		hub := logbridge.NewHub()
		_, ch := hub.Subscribe("")

		// Publish more entries than the channel buffer without consuming.
		done := make(chan struct{})

		go func() {
			defer close(done)

			for i := range 300 {
				hub.Publish(protocol.LogEntry{Type: "text", Content: fmt.Sprintf("msg-%d", i)})
			}
		}()

		// The goroutine must complete without blocking.
		select {
		case <-done:
			// success
		case <-time.After(2 * time.Second):
			t.Fatal("Publish blocked on full subscriber")
		}

		_ = ch
	})

	t.Run("Unsubscribe closes channel", func(t *testing.T) {
		t.Parallel()

		hub := logbridge.NewHub()
		id, ch := hub.Subscribe("")
		hub.Unsubscribe(id)

		_, open := <-ch
		assert.False(t, open, "channel must be closed after Unsubscribe")
	})
}
