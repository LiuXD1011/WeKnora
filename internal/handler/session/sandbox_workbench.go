package session

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"unicode/utf8"

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

// terminalInputSniffer retains only interrupted, never-executed input. Normal
// commands are audited from the shell's post-execution OSC record instead of
// guessed from raw keystrokes.
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
			s.line.Reset()
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
				s.line.Reset()
			}
		}
	}
}

// A disconnect does not execute the unfinished line, so it must not be stored
// as a terminal_command event.
func (s *terminalInputSniffer) flush() {
	s.line.Reset()
}

func (s *terminalInputSniffer) report(interrupted bool) {
	line := s.line.String()
	s.line.Reset()
	s.workbench.AuditTerminalInput(s.ctx, s.sessionID, s.terminalID, line, interrupted)
}

type terminalAuditRecord struct {
	ExitCode  int
	HistoryID string
	Command   string
}

// terminalAuditOSCDecoder strips the private bash prompt-hook OSC sequence
// from the user-visible stream and returns complete execution records. It
// keeps a possible marker suffix between reads because provider chunks may
// split at any byte.
type terminalAuditOSCDecoder struct {
	pending []byte
}

const terminalAuditOSCMaxBytes = 32 << 10

func (d *terminalAuditOSCDecoder) feed(chunk []byte) ([]byte, []terminalAuditRecord) {
	prefix := []byte(service.SandboxWorkbenchTerminalAuditOSCPrefix)
	data := append(append([]byte(nil), d.pending...), chunk...)
	d.pending = nil
	visible := make([]byte, 0, len(data))
	records := make([]terminalAuditRecord, 0, 1)
	for len(data) > 0 {
		index := bytes.Index(data, prefix)
		if index < 0 {
			keep := terminalAuditMarkerSuffix(data, prefix)
			visible = append(visible, data[:len(data)-keep]...)
			d.pending = append(d.pending, data[len(data)-keep:]...)
			break
		}
		visible = append(visible, data[:index]...)
		rest := data[index+len(prefix):]
		end := bytes.IndexByte(rest, '\a')
		if end < 0 {
			d.pending = append(d.pending, data[index:]...)
			if len(d.pending) > terminalAuditOSCMaxBytes {
				d.pending = nil
			}
			break
		}
		if record, ok := parseTerminalAuditOSC(rest[:end]); ok {
			records = append(records, record)
		}
		data = rest[end+1:]
	}
	return visible, records
}

func (d *terminalAuditOSCDecoder) flush() []byte {
	left := d.pending
	d.pending = nil
	return left
}

func terminalAuditMarkerSuffix(data, prefix []byte) int {
	max := len(prefix) - 1
	if len(data) < max {
		max = len(data)
	}
	for size := max; size > 0; size-- {
		if bytes.Equal(data[len(data)-size:], prefix[:size]) {
			return size
		}
	}
	return 0
}

func parseTerminalAuditOSC(payload []byte) (terminalAuditRecord, bool) {
	parts := bytes.SplitN(payload, []byte(";"), 3)
	if len(parts) != 3 || len(parts[1]) == 0 || len(parts[2]) > terminalAuditOSCMaxBytes {
		return terminalAuditRecord{}, false
	}
	exitCode, err := strconv.Atoi(string(parts[0]))
	if err != nil || exitCode < 0 || exitCode > 255 {
		return terminalAuditRecord{}, false
	}
	for _, b := range parts[1] {
		if b < '0' || b > '9' {
			return terminalAuditRecord{}, false
		}
	}
	command, err := base64.StdEncoding.DecodeString(string(parts[2]))
	if err != nil || len(command) == 0 || len(command) > terminalAuditOSCMaxBytes || !utf8.Valid(command) {
		return terminalAuditRecord{}, false
	}
	return terminalAuditRecord{ExitCode: exitCode, HistoryID: string(parts[1]), Command: string(command)}, true
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
		return
	}
	defer conn.Close()
	conn.SetReadLimit(workbenchTerminalWSFrameLimit)
	streamCtx, cancelStream := context.WithCancel(c.Request.Context())
	defer cancelStream()

	cols, rows := parseTerminalSize(c.Query("cols"), c.Query("rows"))
	terminal, err := workbench.OpenTerminal(streamCtx, sessionID, cols, rows)
	if err != nil {
		writeWorkbenchWSError(conn, err)
		return
	}

	var writeMu sync.Mutex
	write := func(kind int, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return err
		}
		return conn.WriteMessage(kind, payload)
	}
	writeText := func(event workbenchWSMessage) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return write(websocket.TextMessage, payload)
	}
	if err := writeText(workbenchWSMessage{Type: "ready", TerminalID: terminal.ID, Backend: terminal.Backend}); err != nil {
		terminal.Close("write_failed", -1)
		return
	}

	// Exactly one reader and one serialised writer own each socket direction.
	// The handler selects process completion/lease/disconnect instead of
	// blocking inside ReadMessage until the browser next sends a frame.
	inputDone := make(chan string, 1)
	inputStopped := make(chan struct{})
	go func() {
		defer close(inputStopped)
		sniffer := &terminalInputSniffer{workbench: workbench, ctx: streamCtx,
			sessionID: sessionID, terminalID: terminal.ID}
		defer sniffer.flush()
		for {
			conn.SetReadDeadline(time.Now().Add(workbenchTerminalReadDeadline))
			kind, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				reason := "client_disconnect"
				if stderrors.Is(readErr, websocket.ErrReadLimit) {
					reason = "frame_too_large"
				}
				inputDone <- reason
				return
			}
			switch kind {
			case websocket.BinaryMessage:
				if _, err := terminal.Session.Write(payload); err != nil {
					inputDone <- "backend_error"
					return
				}
				sniffer.feed(payload)
			case websocket.TextMessage:
				var event workbenchWSMessage
				if json.Unmarshal(payload, &event) != nil {
					continue
				}
				switch event.Type {
				case "resize":
					if event.Cols > 0 && event.Rows > 0 && event.Cols <= 1024 && event.Rows <= 1024 {
						if err := terminal.Session.Resize(event.Cols, event.Rows); err != nil {
							inputDone <- "backend_error"
							return
						}
					}
				case "ping":
					if err := writeText(workbenchWSMessage{Type: "pong", Seq: event.Seq}); err != nil {
						inputDone <- "connection_lost"
						return
					}
				}
			}
		}
	}()

	outputDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 16<<10)
		decoder := &terminalAuditOSCDecoder{}
		for {
			n, readErr := terminal.Session.Read(buf)
			if n > 0 {
				visible, records := decoder.feed(buf[:n])
				for _, record := range records {
					workbench.AuditTerminalCommand(
						streamCtx, sessionID, terminal.ID,
						record.HistoryID, record.Command, record.ExitCode,
					)
				}
				if len(visible) > 0 {
					if err := write(websocket.BinaryMessage, visible); err != nil {
						outputDone <- err
						return
					}
				}
			}
			if readErr != nil {
				if tail := decoder.flush(); len(tail) > 0 {
					if err := write(websocket.BinaryMessage, tail); err != nil {
						outputDone <- err
						return
					}
				}
				outputDone <- readErr
				return
			}
		}
	}()
	type waitResult struct {
		code int
		err  error
	}
	waitDone := make(chan waitResult, 1)
	go func() {
		code, err := terminal.Session.Wait(terminal.Context)
		waitDone <- waitResult{code, err}
	}()

	reason, exitCode := "process_exit", -1
	outputFinished := false
	outputEvents := outputDone
waitLoop:
	for {
		select {
		case result := <-waitDone:
			exitCode = result.code
			if result.err != nil {
				reason = "backend_error"
				if stderrors.Is(terminal.Context.Err(), context.DeadlineExceeded) {
					reason = "lease_expired"
				} else if terminal.Context.Err() != nil {
					reason = "context_cancelled"
				}
			}
			break waitLoop
		case <-terminal.Context.Done():
			reason = "context_cancelled"
			if stderrors.Is(terminal.Context.Err(), context.DeadlineExceeded) {
				reason = "lease_expired"
			}
			break waitLoop
		case reason = <-inputDone:
			break waitLoop
		case outputErr := <-outputEvents:
			outputFinished = true
			outputEvents = nil
			if !stderrors.Is(outputErr, io.EOF) {
				reason = "backend_error"
				break waitLoop
			}
			// EOF commonly arrives just before ExecInspect observes exit.
			// Keep waiting for the authoritative code rather than assuming 0.
		}
	}
	// For normal completion let already-buffered output drain before closing
	// the PTY. A peer that stopped reading cannot hold this open indefinitely.
	if reason == "process_exit" && !outputFinished {
		select {
		case <-outputDone:
		case <-time.After(2 * time.Second):
		}
	}
	// Record once, using the resolved outcome (not an earlier placeholder -1).
	terminal.Close(reason, exitCode)
	_ = writeText(workbenchWSMessage{Type: "exit", Code: exitCode, Reason: reason})
	_ = conn.Close() // releases the input reader without waiting for its deadline
	cancelStream()
	<-inputStopped
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
