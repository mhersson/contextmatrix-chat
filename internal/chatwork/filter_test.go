package chatwork

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBoardFilterWriter(t *testing.T) {
	t.Parallel()

	boardTools := []string{"claim_card", "update_card", "complete_task"}

	tests := []struct {
		name    string
		input   string
		wantOut string
	}{
		{
			name:    "board tool_call is dropped",
			input:   `{"seq":1,"kind":"tool_call","data":{"name":"claim_card"}}` + "\n",
			wantOut: "",
		},
		{
			name:    "non-board tool_call passes through",
			input:   `{"seq":2,"kind":"tool_call","data":{"name":"read_file"}}` + "\n",
			wantOut: `{"seq":2,"kind":"tool_call","data":{"name":"read_file"}}` + "\n",
		},
		{
			name:    "model_response passes through",
			input:   `{"seq":3,"kind":"model_response","data":{"content":"hello"}}` + "\n",
			wantOut: `{"seq":3,"kind":"model_response","data":{"content":"hello"}}` + "\n",
		},
		{
			name: "multiple lines: board dropped, others kept",
			input: `{"seq":4,"kind":"tool_call","data":{"name":"update_card"}}` + "\n" +
				`{"seq":5,"kind":"tool_result","data":{"output_len":42}}` + "\n" +
				`{"seq":6,"kind":"tool_call","data":{"name":"claim_card"}}` + "\n" +
				`{"seq":7,"kind":"usage","data":{"cost_usd":0.001}}` + "\n",
			wantOut: `{"seq":5,"kind":"tool_result","data":{"output_len":42}}` + "\n" +
				`{"seq":7,"kind":"usage","data":{"cost_usd":0.001}}` + "\n",
		},
		{
			name:    "malformed JSON passes through unchanged",
			input:   "not json\n",
			wantOut: "not json\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			w := newBoardFilterWriter(&buf, boardToolSet(boardTools))

			n, err := w.Write([]byte(tt.input))
			require.NoError(t, err)
			assert.Equal(t, len(tt.input), n, "Write must return len(p) even when lines are filtered")
			assert.Equal(t, tt.wantOut, buf.String())
		})
	}
}

func TestBoardFilterWriterByMCPServer(t *testing.T) {
	t.Parallel()

	call := func(name string) string {
		return `{"seq":1,"kind":"tool_call","data":{"name":"` + name + `"}}` + "\n"
	}
	response := `{"seq":2,"kind":"model_response","data":{"content":"use mcp__contextmatrix__list_cards"}}` + "\n"

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"cm tool dropped", call("mcp__contextmatrix__list_cards"), ""},
		{"Bash passes", call("Bash"), call("Bash")},
		{"grep passes", call("grep"), call("grep")},
		{"bridge tool passes", call("mcp__cm__grep"), call("mcp__cm__grep")},
		{"model_response passes", response, response},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			w := newBoardFilterWriter(&buf, mcpServerTools("contextmatrix"))
			_, err := w.Write([]byte(tt.input))
			require.NoError(t, err)
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

func TestClaudeCodeFilterWriter(t *testing.T) {
	t.Parallel()

	stopDone := `{"seq":1,"kind":"state_change","data":{"stop":"done","turns":2}}` + "\n"
	stopError := `{"seq":2,"kind":"state_change","data":{"stop":"error","turns":1}}` + "\n"
	awaiting := `{"seq":3,"kind":"state_change","data":{"state":"awaiting_human"}}` + "\n"
	boardCall := `{"seq":4,"kind":"tool_call","data":{"name":"mcp__contextmatrix__list_cards"}}` + "\n"
	otherCall := `{"seq":5,"kind":"tool_call","data":{"name":"Bash"}}` + "\n"

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"stop done dropped", stopDone, ""},
		{"stop error dropped", stopError, ""},
		{"awaiting_human kept", awaiting, awaiting},
		{"board tool_call dropped", boardCall, ""},
		{"non-board tool_call kept", otherCall, otherCall},
		{"mixed lines", stopDone + awaiting + boardCall + otherCall, awaiting + otherCall},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			w := newClaudeCodeFilterWriter(&buf, mcpServerTools("contextmatrix"))
			n, err := w.Write([]byte(tt.input))
			require.NoError(t, err)
			assert.Equal(t, len(tt.input), n)
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

func TestBoardFilterWriterKeepsStopLine(t *testing.T) {
	t.Parallel()

	stop := `{"seq":1,"kind":"state_change","data":{"stop":"done","turns":2}}` + "\n"

	var buf bytes.Buffer

	w := newBoardFilterWriter(&buf, boardToolSet([]string{"claim_card"}))
	_, err := w.Write([]byte(stop))
	require.NoError(t, err)
	assert.Equal(t, stop, buf.String())
}
