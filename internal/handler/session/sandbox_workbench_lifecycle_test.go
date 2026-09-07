package session

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type lifecycleTerminal struct {
	r    *io.PipeReader
	w    *io.PipeWriter
	done chan struct{}
	once sync.Once
	code int
	err  error
}

func newLifecycleTerminal() *lifecycleTerminal {
	r, w := io.Pipe()
	return &lifecycleTerminal{r: r, w: w, done: make(chan struct{})}
}
func (t *lifecycleTerminal) finish(code int, err error) {
	t.once.Do(func() { t.code, t.err = code, err; t.w.Close(); close(t.done) })
}
func (t *lifecycleTerminal) Read(p []byte) (int, error)  { return t.r.Read(p) }
func (t *lifecycleTerminal) Write(p []byte) (int, error) { return len(p), nil }
func (t *lifecycleTerminal) Resize(uint16, uint16) error { return nil }
func (t *lifecycleTerminal) Close() error                { t.finish(-1, nil); return nil }
func (t *lifecycleTerminal) Wait(ctx context.Context) (int, error) {
	select {
	case <-t.done:
		return t.code, t.err
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

type lifecycleProvider struct{ terminal *lifecycleTerminal }

func (p *lifecycleProvider) OpenSessionTerminal(context.Context, string, sandbox.SessionTerminalOptions) (sandbox.SessionTerminalSession, error) {
	return p.terminal, nil
}

type lifecycleAudit struct {
	interfaces.AuditLogService
	events chan *types.AuditLog
}

func (a *lifecycleAudit) Log(ctx context.Context, entry *types.AuditLog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.events <- entry
	return nil
}

func openLifecycleSocket(t *testing.T) (*websocket.Conn, *lifecycleTerminal, *lifecycleAudit, <-chan struct{}) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	terminal := newLifecycleTerminal()
	audit := &lifecycleAudit{events: make(chan *types.AuditLog, 32)}
	manager := &wsManager{typeName: sandbox.SandboxTypeDocker, provider: &lifecycleProvider{terminal}}
	svc := service.NewSandboxWorkbenchService(
		&wsSessionService{session: &types.Session{ID: "s-1", TenantID: 7, SandboxConfigID: "cfg-1"}},
		nil, &wsResolver{mgr: manager}, nil, nil, audit,
	)
	h := &Handler{sandboxWorkbench: svc}
	ended := make(chan struct{})
	router := gin.New()
	router.GET("/sessions/:id/terminal", func(c *gin.Context) { defer close(ended); h.TerminalSandboxWorkbenchWS(c) })
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/sessions/s-1/terminal", nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close(); terminal.Close() })
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ready workbenchWSMessage
	require.NoError(t, conn.ReadJSON(&ready))
	require.Equal(t, "ready", ready.Type)
	return conn, terminal, audit, ended
}

func TestTerminalWorkbenchProcessExitWithoutClientInput(t *testing.T) {
	conn, terminal, audit, ended := openLifecycleSocket(t)
	terminal.finish(7, nil)
	// Do NOT send a key, ping or disconnect: process completion must wake the handler.
	var event workbenchWSMessage
	require.NoError(t, conn.ReadJSON(&event), "process exit must not wait for browser input")
	require.Equal(t, "exit", event.Type)
	require.Equal(t, 7, event.Code)
	require.Equal(t, "process_exit", event.Reason)
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not release resources")
	}
	closed := 0
	for len(audit.events) > 0 {
		entry := <-audit.events
		if entry.Action != "sandbox.terminal_closed" {
			continue
		}
		closed++
		var details map[string]any
		require.NoError(t, json.Unmarshal(entry.Details, &details))
		require.Equal(t, float64(7), details["exit_code"])
		require.Equal(t, "process_exit", details["reason"])
	}
	require.Equal(t, 1, closed, "exactly one close audit with the actual outcome")
}

func TestTerminalWorkbenchBackendFailureIsNotLeaseExpiry(t *testing.T) {
	conn, terminal, _, ended := openLifecycleSocket(t)
	terminal.finish(-1, io.ErrUnexpectedEOF)
	var event workbenchWSMessage
	require.NoError(t, conn.ReadJSON(&event))
	require.Equal(t, "backend_error", event.Reason)
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("handler stuck")
	}
}

func TestTerminalWorkbenchOversizedFrameClosesTerminal(t *testing.T) {
	conn, terminal, _, ended := openLifecycleSocket(t)
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, make([]byte, workbenchTerminalWSFrameLimit+1)))
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("oversized message did not terminate connection")
	}
	select {
	case <-terminal.done:
	default:
		t.Fatal("oversized frame leaked terminal")
	}
}
