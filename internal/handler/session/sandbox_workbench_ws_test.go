package session

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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

// --- service-layer fakes mirroring the application service tests ------------

type wsSessionService struct {
	interfaces.SessionService
	session *types.Session
}

func (s *wsSessionService) GetOwnedSession(context.Context, string) (*types.Session, error) {
	return s.session, nil
}

type wsResolver struct {
	mgr sandbox.Manager
}

func (r *wsResolver) Resolve(context.Context, uint64, string) (sandbox.Manager, error) {
	return r.mgr, nil
}

type wsTerminalSession struct {
	mu      sync.Mutex
	echo    []byte
	closed  bool
	resizes [][2]uint16
}

// Read blocks until output is available or the terminal is closed, which is
// the contract the server's output pump expects from a live PTY.
func (t *wsTerminalSession) Read(p []byte) (int, error) {
	for {
		t.mu.Lock()
		if len(t.echo) > 0 {
			n := copy(p, t.echo)
			t.echo = t.echo[n:]
			t.mu.Unlock()
			return n, nil
		}
		closed := t.closed
		t.mu.Unlock()
		if closed {
			return 0, io.EOF
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (t *wsTerminalSession) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Echo back so the test can assert the output pump end to end.
	t.echo = append(t.echo, p...)
	return len(p), nil
}

func (t *wsTerminalSession) Resize(cols, rows uint16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resizes = append(t.resizes, [2]uint16{cols, rows})
	return nil
}

func (t *wsTerminalSession) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

func (t *wsTerminalSession) Wait(context.Context) (int, error) { return 0, nil }

type wsTerminalProvider struct {
	mu       sync.Mutex
	sessions []*wsTerminalSession
}

func (p *wsTerminalProvider) OpenSessionTerminal(
	ctx context.Context, _ string, opts sandbox.SessionTerminalOptions,
) (sandbox.SessionTerminalSession, error) {
	session := &wsTerminalSession{}
	p.sessions = append(p.sessions, session)
	if _, ok := ctx.Deadline(); !ok {
		return nil, context.DeadlineExceeded
	}
	return session, nil
}

type wsManager struct {
	typeName sandbox.SandboxType
	provider sandbox.SessionTerminalProvider
}

func (m *wsManager) Execute(context.Context, *sandbox.ExecuteConfig) (*sandbox.ExecuteResult, error) {
	return nil, nil
}
func (m *wsManager) Cleanup(context.Context) error { return nil }
func (m *wsManager) GetSandbox() sandbox.Sandbox   { return nil }
func (m *wsManager) GetType() sandbox.SandboxType  { return m.typeName }
func (m *wsManager) SessionShellExecutor() sandbox.SessionShellExecutor {
	return nil
}
func (m *wsManager) SessionFileStore() sandbox.SessionFileStore { return nil }
func (m *wsManager) SessionTerminalProvider() sandbox.SessionTerminalProvider {
	return m.provider
}

type wsAudit struct {
	interfaces.AuditLogService
	entries []*types.AuditLog
}

func (a *wsAudit) Log(_ context.Context, entry *types.AuditLog) error {
	a.entries = append(a.entries, entry)
	return nil
}

// newWorkbenchTestHandler builds a handler whose workbench service resolves a
// docker-capable manager with one interactive terminal slot.
func newWorkbenchTestHandler(t *testing.T) (*Handler, *wsTerminalProvider, *wsAudit) {
	t.Helper()
	provider := &wsTerminalProvider{}
	audit := &wsAudit{}
	mgr := &wsManager{typeName: sandbox.SandboxTypeDocker, provider: provider}
	svc := service.NewSandboxWorkbenchService(
		&wsSessionService{session: &types.Session{ID: "s-1", TenantID: 7, SandboxConfigID: "cfg-1"}},
		nil, &wsResolver{mgr: mgr}, nil, nil, audit,
	)
	return &Handler{sandboxWorkbench: svc}, provider, audit
}

// TestTerminalWorkbenchWSRoundTrip drives the full protocol over a real
// WebSocket: ready event, binary keystroke echo, resize forwarding, pong, and
// the audit trail. A single reader goroutine collects everything the server
// sends so assertions never race each other for the socket.
func TestTerminalWorkbenchWSRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, provider, audit := newWorkbenchTestHandler(t)

	router := gin.New()
	router.GET("/api/v1/sessions/:id/sandbox/terminal/ws", handler.TerminalSandboxWorkbenchWS)
	server := httptest.NewServer(router)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http") +
		"/api/v1/sessions/s-1/sandbox/terminal/ws?cols=100&rows=30"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	// 1. ready announces the terminal.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ready workbenchWSMessage
	require.NoError(t, conn.ReadJSON(&ready))
	require.Equal(t, "ready", ready.Type)
	require.Equal(t, string(sandbox.SandboxTypeDocker), ready.Backend)
	require.NotEmpty(t, ready.TerminalID)

	// Reader goroutine: the single consumer of the socket after ready.
	var (
		mu       sync.Mutex
		echo     []byte
		textSeen []workbenchWSMessage
	)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			mu.Lock()
			if messageType == websocket.BinaryMessage {
				echo = append(echo, payload...)
			} else {
				var event workbenchWSMessage
				if json.Unmarshal(payload, &event) == nil {
					textSeen = append(textSeen, event)
				}
			}
			mu.Unlock()
		}
	}()

	// 2. Binary keystrokes reach the PTY and are echoed back.
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("ls -la")))

	// 3. Resize is forwarded to the PTY.
	resizePayload, err := json.Marshal(workbenchWSMessage{Type: "resize", Cols: 120, Rows: 40})
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, resizePayload))

	// 4. Ping gets a pong.
	pingPayload, err := json.Marshal(workbenchWSMessage{Type: "ping", Seq: 3})
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, pingPayload))

	// 5. The typed line hit the interactive audit trail.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(string(echo), "ls -la") &&
			len(textSeen) >= 1 && textSeen[len(textSeen)-1].Type == "pong" &&
			textSeen[len(textSeen)-1].Seq == 3
	}, 5*time.Second, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return len(provider.sessions) == 1 && len(provider.sessions[0].resizes) == 1 &&
			provider.sessions[0].resizes[0] == [2]uint16{120, 40}
	}, 5*time.Second, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		return len(audit.entries) >= 2 &&
			audit.entries[0].Action == "sandbox.terminal_opened" &&
			audit.entries[1].Action == "sandbox.terminal_command" &&
			strings.Contains(string(audit.entries[1].Details), "ls -la")
	}, 5*time.Second, 50*time.Millisecond)

	// 6. Client hang-up ends the terminal and releases the slot.
	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return len(provider.sessions) == 1 && provider.sessions[0].closed
	}, 5*time.Second, 50*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
}

// TestTerminalWorkbenchWSRejectsWhenNoSlotIsLeft proves the limit error is
// surfaced on the socket as a structured error event rather than an HTTP
// status the browser cannot read after upgrade.
func TestTerminalWorkbenchWSRejectsWhenNoSlotIsLeft(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, provider, _ := newWorkbenchTestHandler(t)

	// Exhaust the per-session cap before the browser connects.
	first, err := handler.sandboxWorkbench.OpenTerminal(context.Background(), "s-1", 80, 24)
	require.NoError(t, err)
	second, err := handler.sandboxWorkbench.OpenTerminal(context.Background(), "s-1", 80, 24)
	require.NoError(t, err)
	defer first.Close("test", 0)
	defer second.Close("test", 0)

	router := gin.New()
	router.GET("/api/v1/sessions/:id/sandbox/terminal/ws", handler.TerminalSandboxWorkbenchWS)
	server := httptest.NewServer(router)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http") +
		"/api/v1/sessions/s-1/sandbox/terminal/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var event workbenchWSMessage
	require.NoError(t, conn.ReadJSON(&event))
	require.Equal(t, "error", event.Type)
	require.Equal(t, "terminal_limit", event.Error)

	provider.mu.Lock()
	defer provider.mu.Unlock()
	require.Len(t, provider.sessions, 2)
}

// TestTerminalWorkbenchWSSameOriginCheck locks the CSRF/CSWSH posture:
// same-origin handshakes pass, cross-site ones without credentials fail.
func TestTerminalWorkbenchWSSameOriginCheck(t *testing.T) {
	request := &http.Request{Host: "weknora.example.com:8080"}
	request.Header = http.Header{}

	request.Header.Set("Origin", "http://weknora.example.com:8080")
	require.True(t, workbenchSameOrigin(request))

	request.Header.Set("Origin", "http://evil.example.com")
	require.False(t, workbenchSameOrigin(request))

	request.Header.Del("Origin")
	require.True(t, workbenchSameOrigin(request))
}
