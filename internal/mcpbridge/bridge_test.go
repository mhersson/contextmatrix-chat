package mcpbridge_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-chat/internal/mcpbridge"
)

// roundTripperFunc adapts a function to http.RoundTripper so a test can inject a
// base transport and observe that Connect routes MCP requests through it.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
	b, err := mcpbridge.Connect(ctx, hs.URL, "", nil)
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
	b, err := mcpbridge.Connect(ctx, hs.URL, "", nil)
	require.NoError(t, err)

	defer b.Close()

	assert.Contains(t, b.BoardToolNames(), toolName)
}

func TestBridgeExecute(t *testing.T) {
	hs := newTestServer(t)
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "", nil)
	require.NoError(t, err)

	defer b.Close()

	ts := b.Tools()
	require.Len(t, ts, 1)

	got, err := ts[0].Execute(ctx, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, toolResult, got.Text)
	assert.Empty(t, got.Images) // text-only board tool → no images
}

func TestBridgeSchema(t *testing.T) {
	hs := newTestServer(t)
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "", nil)
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
	b, err := mcpbridge.Connect(ctx, hs.URL, apiKey, nil)
	require.NoError(t, err)

	defer b.Close()

	mu.Lock()
	auth := gotAuth
	mu.Unlock()
	assert.Equal(t, "Bearer "+apiKey, auth)
}

func TestBridgeUsesBaseTransport(t *testing.T) {
	t.Run("base transport carries requests without an api key", func(t *testing.T) {
		hs := newTestServer(t)
		defer hs.Close()

		var count atomic.Int32

		base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			count.Add(1)

			return http.DefaultTransport.RoundTrip(req)
		})

		ctx := context.Background()
		b, err := mcpbridge.Connect(ctx, hs.URL, "", base)
		require.NoError(t, err)

		defer b.Close()

		assert.Positive(t, count.Load(), "the injected base transport must carry MCP requests")
	})

	t.Run("base transport composes under a bearer key", func(t *testing.T) {
		// The base transport (stand-in for the CA transport) must still carry the
		// request AND the bearer header must be applied on top — proving
		// bearerTransport wraps the injected base rather than replacing it.
		server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
		server.AddTool(&mcp.Tool{
			Name:        toolName,
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})

		mcpHandler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, nil)

		var (
			mu      sync.Mutex
			gotAuth string
		)

		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gotAuth = r.Header.Get("Authorization")
			mu.Unlock()
			mcpHandler.ServeHTTP(w, r)
		}))
		defer hs.Close()

		var count atomic.Int32

		base := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			count.Add(1)

			return http.DefaultTransport.RoundTrip(req)
		})

		ctx := context.Background()
		b, err := mcpbridge.Connect(ctx, hs.URL, "super-secret", base)
		require.NoError(t, err)

		defer b.Close()

		assert.Positive(t, count.Load(), "the base transport must carry requests even when a bearer key is set")

		mu.Lock()
		defer mu.Unlock()

		assert.Equal(t, "Bearer super-secret", gotAuth, "the bearer header must be applied on top of the base transport")
	})
}

func TestBridgeExecuteErrorNoImages(t *testing.T) {
	pngData := []byte{0x89, 0x50, 0x4e, 0x47}

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "error_tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{Text: "boom"},
				&mcp.ImageContent{Data: pngData, MIMEType: "image/png"},
			},
		}, nil
	})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, nil))
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "", nil)
	require.NoError(t, err)

	defer b.Close()

	ts := b.Tools()
	require.Len(t, ts, 1)

	got, err := ts[0].Execute(ctx, map[string]any{})
	require.Error(t, err)
	assert.Equal(t, "boom", got.Text)
	assert.Empty(t, got.Images) // error path must never leak images
}

func TestBridgeExecuteSurfacesImages(t *testing.T) {
	pngData := []byte{0x89, 0x50, 0x4e, 0x47} // opaque bytes; the bridge does not decode

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "get_card",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "card body"},
			&mcp.ImageContent{Data: pngData, MIMEType: "image/png"},
		}}, nil
	})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, nil))
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "", nil)
	require.NoError(t, err)

	defer b.Close()

	ts := b.Tools()
	require.Len(t, ts, 1)

	got, err := ts[0].Execute(ctx, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "card body", got.Text)

	require.Len(t, got.Images, 1)

	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData)
	assert.Equal(t, want, got.Images[0].URL)
}

func TestBridgeExecuteReconnectsAfterSessionPoisoned(t *testing.T) {
	hs := newTestServer(t)
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "", nil)
	require.NoError(t, err)

	defer b.Close()

	ts := b.Tools()
	require.Len(t, ts, 1)

	// First call succeeds on the initial session.
	got, err := ts[0].Execute(ctx, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, toolResult, got.Text)

	// Poison the session the way a proxy idle-close or a CM redeploy would: the
	// underlying session is closed, so the next call fails with an error
	// wrapping ErrConnectionClosed unless the bridge re-dials.
	b.CloseSessionForTest()

	// Without reconnect this returns "client is closing"; with it, the adapter
	// re-dials the same still-running server through the Bridge and succeeds.
	got, err = ts[0].Execute(ctx, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, toolResult, got.Text)
}

func TestBridgeExecuteDoesNotReconnectOnToolError(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "error_tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "boom"}},
		}, nil
	})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, nil))
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "", nil)
	require.NoError(t, err)

	defer b.Close()

	ts := b.Tools()
	require.Len(t, ts, 1)

	before := b.SessionForTest()

	_, err = ts[0].Execute(ctx, map[string]any{})
	require.Error(t, err) // a tool-level IsError result surfaces as a Go error

	assert.Same(t, before, b.SessionForTest(), "tool-level IsError must not trigger a re-dial")
}

func TestBridgeExecuteCoercesStringScalars(t *testing.T) {
	var (
		mu      sync.Mutex
		gotFlag any
		gotLim  any
		gotName any
	)

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "toggle",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"flag":{"type":["null","boolean"]},"limit":{"type":"integer"},"name":{"type":"string"}}}`),
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var m map[string]any

		_ = json.Unmarshal(req.Params.Arguments, &m)

		mu.Lock()
		gotFlag, gotLim, gotName = m["flag"], m["limit"], m["name"]
		mu.Unlock()

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, nil))
	defer hs.Close()

	ctx := context.Background()
	b, err := mcpbridge.Connect(ctx, hs.URL, "", nil)
	require.NoError(t, err)

	defer b.Close()

	ts := b.Tools()
	require.Len(t, ts, 1)

	// The model serialized every scalar as a string (the weak-model quirk).
	got, err := ts[0].Execute(ctx, map[string]any{"flag": "true", "limit": "5", "name": "keep"})
	require.NoError(t, err)
	assert.Equal(t, "ok", got.Text)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, true, gotFlag)                // "true" coerced to a JSON boolean
	assert.InEpsilon(t, float64(5), gotLim, 1e-9) // "5" coerced to a JSON number
	assert.Equal(t, "keep", gotName)              // string-typed arg left untouched
}
