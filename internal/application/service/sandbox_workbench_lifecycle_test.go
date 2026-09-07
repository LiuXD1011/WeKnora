package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type workbenchLeaseAudit struct {
	interfaces.AuditLogService
	events chan *types.AuditLog
}

func (a *workbenchLeaseAudit) Log(ctx context.Context, entry *types.AuditLog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.events <- entry
	return nil
}

func TestSandboxWorkbenchLeaseClosesWithoutTransportPump(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			svc, provider, _ := newWorkbenchForTerminalTest()
			audit := &workbenchLeaseAudit{events: make(chan *types.AuditLog, 8)}
			svc.audit = audit
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			terminal, err := svc.OpenTerminal(ctx, "s-1", 80, 24)
			require.NoError(t, err)
			if mode == "cancel" {
				cancel()
			}
			// No handler or caller invokes Close: the service owns lease cleanup.
			require.Eventually(t, func() bool {
				provider.terminals[0].mu.Lock()
				defer provider.terminals[0].mu.Unlock()
				return provider.terminals[0].closed
			}, time.Second, 5*time.Millisecond)
			terminal.Close("late", 9) // waits for closeOnce, must not overwrite outcome
			svc.terminalsMu.Lock()
			count := svc.terminalCounts["s-1"]
			svc.terminalsMu.Unlock()
			require.Zero(t, count)
			var closed []*types.AuditLog
			for len(audit.events) > 0 {
				entry := <-audit.events
				if entry.Action == "sandbox.terminal_closed" {
					closed = append(closed, entry)
				}
			}
			require.Len(t, closed, 1, "close audit must persist after request cancellation")
			var details map[string]any
			require.NoError(t, json.Unmarshal(closed[0].Details, &details))
			reason := "context_cancelled"
			if mode == "deadline" {
				reason = "lease_expired"
			}
			require.Equal(t, reason, details["reason"])
			require.Equal(t, float64(-1), details["exit_code"])
		})
	}
}
