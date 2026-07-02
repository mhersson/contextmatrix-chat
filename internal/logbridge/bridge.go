// Package logbridge converts worker event JSONL into protocol.LogEntry
// frames and fans them out to /logs SSE subscribers, keyed by session.
package logbridge

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-harness/redact"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

const subBufSize = 256

// sub is one active subscriber.
type sub struct {
	ch        chan protocol.LogEntry
	sessionID string // empty = all sessions
}

// DropObserver is notified once per LogEntry dropped because a subscriber's
// channel was full. The serve layer supplies a Prometheus-backed adapter; the
// interface keeps logbridge free of any metrics dependency.
type DropObserver interface {
	ObserveDrop()
}

// Hub fans out LogEntry frames to registered subscribers.
// mu protects subs and nextID.
type Hub struct {
	mu           sync.Mutex
	subs         map[int]*sub
	nextID       int
	dropObserver DropObserver
}

// NewHub creates a ready Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[int]*sub)}
}

// NewHubWithDropObserver creates a Hub that notifies obs each time a full
// subscriber channel forces a drop. A nil obs behaves like NewHub.
func NewHubWithDropObserver(obs DropObserver) *Hub {
	return &Hub{subs: make(map[int]*sub), dropObserver: obs}
}

// Subscribe registers a subscriber. An empty sessionID string receives all
// entries regardless of session. Returns an opaque id for Unsubscribe.
func (h *Hub) Subscribe(sessionID string) (int, <-chan protocol.LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	id := h.nextID
	ch := make(chan protocol.LogEntry, subBufSize)
	h.subs[id] = &sub{ch: ch, sessionID: sessionID}

	return id, ch
}

// Unsubscribe removes the subscriber and closes its channel.
func (h *Hub) Unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if s, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(s.ch)
	}
}

// Publish delivers e to all matching subscribers. Per-subscriber delivery is
// non-blocking: a full channel is silently dropped.
func (h *Hub) Publish(e protocol.LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, s := range h.subs {
		if s.sessionID != "" && s.sessionID != e.SessionID {
			continue
		}

		select {
		case s.ch <- e:
		default:
			if h.dropObserver != nil {
				h.dropObserver.ObserveDrop()
			}
		}
	}
}

// Bridge maps one worker output line to zero or one published LogEntry.
type Bridge struct {
	hub      *Hub
	redactor atomic.Pointer[redact.Redactor]
}

// New creates a Bridge. r may be nil (no redaction).
func New(hub *Hub, r *redact.Redactor) *Bridge {
	b := &Bridge{hub: hub}
	b.redactor.Store(r)

	return b
}

// SetRedactor atomically swaps the redactor used for all lines bridged after
// the call. Safe for concurrent use with BridgeLine — intended to be called
// from the secrets Refresher's OnRotate hook so a rotated GitHub token is
// masked in bridged logs the instant the host mints it, without a restart.
func (b *Bridge) SetRedactor(r *redact.Redactor) {
	b.redactor.Store(r)
}

// BridgeLine maps one worker output line (stdout JSONL event or raw stderr)
// to zero or one published LogEntry, stamped with sessionID/time.Now().
func (b *Bridge) BridgeLine(sessionID string, line []byte, isStderr bool) {
	if isStderr {
		b.hub.Publish(protocol.LogEntry{
			Timestamp: time.Now(),
			SessionID: sessionID,
			Type:      "stderr",
			Content:   b.redactor.Load().Apply(string(line)),
		})

		return
	}

	var ev struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		// Unparsable (e.g. panic stack trace) — surface as stderr.
		b.hub.Publish(protocol.LogEntry{
			Timestamp: time.Now(),
			SessionID: sessionID,
			Type:      "stderr",
			Content:   b.redactor.Load().Apply(string(line)),
		})

		return
	}

	entry, skip := b.mapEvent(ev.Kind, ev.Data)
	if skip {
		return
	}

	entry.Timestamp = time.Now()
	entry.SessionID = sessionID
	entry.Content = b.redactor.Load().Apply(entry.Content)

	b.hub.Publish(entry)
}

// mapEvent converts a parsed event kind+data into a LogEntry.
// Returns skip=true for kinds that are deliberately not bridged.
func (b *Bridge) mapEvent(kind string, data map[string]any) (entry protocol.LogEntry, skip bool) {
	switch kind {
	case "model_response":
		content := strField(data, "content")
		if strings.TrimSpace(content) == "" {
			// Pure tool-call turn — no text to show; skip the empty frame.
			return protocol.LogEntry{}, true
		}

		return protocol.LogEntry{
			Type:    "text",
			Content: content,
			Model:   strField(data, "model"),
		}, false

	case "thinking":
		return protocol.LogEntry{
			Type:    "thinking",
			Content: strField(data, "content"),
		}, false

	case "tool_call":
		id := strField(data, "id")
		name := strField(data, "name")
		args := strField(data, "raw_args")

		content := truncate(name+"("+args+")", 200)

		return protocol.LogEntry{
			Type:      "tool_call",
			Content:   content,
			ToolUseID: id,
		}, false

	case "usage":
		inputTokens := int64Field(data, "prompt_tokens")
		outputTokens := int64Field(data, "completion_tokens")

		return protocol.LogEntry{
			Type:  "usage",
			Model: strField(data, "model"),
			Usage: &protocol.LogTokenUsage{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}, false

	case "state_change":
		if strField(data, "state") == "awaiting_human" {
			// Normal idle between chat turns — not a transcript entry.
			return protocol.LogEntry{}, true
		}

		return protocol.LogEntry{
			Type:    "system",
			Content: summarizeData(data),
		}, false

	case "context_limit":
		return protocol.LogEntry{
			Type:    "system",
			Content: summarizeData(data),
		}, false

	case "error":
		return protocol.LogEntry{
			Type:    "stderr",
			Content: strField(data, "error"),
		}, false

	// Transcript-only kinds — not bridged.
	case "model_request", "tool_result", "tool_repair", "user_input", "verification":
		return protocol.LogEntry{}, true

	default:
		// Unknown future kinds: skip silently.
		return protocol.LogEntry{}, true
	}
}

// strField extracts a string value from data, returning "" if absent or
// not a string.
func strField(data map[string]any, key string) string {
	if data == nil {
		return ""
	}

	v, ok := data[key]
	if !ok {
		return ""
	}

	s, _ := v.(string)

	return s
}

// int64Field extracts a numeric value (JSON numbers unmarshal as float64)
// from data, returning 0 if absent or not numeric.
func int64Field(data map[string]any, key string) int64 {
	if data == nil {
		return 0
	}

	v, ok := data[key]
	if !ok {
		return 0
	}

	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}

	return 0
}

// summarizeData produces a brief human-readable string from event data for
// system-type frames where no dedicated field carries the message.
func summarizeData(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}

	b, err := json.Marshal(data)
	if err != nil {
		return ""
	}

	return truncate(string(b), 200)
}

// truncate cuts s to at most limit bytes without splitting a multi-byte rune:
// the cut point backs off past any continuation bytes so the result is
// always valid UTF-8 (assuming s is).
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}
