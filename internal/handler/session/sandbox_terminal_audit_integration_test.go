//go:build terminal_audit_integration

package session

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type terminalAuditPersistence struct {
	durable interfaces.AuditLogService
	capture *wsAudit
}

func (a *terminalAuditPersistence) Log(ctx context.Context, entry *types.AuditLog) error {
	if err := a.durable.Log(ctx, entry); err != nil {
		return err
	}
	return a.capture.Log(ctx, entry)
}

func (a *terminalAuditPersistence) LogDenied(
	ctx context.Context, c *gin.Context, tenantID uint64, actorUserID, actorRole string, requiredRole types.TenantRole,
) error {
	return a.durable.LogDenied(ctx, c, tenantID, actorUserID, actorRole, requiredRole)
}

func (a *terminalAuditPersistence) List(
	ctx context.Context, tenantID uint64, query *interfaces.AuditLogQuery,
) ([]*types.AuditLog, error) {
	return a.durable.List(ctx, tenantID, query)
}

func (a *terminalAuditPersistence) Purge(ctx context.Context, retentionDays int) (int64, error) {
	return a.durable.Purge(ctx, retentionDays)
}

func TestTerminalAuditRealDockerExecutionAndHistoryRecall(t *testing.T) {
	image := strings.TrimSpace(os.Getenv("TERMINAL_AUDIT_IMAGE"))
	require.NotEmpty(t, image, "set TERMINAL_AUDIT_IMAGE")
	cfg := sandbox.DefaultConfig()
	cfg.Type = sandbox.SandboxTypeDocker
	cfg.DockerHost = "unix:///var/run/docker.sock"
	cfg.DockerImage = image
	remote, err := sandbox.NewDockerRemoteClient(cfg)
	require.NoError(t, err)
	manager, err := sandbox.NewSessionBoundManager(sandbox.SessionBoundManagerConfig{
		Config: cfg, Client: remote,
		Store: sandbox.NewMemorySessionSandboxBindingStore(), Checker: sandbox.PermissiveSessionExistenceChecker{},
		ConfigID: "terminal-audit-integration", SkipHealthProbe: true,
	})
	require.NoError(t, err)
	provider, ok := any(manager).(sandbox.SessionTerminalCapabilityProvider)
	require.True(t, ok)
	require.NotNil(t, provider.SessionTerminalProvider())

	db, err := gorm.Open(sqlite.Open("file:terminal-audit-integration?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.AuditLog{}))
	capture := &wsAudit{}
	audit := &terminalAuditPersistence{
		durable: service.NewAuditLogService(repository.NewAuditLogRepository(db)),
		capture: capture,
	}
	svc := service.NewSandboxWorkbenchService(
		&wsSessionService{session: &types.Session{ID: "audit-session", TenantID: 2301, SandboxConfigID: "cfg-audit"}},
		nil, &wsResolver{mgr: manager}, nil, nil, audit,
	)
	handler := &Handler{sandboxWorkbench: svc}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(types.WithSandboxTenantID(c.Request.Context(), 2301))
		c.Next()
	})
	router.GET("/api/v1/sessions/:id/sandbox/terminal/ws", handler.TerminalSandboxWorkbenchWS)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(types.WithSandboxTenantID(context.Background(), 2301), 30*time.Second)
		defer cancel()
		require.NoError(t, manager.DestroySession(ctx, "audit-session"))
	})

	url := "ws" + strings.TrimPrefix(server.URL, "http") +
		"/api/v1/sessions/audit-session/sandbox/terminal/ws?cols=100&rows=30"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var ready workbenchWSMessage
	require.NoError(t, conn.ReadJSON(&ready))
	t.Logf("READY_EVENT=%+v", ready)
	require.Equal(t, "ready", ready.Type)

	var (
		outputMu sync.Mutex
		output   strings.Builder
	)
	readerDone := make(chan struct{})
	t.Cleanup(func() {
		outputMu.Lock()
		t.Logf("VISIBLE_OUTPUT=%q", output.String())
		outputMu.Unlock()
		capture.mu.Lock()
		for index, entry := range capture.entries {
			t.Logf("AUDIT_%d action=%s details=%s", index, entry.Action, string(entry.Details))
		}
		capture.mu.Unlock()
	})
	go func() {
		defer close(readerDone)
		for {
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			kind, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				outputMu.Lock()
				output.Write(payload)
				outputMu.Unlock()
			}
		}
	}()

	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("echo audit-history\r")))
	require.Eventually(t, func() bool { return terminalShellAuditRecords(capture) == 1 }, 10*time.Second, 50*time.Millisecond)
	// Readline history recall changes the input bytes to an escape sequence, but
	// the execution-layer event must still contain the actual recalled command.
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("\x1b[A\r")))
	require.Eventually(t, func() bool { return terminalShellAuditRecords(capture) == 2 }, 10*time.Second, 50*time.Millisecond)

	capture.mu.Lock()
	var commands []map[string]any
	for _, entry := range capture.entries {
		if entry.Action != "sandbox.terminal_command" {
			continue
		}
		var details map[string]any
		require.NoError(t, json.Unmarshal(entry.Details, &details))
		if details["source"] == "shell" {
			commands = append(commands, details)
		}
	}
	capture.mu.Unlock()
	require.Len(t, commands, 2)
	for _, details := range commands {
		require.Equal(t, "echo audit-history", details["command"])
		require.Equal(t, float64(0), details["exit_code"])
		require.NotEmpty(t, details["history_id"])
	}
	require.NotEqual(t, commands[0]["history_id"], commands[1]["history_id"])
	listed, err := audit.List(context.Background(), 2301, &interfaces.AuditLogQuery{
		Action: types.AuditAction("sandbox.terminal_command"), Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, listed, 2)
	for _, entry := range listed {
		require.Equal(t, types.AuditAction("sandbox.terminal_command"), entry.Action)
		require.Contains(t, string(entry.Details), `"command":"echo audit-history"`)
		require.Contains(t, string(entry.Details), `"source":"shell"`)
	}

	outputMu.Lock()
	visible := output.String()
	outputMu.Unlock()
	require.Contains(t, visible, "audit-history")
	require.NotContains(t, visible, service.SandboxWorkbenchTerminalAuditOSCPrefix)
	t.Logf("EXECUTION_AUDIT=PASS commands=%q history_ids=%v,%v query_rows=%d marker_visible=false",
		commands[0]["command"], commands[0]["history_id"], commands[1]["history_id"], len(listed))

	require.NoError(t, conn.Close())
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("websocket reader did not stop")
	}
}

func terminalShellAuditRecords(audit *wsAudit) int {
	audit.mu.Lock()
	defer audit.mu.Unlock()
	count := 0
	for _, entry := range audit.entries {
		if entry.Action == "sandbox.terminal_command" && strings.Contains(string(entry.Details), `"source":"shell"`) {
			count++
		}
	}
	return count
}
