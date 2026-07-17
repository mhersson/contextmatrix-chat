// Package mcpbridge connects to ContextMatrix's /mcp endpoint and exposes
// each board tool as a harness tools.Tool, so the chat work loop can offer
// the model CM's full board toolset.
package mcpbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Bridge holds a live MCP client session and the tools listed at connect time.
// Chat keeps one long-lived session per worker, so callTool guards the session
// behind mu and re-dials once on a poisoned session (see callTool).
type Bridge struct {
	mu      sync.Mutex
	session *mcp.ClientSession
	listed  []*mcp.Tool
	dial    func(ctx context.Context) (*mcp.ClientSession, error)
}

// bearerTransport injects Authorization: Bearer on every outbound request.
type bearerTransport struct {
	apiKey string
	base   http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.apiKey)

	return t.base.RoundTrip(r)
}

// Connect dials mcpURL, authenticates with apiKey when non-empty, lists tools,
// and returns a ready Bridge. base is the outbound RoundTripper carrying every
// MCP request; a nil base uses http.DefaultTransport. A non-nil base lets the
// caller inject a CA-augmented transport (see the worker's ca_cert_file
// support); when apiKey is set it is wrapped by bearerTransport so the CA trust
// and the bearer header compose on the same connection. An error from Connect or
// ListTools is fatal.
func Connect(ctx context.Context, mcpURL, apiKey string, base http.RoundTripper) (*Bridge, error) {
	if base == nil {
		base = http.DefaultTransport
	}

	rt := base
	if apiKey != "" {
		rt = &bearerTransport{apiKey: apiKey, base: base}
	}

	httpClient := &http.Client{Transport: rt}

	// dial builds a fresh session with the same auth/CA posture. DisableStandaloneSSE:
	// chat registers no server->client handlers (NewClient gets nil options), so the
	// standalone GET stream carries nothing - while its SDK-side retry counter only
	// resets on event-ID progress, meaning any handful of idle closes over the
	// session lifetime (proxy idle timeouts, CM redeploys, blips) would otherwise
	// deterministically poison the whole long-lived session.
	dial := func(ctx context.Context) (*mcp.ClientSession, error) {
		transport := &mcp.StreamableClientTransport{
			Endpoint:             mcpURL,
			HTTPClient:           httpClient,
			DisableStandaloneSSE: true,
		}

		client := mcp.NewClient(&mcp.Implementation{Name: "contextmatrix-chat", Version: "0.1.0"}, nil)

		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			return nil, fmt.Errorf("connect to mcp endpoint: %w", err)
		}

		return session, nil
	}

	session, err := dial(ctx)
	if err != nil {
		return nil, err
	}

	// List tools once on the initial session; the cached schemas are reused across
	// reconnects (dial does not re-list).
	result, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		_ = session.Close()

		return nil, fmt.Errorf("list tools: %w", err)
	}

	return &Bridge{session: session, listed: result.Tools, dial: dial}, nil
}

// Tools returns one harness Tool adapter per listed MCP tool. Each adapter reads
// the current session through the Bridge (via callTool), so a reconnect that
// swaps b.session is visible to every already-handed-out adapter.
func (b *Bridge) Tools() []tools.Tool {
	out := make([]tools.Tool, 0, len(b.listed))
	for _, t := range b.listed {
		out = append(out, &toolAdapter{bridge: b, tool: t, types: schemaPropTypes(t.InputSchema)})
	}

	return out
}

// callTool is the single session chokepoint: on a poisoned session
// (ErrConnectionClosed from the SDK's client/server-closing states, or
// ErrSessionMissing when CM restarted and forgot the session) it re-dials a
// fresh session ONCE and retries the call once. Tool-level IsError results
// (which come back as err==nil here) and context cancellation never trigger a
// re-dial. Single-flight: concurrent callers piggyback on the first re-dial via
// the b.session == sess guard.
func (b *Bridge) callTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	b.mu.Lock()
	sess := b.session
	b.mu.Unlock()

	result, err := sess.CallTool(ctx, params)
	if err == nil || (!errors.Is(err, mcp.ErrConnectionClosed) && !errors.Is(err, mcp.ErrSessionMissing)) {
		return result, err
	}

	b.mu.Lock()
	if b.session == sess {
		fresh, derr := b.dial(ctx)
		if derr != nil {
			b.mu.Unlock()

			return nil, fmt.Errorf("reconnect after poisoned session: %w", errors.Join(err, derr))
		}

		_ = sess.Close()
		b.session = fresh

		slog.Warn("mcpbridge: mcp session poisoned; reconnected with a fresh session", "tool", params.Name)
	}

	sess = b.session
	b.mu.Unlock()

	return sess.CallTool(ctx, params)
}

// BoardToolNames returns the name of every listed MCP tool.
func (b *Bridge) BoardToolNames() []string {
	names := make([]string, 0, len(b.listed))
	for _, t := range b.listed {
		names = append(names, t.Name)
	}

	return names
}

// Close ends the MCP session. The mutex serialises Close against a concurrent
// reconnect swapping b.session.
func (b *Bridge) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.session.Close(); err != nil {
		return fmt.Errorf("close mcp session: %w", err)
	}

	return nil
}

// toolAdapter wraps one MCP tool as a harness tools.Tool. It holds the Bridge,
// not a captured session, so every call routes through the current session.
type toolAdapter struct {
	bridge *Bridge
	tool   *mcp.Tool
	types  propTypes // per-property schema types, for coercing weak-model string args
}

func (a *toolAdapter) Name() string { return a.tool.Name }

func (a *toolAdapter) Schema() llm.Tool {
	params := json.RawMessage("{}")

	if a.tool.InputSchema != nil {
		if raw, err := json.Marshal(a.tool.InputSchema); err == nil {
			params = raw
		}
	}

	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        a.tool.Name,
			Description: a.tool.Description,
			Parameters:  params,
		},
	}
}

func (a *toolAdapter) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	result, err := a.bridge.callTool(ctx, &mcp.CallToolParams{
		Name:      a.tool.Name,
		Arguments: coerceArgs(args, a.types),
	})
	if err != nil {
		return tools.Result{}, fmt.Errorf("call %s: %w", a.tool.Name, err)
	}

	text := allText(result)
	if result.IsError {
		return tools.Result{Text: text}, fmt.Errorf("%s", text) //nolint:err113
	}

	return tools.Result{Text: text, Images: imageURLs(result)}, nil
}

// allText concatenates the Text of every TextContent in the result.
func allText(result *mcp.CallToolResult) string {
	var sb strings.Builder

	for _, content := range result.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}

	return sb.String()
}

// imageURLs extracts inline ImageContent blocks from an MCP tool result as
// OpenAI image_url data URLs, best-effort: blobs with empty data or no MIME
// type are skipped. Generic across every board tool - no get_card special-casing.
func imageURLs(result *mcp.CallToolResult) []llm.ImageURL {
	var out []llm.ImageURL

	for _, content := range result.Content {
		ic, ok := content.(*mcp.ImageContent)
		if !ok || len(ic.Data) == 0 || ic.MIMEType == "" {
			continue
		}

		enc := base64.StdEncoding.EncodeToString(ic.Data)
		out = append(out, llm.ImageURL{URL: "data:" + ic.MIMEType + ";base64," + enc})
	}

	return out
}
