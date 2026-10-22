package chatwork

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replayDetails is one reasoning block as a provider returns it.
const replayDetails = `[{"type":"reasoning.text","text":"think","index":0}]`

// errRefusesDetails is how an endpoint that does not accept the field answers.
var errRefusesDetails = errors.New("llm endpoint status 400: unknown field reasoning_details")

func setReplay(t *testing.T, on bool) {
	t.Helper()

	v := ""
	if on {
		v = "true"
	}

	t.Setenv("CMX_REPLAY_REASONING", v)
}

// withDetails is resp carrying reasoning served by provider "Prov".
func withDetails(resp llm.Response) llm.Response {
	resp.ReasoningDetails = json.RawMessage(replayDetails)
	resp.Provider = "Prov"

	return resp
}

// carrying returns the messages that would send reasoning details.
func carrying(msgs []llm.Message) []llm.Message {
	var out []llm.Message

	for _, m := range msgs {
		if m.HasReasoningDetails() {
			out = append(out, m)
		}
	}

	return out
}

func countStateEvents(evs []events.Event, event string) int {
	n := 0

	for _, ev := range evs {
		if ev.Kind == events.StateChange && ev.Data["event"] == event {
			n++
		}
	}

	return n
}

// replayWireBodies runs one epoch against an endpoint of llmType that streams
// a reply with reasoning served by "Prov", queues a follow-up after the first
// request and closes the inbox after the second; it returns each request body.
func replayWireBodies(t *testing.T, llmType string) []map[string]json.RawMessage {
	t.Helper()

	in := newChatInbox()

	var (
		mu     sync.Mutex
		bodies []map[string]json.RawMessage
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		mu.Lock()

		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()

		if n == 1 {
			in.push(harness.UserMessage{MessageID: "m1", Content: "go on"})
		} else {
			in.closeInbox()
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"provider":"Prov","choices":[{"index":0,"delta":{"role":"assistant","content":"done","reasoning_details":`+
			replayDetails+`},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	client := llm.NewClient("k", llm.WithBaseURL(srv.URL+"/v1"), llm.WithDialect(dialectFromType(llmType)))

	_, _, err := runEpoch(t, client, in, harnessConfig("test/model", 0, nil, in, nil))
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	return slices.Clone(bodies)
}

func TestReplayOnTheWire(t *testing.T) {
	for _, llmType := range []string{"openrouter", "openai"} {
		for _, on := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/replay=%t", llmType, on), func(t *testing.T) {
				setReplay(t, on)

				bodies := replayWireBodies(t, llmType)
				require.Len(t, bodies, 2, "the follow-up resends the first reply")

				var msgs []map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(bodies[1]["messages"], &msgs))

				var asst map[string]json.RawMessage

				for _, m := range msgs {
					if string(m["role"]) == `"assistant"` {
						asst = m
					}
				}

				require.NotNil(t, asst)

				provider, pinned := bodies[1]["provider"]

				if !on {
					assert.NotContains(t, asst, "reasoning_details", "off sends none")
					assert.False(t, pinned)

					return
				}

				assert.JSONEq(t, replayDetails, string(asst["reasoning_details"]), "both legs get the field unchanged in value")

				if llmType == "openrouter" {
					require.True(t, pinned)
					assert.JSONEq(t, `{"order":["Prov"]}`, string(provider), "the pin routes on the OpenRouter leg")
				} else {
					assert.False(t, pinned, "the endpoint leg carries no routing object")
				}
			})
		}
	}
}

func TestReplayResumedEpochSendsOnlyLiveReasoning(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("replay=%t", on), func(t *testing.T) {
			setReplay(t, on)

			in := newChatInbox()
			answered := 0

			gw := &scriptedGateway{}
			gw.reply = func(c gatewayCall) (llm.Response, error) {
				resp, err := strictReply(c)
				if err != nil {
					return resp, err
				}

				answered++
				if answered == 1 {
					in.push(harness.UserMessage{MessageID: "m1", Content: "go on"})
				} else {
					in.closeInbox()
				}

				return withDetails(resp), nil
			}

			res, evs, err := runEpoch(t, gw, in, harnessConfig("test/model", 0, resumedHistory(), in, nil))
			require.NoError(t, err)
			assert.Equal(t, "done", res.Reason)

			require.Len(t, gw.calls, 3, "the rejected assistant-first request, its user-first resend, the follow-up sent user-first")

			last := gw.calls[2].messages
			assert.Equal(t, "user", firstNonSystemRole(last))
			assert.Equal(t, llm.Message{Role: "user", Content: "welcome"}, last[1], "the flip changes only the seeded welcome's role")

			sent := carrying(last)
			if on {
				require.Len(t, sent, 1, "only the live reply carries reasoning; seeded turns are text-only")
				assert.Equal(t, "ok", sent[0].Content)
				assert.JSONEq(t, replayDetails, string(sent[0].ReasoningDetails))
			} else {
				assert.Empty(t, sent)
			}

			assert.Zero(t, countStateEvents(evs, "replay_dropped"))
		})
	}
}

func TestReplayRefusedByTheEndpointCostsOneRejectedRequestPerTurn(t *testing.T) {
	tests := []struct {
		name        string
		strict      bool   // the gateway also rejects an assistant-first request
		lastLeading string // the role the final request opens with
		calls       int    // every request the gateway saw, rejected ones included, over three turns
	}{
		{"gateway takes assistant-first", false, "assistant", 5},
		{"gateway rejects assistant-first", true, "user", 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setReplay(t, true)

			in := newChatInbox()
			answered := 0

			gw := &scriptedGateway{}
			gw.reply = func(c gatewayCall) (llm.Response, error) {
				if len(carrying(c.messages)) > 0 {
					return llm.Response{}, errRefusesDetails
				}

				if tt.strict && firstNonSystemRole(c.messages) != "user" {
					return llm.Response{}, errRoleOrder
				}

				answered++
				if answered < 3 {
					in.push(harness.UserMessage{MessageID: fmt.Sprintf("m%d", answered), Content: "go on"})
				} else {
					in.closeInbox()
				}

				return withDetails(llm.Response{Content: "ok"}), nil
			}

			res, evs, err := runEpoch(t, gw, in, harnessConfig("test/model", 0, resumedHistory(), in, nil))
			require.NoError(t, err)
			assert.Equal(t, "done", res.Reason, "every turn completes")

			require.Len(t, gw.calls, tt.calls)

			var refused []gatewayCall

			for _, c := range gw.calls {
				if len(carrying(c.messages)) > 0 {
					refused = append(refused, c)
				}
			}

			require.Len(t, refused, 2, "one refused replay per replay turn")

			if !tt.strict {
				for _, c := range refused {
					assert.Equal(t, "assistant", firstNonSystemRole(c.messages), "no flipped retry follows the refusal")
				}
			}

			last := gw.calls[len(gw.calls)-1]
			assert.Empty(t, carrying(last.messages), "the last request is the resend without details")
			assert.Equal(t, tt.lastLeading, firstNonSystemRole(last.messages), "a refused replay never engages the role-order flip")
			assert.Equal(t, 2, countStateEvents(evs, "replay_dropped"))
			assert.NotContains(t, eventKinds(evs), events.ErrorKind)
		})
	}
}
