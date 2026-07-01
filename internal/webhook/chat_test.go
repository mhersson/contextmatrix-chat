package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-chat/internal/executor"
	"github.com/mhersson/contextmatrix-chat/internal/frames"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- test doubles -----------------------------------------------------------

// fakeSkillsResolver is a test double for SkillsResolver. It returns a fixed
// host dir (or an error) so chat/start tests exercise the bind/env wiring
// without a real CM endpoint or git clone.
type fakeSkillsResolver struct {
	dir string
	err error
}

func (f fakeSkillsResolver) Resolve(context.Context) (string, error) {
	return f.dir, f.err
}

// stdinCapture is a thread-safe in-memory WriteCloser that records what was
// written and whether Close was called. Tests inject it into Run.Stdin to
// inspect frame writes without needing a real Docker attachment.
type stdinCapture struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
}

func (c *stdinCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return 0, io.ErrClosedPipe
	}

	return c.buf.Write(p)
}

func (c *stdinCapture) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true

	return nil
}

func (c *stdinCapture) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	b := make([]byte, c.buf.Len())
	copy(b, c.buf.Bytes())

	return b
}

func (c *stdinCapture) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closed
}

// fakeExecutor records Launch calls and injects a Run (with a stdinCapture)
// into the shared tracker so subsequent handler calls can write to the same
// stdin. launchErr, when set, is returned by Launch instead.
type fakeExecutor struct {
	mu        sync.Mutex
	launched  []executor.LaunchSpec
	stopped   []string
	launchErr error
	tracker   *executor.Tracker
	lastStdin *stdinCapture
}

func (f *fakeExecutor) Launch(_ context.Context, spec executor.LaunchSpec) error {
	if f.launchErr != nil {
		return f.launchErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.launched = append(f.launched, spec)

	if f.tracker != nil {
		stdin := &stdinCapture{}
		f.lastStdin = stdin
		run := &executor.Run{
			ContainerID: "cid-" + spec.SessionID,
			SessionID:   spec.SessionID,
			StartedAt:   time.Now(),
			Stdin:       stdin,
		}
		f.tracker.AddIfUnderLimit(run)
	}

	return nil
}

// Stop records the call and mirrors real teardown: the live executor stops the
// container and waitAndCleanup then clears the tracker, so the fake drops the
// tracker entry directly to make end→reopen observable in tests.
func (f *fakeExecutor) Stop(_ context.Context, sessionID string) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, sessionID)
	f.mu.Unlock()

	if f.tracker != nil {
		f.tracker.Remove(sessionID)
	}

	return nil
}

func (f *fakeExecutor) Stopped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.stopped))
	copy(out, f.stopped)

	return out
}

func (f *fakeExecutor) Kill(_ context.Context, _ string) error             { return nil }
func (f *fakeExecutor) List(_ context.Context) ([]*executor.Run, error)    { return nil, nil }
func (f *fakeExecutor) StopAll(_ context.Context) ([]*executor.Run, error) { return nil, nil }

func (f *fakeExecutor) Launched() []executor.LaunchSpec {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]executor.LaunchSpec, len(f.launched))
	copy(out, f.launched)

	return out
}

// ---- helpers ----------------------------------------------------------------

const (
	testSession = "sess-abc123"
	testImage   = "ghcr.io/test/chat-worker:latest"
	testMCPURL  = "http://cm:8080/mcp"
)

// newChatServer builds a Server with a real Tracker and a fakeExecutor.
// chatRunDirBase is set to a temp directory so file-writing tests stay hermetic.
func newChatServer(t *testing.T) (*Server, *executor.Tracker, *fakeExecutor) {
	t.Helper()

	tracker := executor.NewTracker(10)
	fe := &fakeExecutor{tracker: tracker}

	srv := NewServer(Config{
		APIKey:         testAPIKey,
		Executor:       fe,
		Tracker:        tracker,
		SkillsResolver: fakeSkillsResolver{dir: "/host/skills"},
		Chat: ChatConfig{
			Image:          testImage,
			MCPURL:         testMCPURL,
			SecretsHostDir: "/host/secrets",
			ChatRunDirBase: t.TempDir(),
			MemoryBytes:    512 * 1024 * 1024,
			PidsLimit:      128,
			MaxConcurrent:  10,
		},
	})

	return srv, tracker, fe
}

// signedPostBody builds and signs a POST request with the supplied JSON body.
func signedPostBody(t *testing.T, url string, body []byte) *http.Request {
	t.Helper()

	return signedPostBodyAt(t, url, body, nowTS())
}

// signedPostBodyAt builds and signs a POST request with an explicit timestamp
// string. Use this when a test sends multiple requests to the same server and
// needs each signature to be unique (the replay cache rejects duplicate
// timestamp+signature pairs within the skew window).
func signedPostBodyAt(t *testing.T, url string, body []byte, ts string) *http.Request {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	signReq(t, r, testAPIKey, body, ts)

	return r
}

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	b, err := json.Marshal(v)
	require.NoError(t, err)

	return b
}

// ---- /chat/start ------------------------------------------------------------

func TestChatStart_InvalidSessionID(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
	}{
		{"empty", ""},
		{"dotdot", ".."},
		{"dotdot prefix", "../x"},
		{"slash path", "a/b"},
		{"absolute", "/etc/passwd"},
		{"backslash", `a\b`},
		{"dotdot embedded", "foo..bar"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv, _, fe := newChatServer(t)

			body := mustJSON(t, protocol.ChatStartPayload{SessionID: tc.sessionID, Primer: "x"})
			w := httptest.NewRecorder()
			srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/start", body))

			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			assert.Contains(t, w.Body.String(), protocol.CodeInvalidField)
			assert.Empty(t, fe.Launched(), "no container must be launched for invalid session id")
		})
	}
}

func TestChatStart_HMACRequired(t *testing.T) {
	srv, _, _ := newChatServer(t)

	body := mustJSON(t, protocol.ChatStartPayload{SessionID: testSession})
	r := httptest.NewRequest(http.MethodPost, "/chat/start", bytes.NewReader(body))
	// No signing.
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestChatStart_CapacityLimit(t *testing.T) {
	// Create a server with max=1 and fill the tracker.
	tracker := executor.NewTracker(1)
	fe := &fakeExecutor{tracker: tracker}
	srv := NewServer(Config{
		APIKey:   testAPIKey,
		Executor: fe,
		Tracker:  tracker,
		Chat: ChatConfig{
			ChatRunDirBase: t.TempDir(),
			MaxConcurrent:  1,
		},
	})

	// Fill the tracker to capacity.
	stdin := &stdinCapture{}
	tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "existing",
		SessionID:   "existing-session",
		Stdin:       stdin,
	})

	body := mustJSON(t, protocol.ChatStartPayload{SessionID: testSession})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/start", body))

	require.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), protocol.CodeLimitReached)
}

func TestChatStart_Conflict(t *testing.T) {
	srv, tracker, _ := newChatServer(t)

	// Pre-register the session so the conflict check fires.
	stdin := &stdinCapture{}
	tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "existing",
		SessionID:   testSession,
		Stdin:       stdin,
	})

	body := mustJSON(t, protocol.ChatStartPayload{SessionID: testSession})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/start", body))

	require.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), protocol.CodeConflict)
}

func TestChatStart_HappyPath(t *testing.T) {
	srv, _, fe := newChatServer(t)

	resumeTurns := []protocol.ChatResumeTurn{
		{Seq: 1, Role: "user", Content: "hello"},
		{Seq: 2, Role: "assistant", Content: "hi there"},
	}

	payload := protocol.ChatStartPayload{
		SessionID: testSession,
		Project:   "alpha",
		RepoURL:   "https://github.com/org/repo",
		MCPAPIKey: "key-xyz",
		Model:     "claude-sonnet-4-5",
		Resume: &protocol.ChatResumeContext{
			Turns: resumeTurns,
		},
		Primer: "You are a helpful assistant.",
	}

	body := mustJSON(t, payload)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/start", body))

	require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())

	// Verify response shape.
	var resp protocol.ChatStartResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "cid-"+testSession, resp.ContainerID)

	// Verify Launch was called once with the expected spec.
	launched := fe.Launched()
	require.Len(t, launched, 1)

	spec := launched[0]
	assert.Equal(t, testSession, spec.SessionID)
	assert.Equal(t, testImage, spec.Image)

	// Check required env vars.
	envMap := envToMap(spec.Env)
	assert.Equal(t, testSession, envMap["CM_CHAT_SESSION"])
	assert.Equal(t, "alpha", envMap["CM_CHAT_PROJECT"])
	assert.Equal(t, "https://github.com/org/repo", envMap["CM_CHAT_REPO_URL"])
	assert.Equal(t, testMCPURL, envMap["CM_MCP_URL"])
	assert.Equal(t, "key-xyz", envMap["CM_MCP_API_KEY"])
	assert.Equal(t, "claude-sonnet-4-5", envMap["CM_MODEL"])
	assert.Equal(t, "/run/cm-skills", envMap["CMX_TASK_SKILLS_DIR"])
	assert.Equal(t, "1", envMap["CM_CHAT_RESUME"])

	// Check binds contain expected mounts.
	assert.Contains(t, spec.Binds, "/host/secrets:/run/cm-secrets:ro")
	assert.Contains(t, spec.Binds, "/host/skills:/run/cm-skills:ro")

	// The run-dir bind contains the session sub-path.
	var runDirBind string

	for _, b := range spec.Binds {
		if strings.Contains(b, testSession) {
			runDirBind = b
		}
	}

	require.NotEmpty(t, runDirBind, "expected a session run-dir bind")
	assert.True(t, strings.HasSuffix(runDirBind, ":/run/cm-chat:ro"))

	// Extract the host run dir from the bind to verify files.
	hostRunDir := strings.SplitN(runDirBind, ":", 2)[0]

	// primer.txt must match the payload Primer field.
	primerBytes, err := os.ReadFile(filepath.Join(hostRunDir, "primer.txt"))
	require.NoError(t, err)
	assert.Equal(t, payload.Primer, string(primerBytes))

	// resume.jsonl must have one line per turn.
	resumeBytes, err := os.ReadFile(filepath.Join(hostRunDir, "resume.jsonl"))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(string(resumeBytes), "\n"), "\n")
	assert.Len(t, lines, len(resumeTurns), "resume.jsonl line count should match turn count")

	// Each line must be valid JSON.
	for i, line := range lines {
		var turn protocol.ChatResumeTurn
		require.NoError(t, json.Unmarshal([]byte(line), &turn), "line %d is not valid JSON", i)
	}
}

func TestChatStart_NoResumeNoResumeFile(t *testing.T) {
	srv, _, fe := newChatServer(t)

	payload := protocol.ChatStartPayload{
		SessionID: testSession,
		Primer:    "primer text",
	}

	body := mustJSON(t, payload)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/start", body))

	require.Equal(t, http.StatusAccepted, w.Code)

	launched := fe.Launched()
	require.Len(t, launched, 1)

	spec := launched[0]
	envMap := envToMap(spec.Env)

	// CM_CHAT_RESUME must not be set when Resume is nil.
	_, hasResume := envMap["CM_CHAT_RESUME"]
	assert.False(t, hasResume)

	// CM_CHAT_PROJECT and CM_CHAT_REPO_URL must be absent when empty.
	_, hasProject := envMap["CM_CHAT_PROJECT"]
	assert.False(t, hasProject)

	_, hasRepoURL := envMap["CM_CHAT_REPO_URL"]
	assert.False(t, hasRepoURL)

	// resume.jsonl must NOT exist.
	var runDirBind string

	for _, b := range spec.Binds {
		if strings.Contains(b, testSession) {
			runDirBind = b
		}
	}

	hostRunDir := strings.SplitN(runDirBind, ":", 2)[0]
	_, err := os.Stat(filepath.Join(hostRunDir, "resume.jsonl"))
	assert.True(t, os.IsNotExist(err), "resume.jsonl should not exist when Resume is nil")
}

func TestChatStart_ConfigEnvForwarded(t *testing.T) {
	tracker := executor.NewTracker(10)
	fe := &fakeExecutor{tracker: tracker}

	srv := NewServer(Config{
		APIKey:   testAPIKey,
		Executor: fe,
		Tracker:  tracker,
		Chat: ChatConfig{
			Image:                     testImage,
			MCPURL:                    testMCPURL,
			SecretsHostDir:            "/host/secrets",
			ChatRunDirBase:            t.TempDir(),
			MaxConcurrent:             10,
			ToolOutputMaxBytes:        65536,
			CompactionThreshold:       0.75,
			CompactionKeepRecentTurns: 4,
			BashTimeoutMaxSeconds:     300,
			WorkerExtraEnv:            map[string]string{"MY_KEY": "my-value"},
		},
	})

	body := mustJSON(t, protocol.ChatStartPayload{
		SessionID: testSession,
		Primer:    "hello",
	})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/start", body))
	require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())

	launched := fe.Launched()
	require.Len(t, launched, 1)

	envMap := envToMap(launched[0].Env)
	assert.Equal(t, "65536", envMap["CMX_TOOL_OUTPUT_MAX_BYTES"])
	assert.Equal(t, "0.75", envMap["CMX_COMPACTION_THRESHOLD"])
	assert.Equal(t, "4", envMap["CMX_COMPACTION_KEEP_RECENT_TURNS"])
	assert.Equal(t, "300", envMap["CMX_BASH_TIMEOUT_MAX_SECONDS"])
	assert.Equal(t, "my-value", envMap["MY_KEY"])
}

func TestChatStart_ReasoningEffortEnv(t *testing.T) {
	t.Run("set when reasoningEffort configured", func(t *testing.T) {
		t.Parallel()

		tracker := executor.NewTracker(10)
		fe := &fakeExecutor{tracker: tracker}

		srv := NewServer(Config{
			APIKey:   testAPIKey,
			Executor: fe,
			Tracker:  tracker,
			Chat: ChatConfig{
				Image:           testImage,
				MCPURL:          testMCPURL,
				SecretsHostDir:  "/host/secrets",
				ChatRunDirBase:  t.TempDir(),
				MaxConcurrent:   10,
				ReasoningEffort: "medium",
			},
		})

		body := mustJSON(t, protocol.ChatStartPayload{SessionID: testSession, Primer: "hi"})
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/start", body))
		require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())

		launched := fe.Launched()
		require.Len(t, launched, 1)

		envMap := envToMap(launched[0].Env)
		assert.Equal(t, "medium", envMap["CMX_REASONING_EFFORT"])
	})

	t.Run("absent when reasoningEffort empty", func(t *testing.T) {
		t.Parallel()

		tracker := executor.NewTracker(10)
		fe := &fakeExecutor{tracker: tracker}

		srv := NewServer(Config{
			APIKey:   testAPIKey,
			Executor: fe,
			Tracker:  tracker,
			Chat: ChatConfig{
				Image:          testImage,
				MCPURL:         testMCPURL,
				SecretsHostDir: "/host/secrets",
				ChatRunDirBase: t.TempDir(),
				MaxConcurrent:  10,
			},
		})

		body := mustJSON(t, protocol.ChatStartPayload{SessionID: testSession, Primer: "hi"})
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/start", body))
		require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())

		launched := fe.Launched()
		require.Len(t, launched, 1)

		_, has := envToMap(launched[0].Env)["CMX_REASONING_EFFORT"]
		assert.False(t, has, "CMX_REASONING_EFFORT must not be set when reasoningEffort is empty")
	})
}

func TestChatStart_NoSkillsWhenResolverEmpty(t *testing.T) {
	tracker := executor.NewTracker(10)
	fe := &fakeExecutor{tracker: tracker}

	// Resolver yields no skills (empty pointer or fetch failure): the worker
	// launches without the skills bind or CMX_TASK_SKILLS_DIR env.
	srv := NewServer(Config{
		APIKey:         testAPIKey,
		Executor:       fe,
		Tracker:        tracker,
		SkillsResolver: fakeSkillsResolver{dir: ""},
		Chat: ChatConfig{
			Image:          testImage,
			MCPURL:         testMCPURL,
			SecretsHostDir: "/host/secrets",
			ChatRunDirBase: t.TempDir(),
			MaxConcurrent:  10,
		},
	})

	body := mustJSON(t, protocol.ChatStartPayload{SessionID: testSession, Primer: "hi"})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/start", body))
	require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())

	launched := fe.Launched()
	require.Len(t, launched, 1)

	_, hasSkillsEnv := envToMap(launched[0].Env)["CMX_TASK_SKILLS_DIR"]
	assert.False(t, hasSkillsEnv, "no CMX_TASK_SKILLS_DIR when skills unavailable")

	for _, b := range launched[0].Binds {
		assert.NotContains(t, b, "/run/cm-skills", "no skills bind when skills unavailable")
	}
}

// ---- /chat/end --------------------------------------------------------------

func TestChatEnd_ClosesStdin(t *testing.T) {
	srv, tracker, _ := newChatServer(t)

	stdin := &stdinCapture{}
	tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "cid-1",
		SessionID:   testSession,
		Stdin:       stdin,
	})

	body := mustJSON(t, protocol.ChatEndPayload{SessionID: testSession})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/end", body))

	require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())
	assert.True(t, stdin.IsClosed(), "stdin should be closed after /chat/end")
}

// TestChatEnd_StopsContainerAndClearsTracker is the regression guard for the
// end→reopen 409: closing stdin alone never EOFs the worker (StdinOnce=false),
// so /chat/end must stop the container and clear the tracker — otherwise a later
// /chat/start for the same session returns 409 "session already active".
func TestChatEnd_StopsContainerAndClearsTracker(t *testing.T) {
	srv, tracker, fe := newChatServer(t)

	stdin := &stdinCapture{}
	tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "cid-1",
		SessionID:   testSession,
		Stdin:       stdin,
	})

	body := mustJSON(t, protocol.ChatEndPayload{SessionID: testSession})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/end", body))

	require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, []string{testSession}, fe.Stopped(),
		"/chat/end must stop the container, not just close stdin")

	_, ok := tracker.Get(testSession)
	assert.False(t, ok,
		"session must be cleared from the tracker so a later /chat/start does not 409")
}

func TestChatEnd_IdempotentWhenNotFound(t *testing.T) {
	srv, _, _ := newChatServer(t)

	// No session tracked — /chat/end should return 200 (idempotent).
	body := mustJSON(t, protocol.ChatEndPayload{SessionID: "unknown-session"})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/chat/end", body))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp protocol.SuccessResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
}

func TestChatEnd_HMACRequired(t *testing.T) {
	srv, _, _ := newChatServer(t)

	body := mustJSON(t, protocol.ChatEndPayload{SessionID: testSession})
	r := httptest.NewRequest(http.MethodPost, "/chat/end", bytes.NewReader(body))
	// No signing.
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ---- /message ---------------------------------------------------------------

func TestMessage_UserMessageFrameWritten(t *testing.T) {
	srv, tracker, _ := newChatServer(t)

	stdin := &stdinCapture{}
	tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "cid-1",
		SessionID:   testSession,
		Stdin:       stdin,
	})

	body := mustJSON(t, protocol.MessagePayload{
		SessionID: testSession,
		Content:   "hello world",
		MessageID: "msg-001",
	})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/message", body))

	require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())

	var resp protocol.SuccessResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "msg-001", resp.MessageID)

	// Inspect bytes written to stdin: should be a valid user_message frame.
	written := stdin.Bytes()
	require.NotEmpty(t, written)

	var f frames.Frame
	require.NoError(t, json.Unmarshal(bytes.TrimRight(written, "\n"), &f))
	assert.Equal(t, frames.TypeUserMessage, f.Type)
	assert.Equal(t, "hello world", f.Content)
	assert.Equal(t, "msg-001", f.MessageID)
}

func TestMessage_ClearFrameWritten(t *testing.T) {
	srv, tracker, _ := newChatServer(t)

	stdin := &stdinCapture{}
	tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "cid-1",
		SessionID:   testSession,
		Stdin:       stdin,
	})

	body := mustJSON(t, protocol.MessagePayload{
		SessionID: testSession,
		Content:   "/clear",
		MessageID: "msg-clear",
	})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/message", body))

	require.Equal(t, http.StatusAccepted, w.Code)

	var f frames.Frame

	written := stdin.Bytes()
	require.NoError(t, json.Unmarshal(bytes.TrimRight(written, "\n"), &f))
	assert.Equal(t, frames.TypeClear, f.Type)
	assert.Empty(t, f.Content, "clear frame must not carry content")
	assert.Empty(t, f.MessageID, "clear frame must not carry message_id")
}

func TestMessage_DedupCachedAck(t *testing.T) {
	srv, tracker, _ := newChatServer(t)

	stdin := &stdinCapture{}
	tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "cid-1",
		SessionID:   testSession,
		Stdin:       stdin,
	})

	payload := protocol.MessagePayload{
		SessionID: testSession,
		Content:   "first delivery",
		MessageID: "msg-dup",
	}

	ts1 := nowTS()

	// First delivery → 202.
	body := mustJSON(t, payload)
	w1 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w1, signedPostBodyAt(t, "/message", body, ts1))
	require.Equal(t, http.StatusAccepted, w1.Code)

	// Second request with same message_id but a distinct timestamp so the replay
	// cache does not reject it (replay key = timestamp+"."+signature). This is the
	// retry scenario — same payload, fresh signature.
	bytesAfterFirst := len(stdin.Bytes())

	ts2 := strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10)
	body2 := mustJSON(t, payload)
	w2 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w2, signedPostBodyAt(t, "/message", body2, ts2))

	require.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), "duplicate")
	assert.Len(t, stdin.Bytes(), bytesAfterFirst, "stdin should not receive a second write")
}

func TestMessage_EmptyMessageIDNeverDeduped(t *testing.T) {
	srv, tracker, _ := newChatServer(t)

	stdin := &stdinCapture{}
	tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "cid-1",
		SessionID:   testSession,
		Stdin:       stdin,
	})

	// Two requests with empty message_id: each must be delivered independently.
	// Use distinct timestamps so the replay cache sees them as separate requests.
	now := time.Now()
	for i := range 2 {
		ts := strconv.FormatInt(now.Add(time.Duration(i)*time.Second).Unix(), 10)
		body := mustJSON(t, protocol.MessagePayload{
			SessionID: testSession,
			Content:   "no dedup",
			MessageID: "", // empty — opt out of at-most-once
		})
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, signedPostBodyAt(t, "/message", body, ts))
		require.Equal(t, http.StatusAccepted, w.Code)
	}

	// Both deliveries should have reached stdin.
	assert.NotEmpty(t, stdin.Bytes())
}

func TestMessage_NotFound(t *testing.T) {
	srv, _, _ := newChatServer(t)

	body := mustJSON(t, protocol.MessagePayload{
		SessionID: "no-such-session",
		Content:   "hello",
		MessageID: "msg-nf",
	})
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, signedPostBody(t, "/message", body))

	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), protocol.CodeNotFound)
}

func TestMessage_HMACRequired(t *testing.T) {
	srv, _, _ := newChatServer(t)

	body := mustJSON(t, protocol.MessagePayload{SessionID: testSession, Content: "hi"})
	r := httptest.NewRequest(http.MethodPost, "/message", bytes.NewReader(body))
	// No signing.
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ---- /health with tracker ---------------------------------------------------

func TestHealth_ReturnsTrackerCount(t *testing.T) {
	srv, tracker, _ := newChatServer(t)

	stdin := &stdinCapture{}
	tracker.AddIfUnderLimit(&executor.Run{
		ContainerID: "cid-1",
		SessionID:   "s1",
		Stdin:       stdin,
	})

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	require.Equal(t, http.StatusOK, w.Code)

	var hr protocol.HealthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &hr))
	assert.True(t, hr.OK)
	assert.Equal(t, 1, hr.RunningContainers)
	assert.Equal(t, 10, hr.MaxConcurrent)
}

// ---- helpers ----------------------------------------------------------------

// envToMap converts KEY=VALUE env strings to a map for assertion convenience.
func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))

	for _, kv := range env {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			m[kv] = ""

			continue
		}

		m[kv[:idx]] = kv[idx+1:]
	}

	return m
}
