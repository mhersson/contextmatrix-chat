package chatwork

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errRoleOrder error = &llm.StatusError{StatusCode: 400, Body: "the first non-system message must be from the user"}

type gatewayCall struct {
	stream   bool
	messages []llm.Message
}

// scriptedGateway is an llm.LLM that records every request and answers it
// with reply. harness.Run calls it from the test goroutine only.
type scriptedGateway struct {
	calls []gatewayCall
	reply func(c gatewayCall) (llm.Response, error)
}

func (g *scriptedGateway) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return g.record(false, req)
}

func (g *scriptedGateway) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return g.record(true, req)
}

func (g *scriptedGateway) record(stream bool, req llm.Request) (llm.Response, error) {
	c := gatewayCall{stream: stream, messages: slices.Clone(req.Messages)}
	g.calls = append(g.calls, c)

	return g.reply(c)
}

func firstNonSystemRole(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role != "system" {
			return m.Role
		}
	}

	return ""
}

// strictReply answers like a gateway that hoists system messages and rejects
// a request whose first remaining message is not a user turn.
func strictReply(c gatewayCall) (llm.Response, error) {
	if firstNonSystemRole(c.messages) != "user" {
		return llm.Response{}, errRoleOrder
	}

	return llm.Response{Content: "ok"}, nil
}

// resumedHistory is SeedHistory's shape for a resumed chat: the transcript
// opens with the model's cold-open welcome.
func resumedHistory() []llm.Message {
	return []llm.Message{
		{Role: "assistant", Content: "welcome"},
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}
}

// runEpoch drives one interactive harness.Run epoch through the role-order
// fallback and returns its result, the emitted events, and the run error.
func runEpoch(t *testing.T, gw llm.LLM, in *chatInbox, cfg harness.Config) (harness.Result, []events.Event, error) {
	t.Helper()

	var transcript bytes.Buffer

	cfg.Interactive = true
	cfg.Inbox = in
	cfg.SystemPrompt = "system prompt"

	// An interactive run that hits an unexpected error blocks for input; the
	// deadline turns that into a canceled run instead of a hung test.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	res, err := harness.Run(ctx, newRoleOrderFallback(gw), tools.NewRegistry(), events.NewEmitter(io.Discard, &transcript), "primer", cfg)

	var evs []events.Event

	sc := bufio.NewScanner(&transcript)
	for sc.Scan() {
		var ev events.Event

		require.NoError(t, json.Unmarshal(sc.Bytes(), &ev))

		evs = append(evs, ev)
	}

	return res, evs, err
}

func eventKinds(evs []events.Event) []events.Kind {
	kinds := make([]events.Kind, 0, len(evs))
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}

	return kinds
}

func countUserContent(msgs []llm.Message, content string) int {
	n := 0

	for _, m := range msgs {
		if m.Role == "user" && m.Content == content {
			n++
		}
	}

	return n
}

func TestRoleOrderFallbackResumedEpochRecoversUserFirst(t *testing.T) {
	t.Parallel()

	in := newChatInbox()
	in.push(harness.UserMessage{MessageID: "m1", Content: "queued before the first call"})

	followUps := []string{"follow-up one", "follow-up two"}
	answered := 0

	gw := &scriptedGateway{}
	gw.reply = func(c gatewayCall) (llm.Response, error) {
		resp, err := strictReply(c)
		if err != nil {
			return resp, err
		}

		if answered < len(followUps) {
			in.push(harness.UserMessage{MessageID: followUps[answered], Content: followUps[answered]})
		} else {
			in.closeInbox()
		}

		answered++

		return resp, nil
	}

	res, evs, err := runEpoch(t, gw, in, harness.Config{History: resumedHistory()})
	require.NoError(t, err)

	assert.Equal(t, "done", res.Reason)
	assert.Equal(t, 3, res.Turns)
	assert.NotContains(t, eventKinds(evs), events.ErrorKind, "the rejected request must not surface as an error event")

	require.Len(t, gw.calls, 4, "one rejected request, its user-first resend, then two later turns sent user-first up front")
	assert.Equal(t, "assistant", firstNonSystemRole(gw.calls[0].messages))

	for i, c := range gw.calls[1:] {
		assert.Equal(t, "user", firstNonSystemRole(c.messages), "call %d", i+2)
	}

	want := slices.Clone(gw.calls[0].messages)
	want[1].Role = "user"
	assert.Equal(t, want, gw.calls[1].messages, "the resend changes only the leading assistant turn's role")

	last := gw.calls[len(gw.calls)-1].messages
	for _, content := range append([]string{"queued before the first call"}, followUps...) {
		assert.Equal(t, 1, countUserContent(last, content), "user message %q must be sent exactly once", content)
	}

	require.Greater(t, len(res.Messages), 1)
	assert.Equal(t, "assistant", res.Messages[1].Role, "the harness's own conversation keeps the transcript order")
}

func TestRoleOrderFallbackPassesThrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		history []llm.Message
		err     error
	}{
		{
			name:    "user-first request rejected with 400",
			history: []llm.Message{{Role: "user", Content: "earlier question"}, {Role: "assistant", Content: "earlier answer"}},
			err:     errRoleOrder,
		},
		{
			name: "fresh epoch without history rejected with 400",
			err:  errRoleOrder,
		},
		{
			name:    "assistant-first request failing with a non-400 error",
			history: resumedHistory(),
			err:     &llm.StatusError{StatusCode: 500, Body: "upstream unavailable"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := newChatInbox()
			in.closeInbox()

			gw := &scriptedGateway{reply: func(gatewayCall) (llm.Response, error) { return llm.Response{}, tt.err }}

			_, evs, err := runEpoch(t, gw, in, harness.Config{History: tt.history})
			require.NoError(t, err)

			assert.Len(t, gw.calls, 1, "no resend")
			assert.Contains(t, eventKinds(evs), events.ErrorKind, "the error reaches the harness unchanged")
		})
	}
}

func TestRoleOrderFallbackFailedResendIsNotSticky(t *testing.T) {
	t.Parallel()

	in := newChatInbox()

	gw := &scriptedGateway{}
	gw.reply = func(gatewayCall) (llm.Response, error) {
		switch len(gw.calls) {
		case 2:
			in.push(harness.UserMessage{MessageID: "m1", Content: "are you there?"})
		case 4:
			in.closeInbox()
		}

		return llm.Response{}, &llm.StatusError{StatusCode: 400, Body: "unknown model"}
	}

	_, evs, err := runEpoch(t, gw, in, harness.Config{History: resumedHistory()})
	require.NoError(t, err)

	require.Len(t, gw.calls, 4, "each rejected request is resent exactly once")
	assert.Equal(t, "user", firstNonSystemRole(gw.calls[1].messages))
	assert.Equal(t, "assistant", firstNonSystemRole(gw.calls[2].messages), "a failed resend must not flip later requests up front")
	assert.Equal(t, "user", firstNonSystemRole(gw.calls[3].messages))

	errorEvents := 0

	for _, ev := range evs {
		if ev.Kind == events.ErrorKind {
			errorEvents++
		}
	}

	assert.Equal(t, 2, errorEvents, "each failed resend's error reaches the harness")
}

func TestRoleOrderFallbackCoversCompaction(t *testing.T) {
	t.Parallel()

	in := newChatInbox()
	in.closeInbox()

	gw := &scriptedGateway{}
	gw.reply = func(c gatewayCall) (llm.Response, error) {
		resp, err := strictReply(c)
		if err != nil {
			return resp, err
		}

		// The first answered turn reports a prompt over the compaction threshold.
		if len(gw.calls) == 2 {
			resp.Usage.PromptTokens = 900
		}

		return resp, nil
	}

	_, evs, err := runEpoch(t, gw, in, harness.Config{
		History:       resumedHistory(),
		ContextWindow: 1000,
		Compaction:    &harness.Compaction{Threshold: 0.5, KeepRecentTurns: 1},
	})
	require.NoError(t, err)

	require.Len(t, gw.calls, 3, "rejected turn, resend, compaction summary; the triggering reply is kept, not re-asked")
	assert.False(t, gw.calls[2].stream, "the compaction summary goes through Send")
	assert.Equal(t, "user", firstNonSystemRole(gw.calls[2].messages))
	assert.NotContains(t, eventKinds(evs), events.ErrorKind)

	compacted := false

	for _, ev := range evs {
		if ev.Kind == events.StateChange && ev.Data["event"] == "compaction" {
			compacted = true
		}
	}

	assert.True(t, compacted, "compaction must succeed")
}

func TestRoleOrderFallbackSendResendsUserFirst(t *testing.T) {
	t.Parallel()

	gw := &scriptedGateway{reply: strictReply}
	msgs := []llm.Message{
		{Role: "system", Content: "summarize"},
		{Role: "assistant", Content: "welcome"},
		{Role: "user", Content: "earlier question"},
	}

	resp, err := newRoleOrderFallback(gw).Send(context.Background(), llm.Request{Messages: msgs})
	require.NoError(t, err)

	assert.Equal(t, "ok", resp.Content)
	require.Len(t, gw.calls, 2)
	assert.False(t, gw.calls[1].stream)
	assert.Equal(t, "user", firstNonSystemRole(gw.calls[1].messages))
	assert.Equal(t, "welcome", gw.calls[1].messages[1].Content)
	assert.Equal(t, "assistant", msgs[1].Role, "the caller's messages are never mutated")
}

func TestRoleOrderFallbackLeavesAReasoningRefusalToTheHarness(t *testing.T) {
	t.Parallel()

	detailed := []llm.Message{
		{Role: "assistant", Content: "welcome"},
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer", ReasoningDetails: json.RawMessage(replayDetails)},
	}
	without := slices.Clone(detailed)
	without[2].ReasoningDetails = nil

	methods := map[string]func(f *roleOrderFallback, msgs []llm.Message) (llm.Response, error){
		"Send": func(f *roleOrderFallback, msgs []llm.Message) (llm.Response, error) {
			return f.Send(context.Background(), llm.Request{Messages: msgs})
		},
		"SendStream": func(f *roleOrderFallback, msgs []llm.Message) (llm.Response, error) {
			return f.SendStream(context.Background(), llm.Request{Messages: msgs}, func(llm.Delta) {})
		},
	}

	for name, send := range methods {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gw := &scriptedGateway{reply: func(gatewayCall) (llm.Response, error) {
				return llm.Response{}, errRoleOrder
			}}
			f := newRoleOrderFallback(gw)

			_, err := send(f, detailed)
			require.Equal(t, errRoleOrder, err, "the refusal is returned unchanged")
			require.Len(t, gw.calls, 1, "no flipped retry for a request carrying reasoning details")
			assert.Equal(t, "assistant", firstNonSystemRole(gw.calls[0].messages))

			_, err = send(f, without)
			require.ErrorIs(t, err, errRoleOrder)
			require.Len(t, gw.calls, 3, "without details the flipped retry still happens")
			assert.Equal(t, "assistant", firstNonSystemRole(gw.calls[1].messages))
			assert.Equal(t, "user", firstNonSystemRole(gw.calls[2].messages))
		})
	}
}
