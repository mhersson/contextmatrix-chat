package chatwork

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/mhersson/contextmatrix-harness/llm"
)

// roleOrderFallback is an llm.LLM that resends an assistant-first request
// user-first when the gateway rejects it with a 400: a resumed transcript opens
// with the model's welcome, and some gateways require a user turn first. It
// wraps the client rather than retrying harness.Run because an interactive run
// never returns a transport error; it emits it and waits for the next user message before resending.
type roleOrderFallback struct {
	next llm.LLM
	// engaged is set once a flipped resend succeeds; later assistant-first
	// requests are flipped up front because the harness keeps the transcript
	// order in its own history and would otherwise be rejected every turn.
	engaged atomic.Bool
}

func newRoleOrderFallback(next llm.LLM) *roleOrderFallback {
	return &roleOrderFallback{next: next}
}

func (f *roleOrderFallback) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	return f.send(req, func(r llm.Request) (llm.Response, error) {
		return f.next.Send(ctx, r)
	})
}

func (f *roleOrderFallback) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	return f.send(req, func(r llm.Request) (llm.Response, error) {
		return f.next.SendStream(ctx, r, onDelta)
	})
}

func (f *roleOrderFallback) send(req llm.Request, call func(llm.Request) (llm.Response, error)) (llm.Response, error) {
	i := leadingAssistant(req.Messages)
	if i < 0 {
		return call(req)
	}

	flipped := req
	flipped.Messages = slices.Clone(req.Messages)
	flipped.Messages[i].Role = "user"

	if f.engaged.Load() {
		return call(flipped)
	}

	resp, err := call(req)
	if err == nil || !strings.Contains(err.Error(), "llm endpoint status 400") {
		return resp, err
	}

	resp, err = call(flipped)
	if err != nil {
		return resp, err
	}

	if f.engaged.CompareAndSwap(false, true) {
		slog.Warn("llm gateway rejected an assistant-first request; sending the leading assistant turn as a user turn from now on")
	}

	return resp, nil
}

// leadingAssistant returns the index of the first non-system message when it
// is an assistant turn, or -1.
func leadingAssistant(msgs []llm.Message) int {
	for i, m := range msgs {
		if m.Role == "system" {
			continue
		}

		if m.Role == "assistant" {
			return i
		}

		return -1
	}

	return -1
}
