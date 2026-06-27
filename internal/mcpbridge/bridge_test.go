package mcpbridge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-chat/internal/mcpbridge"
)

const (
	toolName   = "list_projects"
	toolResult = "projects: alpha, beta"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        toolName,
		Description: "List all projects",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: toolResult}},
		}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return server
	}, nil)

	return httptest.NewServer(handler)
}

func TestBridgeConnect(t *testing.T) {
	hs := newTestServer(t)
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "")
	require.NoError(t, err)

	defer b.Close()

	ts := b.Tools()
	require.Len(t, ts, 1)
	assert.Equal(t, toolName, ts[0].Name())
}

func TestBridgeBoardToolNames(t *testing.T) {
	hs := newTestServer(t)
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "")
	require.NoError(t, err)

	defer b.Close()

	assert.Contains(t, b.BoardToolNames(), toolName)
}

func TestBridgeExecute(t *testing.T) {
	hs := newTestServer(t)
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "")
	require.NoError(t, err)

	defer b.Close()

	ts := b.Tools()
	require.Len(t, ts, 1)

	got, err := ts[0].Execute(ctx, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, toolResult, got)
}

func TestBridgeSchema(t *testing.T) {
	hs := newTestServer(t)
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "")
	require.NoError(t, err)

	defer b.Close()

	ts := b.Tools()
	require.Len(t, ts, 1)

	schema := ts[0].Schema()

	assert.Equal(t, "function", schema.Type)
	assert.Equal(t, toolName, schema.Function.Name)
	require.NotNil(t, schema.Function.Parameters)

	var m map[string]any
	require.NoError(t, json.Unmarshal(schema.Function.Parameters, &m))
	assert.Equal(t, "object", m["type"])
}

func TestBridgeBearerHeader(t *testing.T) {
	const apiKey = "super-secret"

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        toolName,
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
		}, nil
	})

	mcpHandler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return server
	}, nil)

	var (
		mu      sync.Mutex
		gotAuth string
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		mcpHandler.ServeHTTP(w, r)
	})

	hs := httptest.NewServer(handler)
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, apiKey)
	require.NoError(t, err)

	defer b.Close()

	mu.Lock()
	auth := gotAuth
	mu.Unlock()
	assert.Equal(t, "Bearer "+apiKey, auth)
}
