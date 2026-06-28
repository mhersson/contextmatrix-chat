package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-chat/internal/executor"
	"github.com/mhersson/contextmatrix-chat/internal/frames"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// skillsMountPath is the fixed in-container mount point for task-skills. CM is
// the single source of truth and the chat service clones the pointer it serves;
// the path is not operator-configurable.
const skillsMountPath = "/run/cm-skills"

// handleChatStart starts a long-lived chat container for the given session. It
// creates the per-session run directory, writes resume.jsonl and primer.txt,
// builds the LaunchSpec, and delegates to the executor. The response body
// includes the container ID so CM can correlate sessions.
func (s *Server) handleChatStart(w http.ResponseWriter, r *http.Request) {
	var p protocol.ChatStartPayload
	if !s.decode(w, r, &p) {
		return
	}

	// Guard: a missing or path-unsafe session ID would let chatExit's
	// os.RemoveAll(filepath.Join(chatRunDirBase, sessionID)) delete the entire
	// run-dir base (empty ID) or escape the bind-mount (path separators / ..).
	if p.SessionID == "" ||
		p.SessionID != filepath.Base(p.SessionID) ||
		strings.ContainsAny(p.SessionID, `/\`) ||
		strings.Contains(p.SessionID, "..") {
		writeError(w, http.StatusBadRequest, protocol.CodeInvalidField, "invalid session_id")

		return
	}

	// Capacity pre-check: refuse before we hit Docker so the 429 is fast.
	if s.tracker.Count() >= s.maxConcurrent {
		s.logger.Warn("chat/start: capacity limit reached",
			"session_id", p.SessionID, "limit", s.maxConcurrent)
		writeError(w, http.StatusTooManyRequests, protocol.CodeLimitReached, "concurrency limit reached")

		return
	}

	// Conflict check: exactly one container per session.
	if _, exists := s.tracker.Get(p.SessionID); exists {
		writeError(w, http.StatusConflict, protocol.CodeConflict, "session already active")

		return
	}

	// Create the per-session run directory. The container mounts it at
	// /run/cm-chat; the entrypoint reads resume.jsonl and primer.txt from there.
	runDir := filepath.Join(s.chatRunDirBase, p.SessionID)
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		s.logger.Error("chat/start: mkdir failed", "session_id", p.SessionID, "error", err)
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "internal error")

		return
	}

	// Write resume.jsonl only when a resume context was supplied. Each turn is
	// one JSON line so the entrypoint can stream it with bufio.Scanner.
	if p.Resume != nil {
		if err := writeResumeJSONL(filepath.Join(runDir, "resume.jsonl"), p.Resume.Turns); err != nil {
			s.logger.Error("chat/start: write resume.jsonl failed",
				"session_id", p.SessionID, "error", err)
			writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "internal error")

			return
		}
	}

	// Write primer.txt unconditionally; an empty primer is a zero-byte file.
	if err := os.WriteFile(filepath.Join(runDir, "primer.txt"), []byte(p.Primer), 0o640); err != nil {
		s.logger.Error("chat/start: write primer.txt failed",
			"session_id", p.SessionID, "error", err)
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "internal error")

		return
	}

	env := []string{
		"CM_CHAT_SESSION=" + p.SessionID,
		"CM_MCP_URL=" + s.mcpURL,
		"CM_MCP_API_KEY=" + p.MCPAPIKey,
		"CM_MODEL=" + p.Model,
		"CMX_TOOL_OUTPUT_MAX_BYTES=" + strconv.Itoa(s.toolOutputMaxBytes),
		"CMX_COMPACTION_THRESHOLD=" + strconv.FormatFloat(s.compactionThreshold, 'g', -1, 64),
		"CMX_COMPACTION_KEEP_RECENT_TURNS=" + strconv.Itoa(s.compactionKeepRecentTurns),
		"CMX_BASH_TIMEOUT_MAX_SECONDS=" + strconv.Itoa(s.bashTimeoutMaxSeconds),
	}

	if p.Project != "" {
		env = append(env, "CM_CHAT_PROJECT="+p.Project)
	}

	if p.RepoURL != "" {
		env = append(env, "CM_CHAT_REPO_URL="+p.RepoURL)
	}

	if p.Resume != nil {
		env = append(env, "CM_CHAT_RESUME=1")
	}

	// Resolve task-skills from CM (the single source of truth): fetch the git
	// pointer, clone once, and bind the clone read-only at skillsMountPath. A
	// failure or empty pointer means this session runs without the Skill tool —
	// never fatal to the chat start.
	var skillsHostDir string

	if s.skillsResolver != nil {
		dir, serr := s.skillsResolver.Resolve(r.Context())
		if serr != nil {
			s.logger.Warn("chat/start: task-skills unavailable; launching without skills",
				"session_id", p.SessionID, "error", serr)
		} else {
			skillsHostDir = dir
		}
	}

	if skillsHostDir != "" {
		env = append(env, "CMX_TASK_SKILLS_DIR="+skillsMountPath)
	}

	// Operator-supplied extra env is appended after the system vars so that
	// explicit operator entries take precedence over CM_*/CMX_* defaults for
	// any duplicate keys.
	for k, v := range s.workerExtraEnv {
		env = append(env, k+"="+v)
	}

	binds := []string{
		s.secretsHostDir + ":/run/cm-secrets:ro",
		runDir + ":/run/cm-chat:ro",
	}
	if skillsHostDir != "" {
		binds = append(binds, skillsHostDir+":"+skillsMountPath+":ro")
	}

	spec := executor.LaunchSpec{
		SessionID:   p.SessionID,
		Image:       s.image,
		Env:         env,
		Binds:       binds,
		MemoryBytes: s.memBytes,
		PidsLimit:   s.pidsLimit,
	}

	if err := s.executor.Launch(r.Context(), spec); err != nil {
		if errors.Is(err, executor.ErrCapacity) {
			writeError(w, http.StatusTooManyRequests, protocol.CodeLimitReached, "concurrency limit reached")

			return
		}

		s.logger.Error("chat/start: launch failed", "session_id", p.SessionID, "error", err)
		writeError(w, http.StatusBadGateway, protocol.CodeUpstreamFailure, "launch failed")

		return
	}

	containerID := ""
	if run, ok := s.tracker.Get(p.SessionID); ok {
		containerID = run.ContainerID
	}

	s.logger.Info("chat/start: session started",
		"session_id", p.SessionID, "container_id", containerID)

	writeJSON(w, http.StatusAccepted, protocol.ChatStartResponse{
		OK:          true,
		ContainerID: containerID,
	})
}

// writeResumeJSONL writes one JSON line per ChatResumeTurn to the named file.
// json.Encoder appends a newline after each encoded value, producing well-formed
// JSON Lines output.
func writeResumeJSONL(path string, turns []protocol.ChatResumeTurn) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)

	for i := range turns {
		if err := enc.Encode(turns[i]); err != nil {
			return err
		}
	}

	return nil
}

// handleChatEnd closes the stdin of the tracked chat container, signalling EOF
// to the work process so it exits naturally. It is idempotent: an untracked
// session (already ended or never started) returns 200. A stale or already-
// closed stdin is not a hard error — we log it and return 202 regardless.
func (s *Server) handleChatEnd(w http.ResponseWriter, r *http.Request) {
	var p protocol.ChatEndPayload
	if !s.decode(w, r, &p) {
		return
	}

	run, ok := s.tracker.Get(p.SessionID)
	if !ok {
		// Idempotent: session not found means it was already ended or never started.
		writeJSON(w, http.StatusOK, protocol.SuccessResponse{OK: true})

		return
	}

	mu := s.stdinLock(p.SessionID)
	mu.Lock()
	err := run.Stdin.Close()
	mu.Unlock()

	if err != nil {
		// Best-effort: an already-closed stdin is not fatal on the /chat/end path.
		s.logger.Warn("chat/end: stdin close failed (best-effort)",
			"session_id", p.SessionID, "error", err)
	}

	// Closing stdin alone does not end the session: the container runs with
	// StdinOnce=false, so closing the attach connection does not EOF the worker.
	// Stop the container explicitly (mirrors the runner and agent backends) so
	// waitAndCleanup removes the container and clears the tracker entry —
	// otherwise a later /chat/start for the same session sees it still active and
	// returns 409. Detached ctx: the request ctx may be cancelled once we return,
	// but the stop must run to completion.
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := s.executor.Stop(stopCtx, p.SessionID); err != nil && !errors.Is(err, executor.ErrNotFound) {
		s.logger.Warn("chat/end: container stop failed (tracker may retain session until exit)",
			"session_id", p.SessionID, "error", err)
	}

	s.logger.Info("chat/end: session ended", "session_id", p.SessionID)

	writeJSON(w, http.StatusAccepted, protocol.SuccessResponse{OK: true})
}

// handleMessage delivers a user message frame to the tracked chat container's
// stdin. A /clear content writes a TypeClear frame instead of TypeUserMessage.
// Dedup is keyed by (sessionID, messageID): a retry with an already-delivered
// messageID returns a cached 200 ack without re-writing to stdin.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var p protocol.MessagePayload
	if !s.decode(w, r, &p) {
		return
	}

	// Retried request whose first attempt already delivered: return a cached ack
	// without re-writing the frame. Record only after a successful write, so a
	// 404 or write failure never poisons the cache for a valid retry.
	if s.dedup.Contains(p.SessionID, p.MessageID) {
		writeJSON(w, http.StatusOK, protocol.SuccessResponse{
			OK:        true,
			Message:   "duplicate message acknowledged",
			MessageID: p.MessageID,
		})

		return
	}

	run, ok := s.tracker.Get(p.SessionID)
	if !ok {
		writeError(w, http.StatusNotFound, protocol.CodeNotFound, "no tracked container")

		return
	}

	var frame frames.Frame

	if p.Content == "/clear" {
		frame = frames.Frame{Type: frames.TypeClear}
	} else {
		frame = frames.Frame{
			Type:      frames.TypeUserMessage,
			Content:   p.Content,
			MessageID: p.MessageID,
		}
	}

	mu := s.stdinLock(p.SessionID)
	mu.Lock()
	err := frames.Write(run.Stdin, frame)
	mu.Unlock()

	if err != nil {
		s.logger.Error("message stdin write failed",
			"session_id", p.SessionID, "error", err)
		writeError(w, http.StatusInternalServerError, protocol.CodeInternal, "write failed")

		return
	}

	// Delivered: record the message_id so a retry is deduped.
	s.dedup.Record(p.SessionID, p.MessageID)

	writeJSON(w, http.StatusAccepted, protocol.SuccessResponse{
		OK:        true,
		MessageID: p.MessageID,
	})
}
