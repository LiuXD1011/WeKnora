package session

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// workbenchTerminalWSFrames bounds one WebSocket message on either direction.
// PTY output arrives in kernel-buffer-sized chunks; 256 KiB comfortably covers
// a burst (cat of a large file) without letting a peer allocate unbounded
// buffers through us.
const workbenchTerminalWSFrameLimit = 256 << 10

// workbenchTerminalReadDeadline is refreshed on every inbound message. The
// frontend pings every 25s, so a silently dead TCP peer frees the terminal
// (and its per-session slot) within about a minute.
const workbenchTerminalReadDeadline = 75 * time.Second

var workbenchTerminalUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	// Same-origin is the default contract. The one exception is a handshake
	// that presented the bearer sub-protocol: its JWT has already been
	// validated by the auth middleware, and a cross-site attacker cannot know
	// the token, so an authenticated terminal can neither be spoofed nor
	// hijacked. This is what lets the vite dev proxy (changeOrigin rewrites
	// Host, not Origin) connect during development.
	CheckOrigin: func(r *http.Request) bool {
		if workbenchSameOrigin(r) {
			return true
		}
		for _, headerValue := range r.Header.Values("Sec-Websocket-Protocol") {
			for _, candidate := range strings.Split(headerValue, ",") {
				if strings.HasPrefix(strings.TrimSpace(candidate), "bearer.") {
					return true
				}
			}
		}
		return false
	},
}

// workbenchSameOrigin mirrors gorilla's built-in check: an Origin header, when
// present, must point at the host the request was sent to.
func workbenchSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return parsed.Host == r.Host
}

type workbenchWSMessage struct {
	Type string `json:"type"`
	Seq  int    `json:"seq,omitempty"`

	// Terminal ID and backend ride the ready event; code/reason ride exit.
	TerminalID string `json:"terminal_id,omitempty"`
	Backend    string `json:"backend,omitempty"`
	Code       int    `json:"code,omitempty"`
	Reason     string `json:"reason,omitempty"`

	// Error events.
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`

	// Resize events.
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// terminalInputSniffer reconstructs typed command lines from the PTY input
// stream so interactive sessions keep the per-command audit promise. It is
// best effort by design: tab completion and history editing mean the recorded
// line is what the user typed, not necessarily a canonical command string.
type terminalInputSniffer struct {
	workbench  *service.SandboxWorkbenchService
	ctx        context.Context
	sessionID  string
	terminalID string
	line       strings.Builder
}

func (s *terminalInputSniffer) feed(chunk []byte) {
	for _, b := range chunk {
		switch {
		case b == '\r' || b == '\n':
			s.report(false)
		case b == 0x03: // Ctrl-C
			s.report(true)
		case b == 0x7f || b == 0x08: // backspace trims the last typed byte
			raw := s.line.String()
			if len(raw) > 0 {
				s.line.Reset()
				s.line.WriteString(raw[:len(raw)-1])
			}
		case b == 0x1b: // escape sequences (arrows, history) drop the partial line
			s.line.Reset()
		case b >= 0x20 && b != 0x7f, b >= 0x80:
			s.line.WriteByte(b)
			if s.line.Len() > 2000 {
				s.report(false)
			}
		}
	}
}

// flush records a trailing command that was still being typed when the
// connection ended.
func (s *terminalInputSniffer) flush() {
	if s.line.Len() > 0 {
		s.report(false)
	}
}

func (s *terminalInputSniffer) report(interrupted bool) {
	line := s.line.String()
	s.line.Reset()
	s.workbench.AuditTerminalInput(s.ctx, s.sessionID, s.terminalID, line, interrupted)
}

// terminalTermination records the first termination reason any pump observed,
// so the exit event and the audit trail say what actually ended the terminal
// rather than whichever pump happened to unwind last.
type terminalTermination struct {
	once  sync.Once
	value string
}

func newTerminalTermination(defaultReason string) *terminalTermination {
	return &terminalTermination{value: defaultReason}
}

func (t *terminalTermination) set(reason string) {
	t.once.Do(func() { t.value = reason })
}

// TerminalSandboxWorkbenchWS upgrades to the interactive terminal stream. The
// connection is bidirectional: binary frames carry raw PTY bytes in both
// directions, text frames carry the control protocol (resize/ping inbound,
// ready/exit/error/pong outbound). Authentication has already happened in the
// middleware chain — the token arrived either as the Authorization header or
// as the "bearer." WebSocket sub-protocol.
func (h *Handler) TerminalSandboxWorkbenchWS(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	sessionID := workbenchSessionID(c)

	conn, err := workbenchTerminalUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade already wrote the HTTP error response.
		return
	}
	defer conn.Close()

	cols, rows := parseTerminalSize(c.Query("cols"), c.Query("rows"))
	terminal, err := workbench.OpenTerminal(c.Request.Context(), sessionID, cols, rows)
	if err != nil {
		writeWorkbenchWSError(conn, err)
		return
	}

	writeMu := &sync.Mutex{}
	writeText := func(message workbenchWSMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(message)
	}
	writeBinary := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.BinaryMessage, data)
	}

	if err := writeText(workbenchWSMessage{Type: "ready", TerminalID: terminal.ID, Backend: terminal.Backend}); err != nil {
		terminal.Close("write_failed", -1)
		return
	}

	termination := newTerminalTermination("process_exit")

	// Output pump: PTY → browser. EOF (or a dead browser connection) ends it.
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buf := make([]byte, 16<<10)
		for {
			n, readErr := terminal.Session.Read(buf)
			if n > 0 {
				if writeErr := writeBinary(buf[:n]); writeErr != nil {
					termination.set("connection_lost")
					return
				}
			}
			if readErr != nil {
				if !stderrors.Is(readErr, io.EOF) {
					termination.set("backend_error")
				}
				return
			}
		}
	}()

	// Wait pump: reports the process exit code, or -1 when the lease (or any
	// other context cancellation) ended the terminal first.
	waitDone := make(chan int, 1)
	go func() {
		code, waitErr := terminal.Session.Wait(c.Request.Context())
		if waitErr != nil {
			termination.set("lease_expired")
			waitDone <- -1
			return
		}
		waitDone <- code
	}()

	// Input pump: browser → PTY. It runs on the handler goroutine and is the
	// clock that keeps the read deadline refreshed; when it ends, the PTY is
	// torn down so Wait cannot hang on a lease the client abandoned.
	sniffer := &terminalInputSniffer{
		workbench:  workbench,
		ctx:        c.Request.Context(),
		sessionID:  sessionID,
		terminalID: terminal.ID,
	}
	defer sniffer.flush()

inputLoop:
	for {
		conn.SetReadDeadline(time.Now().Add(workbenchTerminalReadDeadline))
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			termination.set("client_disconnect")
			break
		}
		switch messageType {
		case websocket.BinaryMessage:
			if len(payload) > workbenchTerminalWSFrameLimit {
				continue
			}
			sniffer.feed(payload)
			if _, err := terminal.Session.Write(payload); err != nil {
				termination.set("backend_error")
				break inputLoop
			}
		case websocket.TextMessage:
			var event workbenchWSMessage
			if json.Unmarshal(payload, &event) != nil {
				continue
			}
			switch event.Type {
			case "resize":
				if event.Cols > 0 && event.Rows > 0 && event.Cols <= 1024 && event.Rows <= 1024 {
					_ = terminal.Session.Resize(event.Cols, event.Rows)
				}
			case "ping":
				_ = writeText(workbenchWSMessage{Type: "pong", Seq: event.Seq})
			}
		}
	}

	terminal.Close(termination.value, -1)
	exitCode := <-waitDone
	_ = writeText(workbenchWSMessage{Type: "exit", Code: exitCode, Reason: termination.value})
	// The audit close entry wants the resolved outcome, so it closes last.
	terminal.Close(termination.value, exitCode)
}

func parseTerminalSize(colsRaw, rowsRaw string) (uint16, uint16) {
	cols, rows := 0, 0
	cols, _ = strconv.Atoi(colsRaw)
	rows, _ = strconv.Atoi(rowsRaw)
	if cols <= 0 || cols > 1024 {
		cols = 80
	}
	if rows <= 0 || rows > 1024 {
		rows = 24
	}
	return uint16(cols), uint16(rows)
}

// writeWorkbenchWSError reports an OpenTerminal failure on the freshly
// upgraded connection, then the caller closes it.
func writeWorkbenchWSError(conn *websocket.Conn, err error) {
	message := workbenchWSMessage{Type: "error", Message: err.Error()}
	switch {
	case stderrors.Is(err, service.ErrSandboxWorkbenchTerminalLimit):
		message.Error = "terminal_limit"
	case stderrors.Is(err, service.ErrSandboxWorkbenchUnsupported):
		message.Error = "unsupported_backend"
	case stderrors.Is(err, service.ErrSandboxWorkbenchNotReady):
		message.Error = "sandbox_not_ready"
	default:
		message.Error = "open_failed"
	}
	_ = conn.WriteJSON(message)
}

type sandboxRenameRequest struct {
	OldPath string `json:"old_path" binding:"required"`
	NewPath string `json:"new_path" binding:"required"`
}

func workbenchSessionID(c *gin.Context) string {
	if id := strings.TrimSpace(c.Param("id")); id != "" {
		return id
	}
	return strings.TrimSpace(c.Param("session_id"))
}

func (h *Handler) requireWorkbench(c *gin.Context) *service.SandboxWorkbenchService {
	if h == nil || h.sandboxWorkbench == nil {
		c.Error(apperrors.NewServiceUnavailableError("sandbox workbench is not configured"))
		return nil
	}
	return h.sandboxWorkbench
}

func writeWorkbenchError(c *gin.Context, err error) {
	switch {
	case stderrors.Is(err, service.ErrSandboxWorkbenchPath):
		c.Error(apperrors.NewBadRequestError(err.Error()))
	case stderrors.Is(err, service.ErrSandboxWorkbenchNotReady):
		c.Error(apperrors.NewConflictError(err.Error()))
	case stderrors.Is(err, service.ErrSandboxWorkbenchUnsupported):
		c.Error(apperrors.NewServiceUnavailableError(err.Error()))
	default:
		logger.ErrorWithFields(c.Request.Context(), err, map[string]interface{}{
			"session_id": workbenchSessionID(c),
		})
		c.Error(apperrors.NewInternalServerError(err.Error()))
	}
}

// GetSandboxWorkbenchInfo reports the session-pinned provider and effective
// capabilities without exposing a provider sandbox ID.
func (h *Handler) GetSandboxWorkbenchInfo(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	info, err := workbench.Info(c.Request.Context(), workbenchSessionID(c))
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": info})
}

// ListSandboxWorkbenchFiles lists files below /workspace/output. The client
// supplies a relative directory only; absolute provider paths are never
// accepted or returned.
func (h *Handler) ListSandboxWorkbenchFiles(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	files, err := workbench.ListFiles(
		c.Request.Context(), workbenchSessionID(c), c.Query("path"),
	)
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": files})
}

// DownloadSandboxWorkbenchFile streams one live artifact after the owned
// session and output-root checks have succeeded.
func (h *Handler) DownloadSandboxWorkbenchFile(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	relativePath := c.Query("path")
	data, _, err := workbench.ReadFile(
		c.Request.Context(), workbenchSessionID(c), relativePath,
	)
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	name := filepath.Base(filepath.FromSlash(relativePath))
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("X-Content-Type-Options", "nosniff")
	if c.Query("disposition") == "inline" {
		c.Header("Content-Disposition", "inline; filename="+fmt.Sprintf("%q", name))
		// If an HTML file is ever opened as a top-level response, keep it unable
		// to reach WeKnora APIs. The UI additionally renders it in a sandboxed
		// iframe with no allow-same-origin token.
		if strings.EqualFold(filepath.Ext(name), ".html") || strings.EqualFold(filepath.Ext(name), ".htm") {
			c.Header("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; img-src data: blob:; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		}
	} else {
		c.Header("Content-Disposition", buildAttachmentHeader(name))
	}
	c.Data(http.StatusOK, contentType, data)
}

// UploadSandboxWorkbenchFile writes one multipart upload below the artifact
// root. Existing files are replaced deliberately, matching ordinary file
// manager semantics.
func (h *Handler) UploadSandboxWorkbenchFile(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.Error(apperrors.NewBadRequestError("file is required"))
		return
	}
	defer file.Close()
	if header.Size > service.SandboxWorkbenchMaxUploadBytes {
		c.Error(apperrors.NewBadRequestError("file exceeds sandbox workbench upload limit"))
		return
	}
	relativePath := strings.TrimSpace(c.PostForm("path"))
	if relativePath == "" {
		relativePath = filepath.ToSlash(filepath.Base(header.Filename))
	}
	limited := io.LimitReader(file, service.SandboxWorkbenchMaxUploadBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	if int64(len(data)) > service.SandboxWorkbenchMaxUploadBytes {
		c.Error(apperrors.NewBadRequestError("file exceeds sandbox workbench upload limit"))
		return
	}
	if err := workbench.WriteFile(c.Request.Context(), workbenchSessionID(c), relativePath, data); err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": gin.H{"path": relativePath}})
}

func (h *Handler) RenameSandboxWorkbenchFile(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	var request sandboxRenameRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	if err := workbench.RenameFile(
		c.Request.Context(), workbenchSessionID(c), request.OldPath, request.NewPath,
	); err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) DeleteSandboxWorkbenchFile(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	if err := workbench.DeleteFile(
		c.Request.Context(), workbenchSessionID(c), c.Query("path"),
	); err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ExecuteSandboxWorkbenchCommand executes inside the server-selected session
// sandbox. Request cancellation interrupts the provider call through ctx.
func (h *Handler) ExecuteSandboxWorkbenchCommand(c *gin.Context) {
	workbench := h.requireWorkbench(c)
	if workbench == nil {
		return
	}
	var request service.SandboxWorkbenchCommand
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	result, err := workbench.ExecuteCommand(
		c.Request.Context(), workbenchSessionID(c), request,
	)
	if err != nil {
		writeWorkbenchError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}
