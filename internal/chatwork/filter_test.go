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

			w := newBoardFilterWriter(&buf, boardTools)

			n, err := w.Write([]byte(tt.input))
			require.NoError(t, err)
			assert.Equal(t, len(tt.input), n, "Write must return len(p) even when lines are filtered")
			assert.Equal(t, tt.wantOut, buf.String())
		})
	}
}
