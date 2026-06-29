package mcpbridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSchemaPropTypes(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"include_images": map[string]any{"type": []any{"null", "boolean"}},
			"card_id":        map[string]any{"type": "string"},
			"limit":          map[string]any{"type": "integer"},
		},
	}

	pt := schemaPropTypes(schema)
	assert.ElementsMatch(t, []string{"null", "boolean"}, pt["include_images"])
	assert.Equal(t, []string{"string"}, pt["card_id"])
	assert.Equal(t, []string{"integer"}, pt["limit"])
}

func TestSchemaPropTypes_AbsentOrBad(t *testing.T) {
	assert.Empty(t, schemaPropTypes(nil))
	assert.Empty(t, schemaPropTypes(map[string]any{"type": "object"})) // no properties
}

func TestCoerceArgs(t *testing.T) {
	types := propTypes{
		"flag":  {"null", "boolean"},
		"limit": {"integer"},
		"ratio": {"number"},
		"name":  {"string"},
	}

	got := coerceArgs(map[string]any{
		"flag":  "true",
		"limit": "5",
		"ratio": "1.5",
		"name":  "keep-me",
		"extra": "untouched", // not in schema
	}, types)

	assert.Equal(t, true, got["flag"])
	assert.Equal(t, 5, got["limit"])
	assert.InEpsilon(t, 1.5, got["ratio"], 1e-9)
	assert.Equal(t, "keep-me", got["name"])    // schema says string -> unchanged
	assert.Equal(t, "untouched", got["extra"]) // unknown prop -> unchanged
}

func TestCoerceArgs_FalseUnparseableAndNonString(t *testing.T) {
	types := propTypes{
		"flag":  {"boolean"},
		"limit": {"integer"},
	}

	got := coerceArgs(map[string]any{
		"flag":  "false",
		"limit": "not-a-number", // unparseable -> leave as-is
		"keep":  true,           // already a bool, not a string -> untouched
	}, types)

	assert.Equal(t, false, got["flag"])
	assert.Equal(t, "not-a-number", got["limit"])
	assert.Equal(t, true, got["keep"])
}

func TestCoerceArgs_NilTypesNoOp(t *testing.T) {
	got := coerceArgs(map[string]any{"flag": "true"}, nil)
	assert.Equal(t, "true", got["flag"]) // no schema -> no coercion
}
