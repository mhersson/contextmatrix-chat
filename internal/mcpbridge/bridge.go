// Package mcpbridge connects to ContextMatrix's /mcp endpoint and exposes
// each board tool as a harness tools.Tool, so the chat work loop can offer
// the model CM's full board toolset.
package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Bridge holds a live MCP client session and the tools listed at connect time.
type Bridge struct {
	session *mcp.ClientSession
	listed  []*mcp.Tool
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
// and returns a ready Bridge. An error from Connect or ListTools is fatal.
func Connect(ctx context.Context, mcpURL, apiKey string) (*Bridge, error) {
	var httpClient *http.Client
	if apiKey != "" {
		httpClient = &http.Client{
			Transport: &bearerTransport{apiKey: apiKey, base: http.DefaultTransport},
		}
	} else {
		httpClient = http.DefaultClient
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:   mcpURL,
		HTTPClient: httpClient,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "contextmatrix-chat", Version: "0.1.0"}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to mcp endpoint: %w", err)
	}

	result, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		_ = session.Close()

		return nil, fmt.Errorf("list tools: %w", err)
	}

	return &Bridge{session: session, listed: result.Tools}, nil
}

// Tools returns one harness Tool adapter per listed MCP tool.
func (b *Bridge) Tools() []tools.Tool {
	out := make([]tools.Tool, 0, len(b.listed))
	for _, t := range b.listed {
		out = append(out, &toolAdapter{session: b.session, tool: t})
	}

	return out
}

// BoardToolNames returns the name of every listed MCP tool.
func (b *Bridge) BoardToolNames() []string {
	names := make([]string, 0, len(b.listed))
	for _, t := range b.listed {
		names = append(names, t.Name)
	}

	return names
}

// Close ends the MCP session.
func (b *Bridge) Close() error {
	if err := b.session.Close(); err != nil {
		return fmt.Errorf("close mcp session: %w", err)
	}

	return nil
}

// toolAdapter wraps one MCP tool as a harness tools.Tool.
type toolAdapter struct {
	session *mcp.ClientSession
	tool    *mcp.Tool
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

func (a *toolAdapter) Execute(ctx context.Context, args map[string]any) (string, error) {
	result, err := a.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      a.tool.Name,
		Arguments: args,
	})
	if err != nil {
		return "", fmt.Errorf("call %s: %w", a.tool.Name, err)
	}

	text := allText(result)
	if result.IsError {
		return text, fmt.Errorf("%s", text) //nolint:err113
	}

	return text, nil
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
