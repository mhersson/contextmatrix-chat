package mcpbridge

import (
	"encoding/json"
	"slices"
	"strconv"
)

// propTypes maps a top-level property name to the JSON-Schema types it allows
// (e.g. "include_images" -> {"null","boolean"}). Empty when the schema is
// absent or unparseable, in which case coerceArgs is a no-op.
type propTypes map[string][]string

// schemaPropTypes extracts each top-level property's allowed JSON-Schema types
// from a tool InputSchema. The schema arrives as `any` (a map over the wire),
// so it is marshaled and re-read into a minimal shape — robust whether it is a
// map, a typed schema, or raw JSON. Returns nil on any failure or when the
// schema declares no properties.
func schemaPropTypes(inputSchema any) propTypes {
	if inputSchema == nil {
		return nil
	}

	raw, err := json.Marshal(inputSchema)
	if err != nil {
		return nil
	}

	var s struct {
		Properties map[string]struct {
			Type json.RawMessage `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil || len(s.Properties) == 0 {
		return nil
	}

	out := make(propTypes, len(s.Properties))
	for name, p := range s.Properties {
		if ts := decodeTypeField(p.Type); len(ts) > 0 {
			out[name] = ts
		}
	}

	return out
}

// decodeTypeField reads a JSON-Schema "type" value, which is either a single
// string ("boolean") or an array of strings (["null","boolean"]).
func decodeTypeField(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}

	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}

	return nil
}

// coerceArgs returns a copy of args with string values rewritten to the scalar
// type the schema declares, when unambiguous. Weak models often serialize
// scalars as strings (e.g. include_images "true"); this repairs that before the
// args reach the MCP server's schema validation. It never errors and never
// drops a key: anything it cannot confidently coerce passes through unchanged.
func coerceArgs(args map[string]any, types propTypes) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v

		s, ok := v.(string)
		if !ok {
			continue // only string values are coercion candidates
		}

		allowed := types[k]
		if len(allowed) == 0 || contains(allowed, "string") {
			continue // unknown property, or genuinely string-typed -> leave it
		}

		switch {
		case contains(allowed, "boolean") && (s == "true" || s == "false"):
			out[k] = s == "true"
		case contains(allowed, "integer"):
			if n, err := strconv.Atoi(s); err == nil {
				out[k] = n
			}
		case contains(allowed, "number"):
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				out[k] = f
			}
		}
	}

	return out
}

func contains(ss []string, target string) bool {
	return slices.Contains(ss, target)
}
