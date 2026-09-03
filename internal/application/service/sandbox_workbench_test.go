package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type workbenchSessionService struct {
	interfaces.SessionService
	session *types.Session
	err     error
}

func (s *workbenchSessionService) GetOwnedSession(context.Context, string) (*types.Session, error) {
	return s.session, s.err
}

type workbenchResolver struct {
	mgr   sandbox.Manager
	calls int
}

func (r *workbenchResolver) Resolve(context.Context, uint64, string) (sandbox.Manager, error) {
	r.calls++
	return r.mgr, nil
}

type workbenchManager struct {
	typeName sandbox.SandboxType
	store    *workbenchStore
	shell    *workbenchShell
	// terminalProvider is nil by default: backends without a PTY transport
	// keep the capability unadvertised, which is exactly what Info must
	// surface as Interactive=false.
	terminalProvider sandbox.SessionTerminalProvider
}

func (m *workbenchManager) Execute(context.Context, *sandbox.ExecuteConfig) (*sandbox.ExecuteResult, error) {
	return nil, nil
}
func (m *workbenchManager) Cleanup(context.Context) error { return nil }
func (m *workbenchManager) GetSandbox() sandbox.Sandbox   { return nil }
func (m *workbenchManager) GetType() sandbox.SandboxType  { return m.typeName }
func (m *workbenchManager) SessionShellExecutor() sandbox.SessionShellExecutor {
	return m.shell
}
func (m *workbenchManager) SessionFileStore() sandbox.SessionFileStore { return m.store }
func (m *workbenchManager) SessionTerminalProvider() sandbox.SessionTerminalProvider {
	return m.terminalProvider
}

type workbenchStore struct {
	listCalls int
	readCalls int
	writePath string
	entries   []sandbox.RemoteDirEntry
	content   []byte
	stat      *sandbox.RemoteStatEntry
}

func (s *workbenchStore) EnsureSessionDir(context.Context, string, string) error { return nil }
func (s *workbenchStore) ListSessionFiles(context.Context, string, string) ([]sandbox.RemoteDirEntry, error) {
	s.listCalls++
	return s.entries, nil
}
func (s *workbenchStore) StatSessionFile(context.Context, string, string) (*sandbox.RemoteStatEntry, error) {
	return s.stat, nil
}
func (s *workbenchStore) ReadSessionFile(context.Context, string, string) ([]byte, error) {
	s.readCalls++
	return s.content, nil
}
func (s *workbenchStore) WriteSessionInputFile(context.Context, string, string, []byte) error {
	return nil
}
func (s *workbenchStore) WriteSessionWorkspaceFile(_ context.Context, _ string, filePath string, _ []byte) error {
	s.writePath = filePath
	return nil
}
func (s *workbenchStore) RemoveSessionInputPath(context.Context, string, string) error { return nil }

type workbenchShell struct {
	command        string
	workDir        string
	timeout        time.Duration
	result         *sandbox.ExecuteResult
	realpathResult string
}

func (s *workbenchShell) ExecShellCommand(
	_ context.Context, _ string, command, workDir string, timeout time.Duration, _ map[string]string,
) (*sandbox.ExecuteResult, error) {
	s.command, s.workDir, s.timeout = command, workDir, timeout
	if strings.HasPrefix(command, "realpath -m -- ") {
		if s.realpathResult != "" {
			return &sandbox.ExecuteResult{Stdout: s.realpathResult + "\n", ExitCode: 0}, nil
		}
		resolved := strings.Trim(strings.TrimPrefix(command, "realpath -m -- "), "'")
		return &sandbox.ExecuteResult{Stdout: resolved + "\n", ExitCode: 0}, nil
	}
	return s.result, nil
}

type workbenchAudit struct {
	interfaces.AuditLogService
	entries []*types.AuditLog
}

func (a *workbenchAudit) Log(_ context.Context, entry *types.AuditLog) error {
	a.entries = append(a.entries, entry)
	return nil
}

func newWorkbenchForTest(kind sandbox.SandboxType) (*SandboxWorkbenchService, *workbenchStore, *workbenchShell, *workbenchResolver, *workbenchAudit) {
	store := &workbenchStore{
		entries: []sandbox.RemoteDirEntry{
			{Name: "deck.pptx", Path: "/workspace/output/deck.pptx", Type: sandbox.RemoteEntryFile, Size: 12},
			{Name: "passwd", Path: "/etc/passwd", Type: sandbox.RemoteEntryFile, Size: 99},
		},
		content: []byte("hello"),
		stat:    &sandbox.RemoteStatEntry{Path: "/workspace/output/a.txt", Type: sandbox.RemoteEntryFile, Size: 5},
	}
	shell := &workbenchShell{result: &sandbox.ExecuteResult{Stdout: "ok\n", ExitCode: 0, Duration: 5 * time.Millisecond}}
	mgr := &workbenchManager{typeName: kind, store: store, shell: shell}
	resolver := &workbenchResolver{mgr: mgr}
	audit := &workbenchAudit{}
	svc := NewSandboxWorkbenchService(
		&workbenchSessionService{session: &types.Session{ID: "s-1", TenantID: 7, SandboxConfigID: "cfg-1"}},
		nil, resolver, nil, nil, audit,
	)
	return svc, store, shell, resolver, audit
}

func TestCleanArtifactRelativePathRejectsEscapes(t *testing.T) {
	for _, input := range []string{"../secret", "/etc/passwd", `..\\secret`, `folder\\file`} {
		_, _, err := cleanArtifactRelativePath(input, false)
		require.ErrorIs(t, err, ErrSandboxWorkbenchPath, input)
	}
	abs, rel, err := cleanArtifactRelativePath("reports/deck.pptx", false)
	require.NoError(t, err)
	require.Equal(t, "/workspace/output/reports/deck.pptx", abs)
	require.Equal(t, "reports/deck.pptx", rel)
}

func TestSandboxWorkbenchListsOnlyArtifactRootForTwoBackends(t *testing.T) {
	for _, kind := range []sandbox.SandboxType{sandbox.SandboxTypeDocker, sandbox.SandboxTypeCube} {
		t.Run(string(kind), func(t *testing.T) {
			svc, store, _, _, _ := newWorkbenchForTest(kind)
			files, err := svc.ListFiles(context.Background(), "s-1", "")
			require.NoError(t, err)
			require.Equal(t, 1, store.listCalls)
			require.Equal(t, []SandboxWorkbenchFile{{
				Name: "deck.pptx", Path: "deck.pptx", Type: sandbox.RemoteEntryFile, Size: 12,
			}}, files)
		})
	}
}

func TestSandboxWorkbenchRejectsOutsideReadBeforeProvider(t *testing.T) {
	svc, store, _, resolver, _ := newWorkbenchForTest(sandbox.SandboxTypeDocker)
	_, _, err := svc.ReadFile(context.Background(), "s-1", "../../etc/passwd")
	require.ErrorIs(t, err, ErrSandboxWorkbenchPath)
	require.Zero(t, store.readCalls)
	require.Zero(t, resolver.calls)
}

func TestSandboxWorkbenchRejectsSymlinkResolvedOutsideOutput(t *testing.T) {
	svc, store, shell, _, _ := newWorkbenchForTest(sandbox.SandboxTypeDocker)
	shell.realpathResult = "/etc/passwd"
	_, _, err := svc.ReadFile(context.Background(), "s-1", "linked-secret")
	require.ErrorIs(t, err, ErrSandboxWorkbenchPath)
	require.Zero(t, store.readCalls)
}

func TestSandboxWorkbenchRequiresOwnedSession(t *testing.T) {
	resolver := &workbenchResolver{}
	svc := NewSandboxWorkbenchService(
		&workbenchSessionService{err: errors.New("not found")}, nil, resolver, nil, nil, nil,
	)
	_, err := svc.Info(context.Background(), "other-tenant-session")
	require.EqualError(t, err, "not found")
	require.Zero(t, resolver.calls)
}

func TestSandboxWorkbenchWritesOnlyBelowOutput(t *testing.T) {
	svc, store, _, _, audit := newWorkbenchForTest(sandbox.SandboxTypeE2B)
	require.NoError(t, svc.WriteFile(context.Background(), "s-1", "tables/result.csv", []byte("a,b")))
	require.Equal(t, "/workspace/output/tables/result.csv", store.writePath)
	require.Len(t, audit.entries, 1)
	require.Equal(t, types.AuditAction("sandbox.file_written"), audit.entries[0].Action)
}

func TestSandboxWorkbenchCommandIsBoundedAndAudited(t *testing.T) {
	svc, _, shell, _, audit := newWorkbenchForTest(sandbox.SandboxTypeDocker)
	result, err := svc.ExecuteCommand(context.Background(), "s-1", SandboxWorkbenchCommand{
		Command: "printf ok", WorkDir: "output", TimeoutSeconds: 999,
	})
	require.NoError(t, err)
	require.Equal(t, "printf ok", shell.command)
	require.Equal(t, "/workspace/output", shell.workDir)
	require.Equal(t, SandboxWorkbenchMaxCommandTimeout, shell.timeout)
	require.Equal(t, "ok\n", result.Stdout)
	require.Len(t, audit.entries, 1)
	require.Equal(t, types.AuditAction("sandbox.terminal_command"), audit.entries[0].Action)
}

func TestSandboxWorkbenchRenameQuotesBothValidatedPaths(t *testing.T) {
	svc, _, shell, _, _ := newWorkbenchForTest(sandbox.SandboxTypeCube)
	require.NoError(t, svc.RenameFile(context.Background(), "s-1", "old name.txt", "nested/new name.txt"))
	require.Contains(t, shell.command, "'/workspace/output/old name.txt'")
	require.Contains(t, shell.command, "'/workspace/output/nested/new name.txt'")
	require.Equal(t, "/workspace", shell.workDir)
}

// --- interactive terminal fakes ---------------------------------------------

type workbenchTerminalSession struct {
	mu       sync.Mutex
	writeBuf []byte
	closed   bool
	closes   int
}

func (t *workbenchTerminalSession) Read([]byte) (int, error) { return 0, io.EOF }
func (t *workbenchTerminalSession) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writeBuf = append(t.writeBuf, p...)
	return len(p), nil
}
func (t *workbenchTerminalSession) Resize(uint16, uint16) error { return nil }
func (t *workbenchTerminalSession) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.closes++
	return nil
}
func (t *workbenchTerminalSession) Wait(context.Context) (int, error) { return 0, nil }

type workbenchTerminalProvider struct {
	opened    int
	terminals []*workbenchTerminalSession
	// deadlines records the ctx deadline of each open, proving the lease.
	deadlines []time.Time
	// opts records the sandbox options of each open.
	opts []sandbox.SessionTerminalOptions
}

func (p *workbenchTerminalProvider) OpenSessionTerminal(
	ctx context.Context, _ string, opts sandbox.SessionTerminalOptions,
) (sandbox.SessionTerminalSession, error) {
	p.opened++
	p.opts = append(p.opts, opts)
	if deadline, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, deadline)
	} else {
		p.deadlines = append(p.deadlines, time.Time{})
	}
	session := &workbenchTerminalSession{}
	p.terminals = append(p.terminals, session)
	return session, nil
}

func newWorkbenchForTerminalTest() (*SandboxWorkbenchService, *workbenchTerminalProvider, *workbenchAudit) {
	provider := &workbenchTerminalProvider{}
	store := &workbenchStore{
		entries: []sandbox.RemoteDirEntry{},
		stat:    &sandbox.RemoteStatEntry{Path: "/workspace/output/a.txt", Type: sandbox.RemoteEntryFile},
	}
	shell := &workbenchShell{result: &sandbox.ExecuteResult{ExitCode: 0}}
	mgr := &workbenchManager{typeName: sandbox.SandboxTypeDocker, store: store, shell: shell}
	mgr.terminalProvider = provider
	audit := &workbenchAudit{}
	svc := NewSandboxWorkbenchService(
		&workbenchSessionService{session: &types.Session{ID: "s-1", TenantID: 7, SandboxConfigID: "cfg-1"}},
		nil, &workbenchResolver{mgr: mgr}, nil, nil, audit,
	)
	return svc, provider, audit
}

// TestSandboxWorkbenchInfoReportsInteractiveCapability ensures the browser
// learns about PTY support from the capability endpoint instead of probing.
func TestSandboxWorkbenchInfoReportsInteractiveCapability(t *testing.T) {
	withTerminal, _, _ := newWorkbenchForTerminalTest()
	info, err := withTerminal.Info(context.Background(), "s-1")
	require.NoError(t, err)
	require.True(t, info.Interactive)
	require.Equal(t, string(sandbox.SandboxTypeDocker), info.Backend)

	// A backend without the terminal capability (Cube/E2B today) degrades.
	providerless := NewSandboxWorkbenchService(
		&workbenchSessionService{session: &types.Session{ID: "s-1", TenantID: 7, SandboxConfigID: "cfg-1"}},
		nil, &workbenchResolver{mgr: &workbenchManager{typeName: sandbox.SandboxTypeCube,
			store: &workbenchStore{}, shell: &workbenchShell{}}}, nil, nil, nil,
	)
	info, err = providerless.Info(context.Background(), "s-1")
	require.NoError(t, err)
	require.False(t, info.Interactive)
	require.Equal(t, string(sandbox.SandboxTypeCube), info.Backend)
}

// TestSandboxWorkbenchOpenTerminalEnforcesPerSessionCap locks the governance
// contract: at most two live terminals per session, slots freed on Close.
func TestSandboxWorkbenchOpenTerminalEnforcesPerSessionCap(t *testing.T) {
	svc, provider, _ := newWorkbenchForTerminalTest()

	first, err := svc.OpenTerminal(context.Background(), "s-1", 0, 0)
	require.NoError(t, err)
	second, err := svc.OpenTerminal(context.Background(), "s-1", 120, 36)
	require.NoError(t, err)
	_, err = svc.OpenTerminal(context.Background(), "s-1", 80, 24)
	require.ErrorIs(t, err, ErrSandboxWorkbenchTerminalLimit)

	// Defaults applied when the browser sends no size.
	require.Equal(t, uint16(80), provider.opts[0].Cols)
	require.Equal(t, uint16(24), provider.opts[0].Rows)
	require.Equal(t, uint16(120), provider.opts[1].Cols)
	require.Equal(t, uint16(36), provider.opts[1].Rows)

	// Every open runs under the terminal lease.
	lease := time.Until(provider.deadlines[0])
	require.Greater(t, lease, time.Duration(0))
	require.LessOrEqual(t, lease, SandboxWorkbenchTerminalLease)

	// Closing one frees exactly one slot.
	first.Close("test", 0)
	third, err := svc.OpenTerminal(context.Background(), "s-1", 80, 24)
	require.NoError(t, err)
	second.Close("test", 0)
	third.Close("test", 0)
	require.Equal(t, 3, provider.opened)
}

// TestSandboxWorkbenchTerminalLifecycleAudits covers the audit trail of one
// interactive session: opened, per-command lines, closed with the outcome.
func TestSandboxWorkbenchTerminalLifecycleAudits(t *testing.T) {
	svc, provider, audit := newWorkbenchForTerminalTest()

	terminal, err := svc.OpenTerminal(context.Background(), "s-1", 80, 24)
	require.NoError(t, err)
	require.NotEmpty(t, terminal.ID)
	require.Equal(t, string(sandbox.SandboxTypeDocker), terminal.Backend)

	svc.AuditTerminalInput(context.Background(), "s-1", terminal.ID, "ls -la /workspace/output", false)
	svc.AuditTerminalInput(context.Background(), "s-1", terminal.ID, "", true) // bare Ctrl-C
	terminal.Close("process_exit", 0)

	actions := make([]types.AuditAction, 0, len(audit.entries))
	for _, entry := range audit.entries {
		actions = append(actions, entry.Action)
	}
	require.Equal(t, []types.AuditAction{
		"sandbox.terminal_opened",
		"sandbox.terminal_command",
		"sandbox.terminal_command",
		"sandbox.terminal_closed",
	}, actions)

	// The close entry records why the terminal ended.
	closed := audit.entries[len(audit.entries)-1]
	require.Contains(t, string(closed.Details), "process_exit")
	// The interrupted line is stored as a command with the Ctrl-C marker.
	interrupted := audit.entries[len(audit.entries)-2]
	require.Contains(t, string(interrupted.Details), "^C")

	// Closing twice must not double-free the slot or double-audit.
	closesBefore := len(audit.entries)
	terminal.Close("late", 1)
	require.Len(t, audit.entries, closesBefore)
	require.Equal(t, 1, provider.terminals[0].closes)
}

// TestCleanArtifactRelativePathRejectsEncodedTraversals covers the attack
// cases the workbench must refuse regardless of encoding: URL-decoded
// traversal, backslash forms, null bytes and mixed absolute prefixes.
func TestCleanArtifactRelativePathRejectsEncodedTraversals(t *testing.T) {
	for _, input := range []string{
		"../secret",
		"a/../../../etc/shadow",
		"/etc/passwd",
		`..\secret`,
		`C:\Windows\system32`,
		"report\x00.txt",
		"..\x00/secret",
		"./../../x",
	} {
		_, _, err := cleanArtifactRelativePath(input, false)
		require.ErrorIs(t, err, ErrSandboxWorkbenchPath, input)
	}

	// Harmless names survive: a double-encoded traversal decodes once at the
	// HTTP layer and stays a literal file name inside the artifact root.
	abs, rel, err := cleanArtifactRelativePath("reports/%2e%2e/list.pptx", false)
	require.NoError(t, err)
	require.Equal(t, "/workspace/output/reports/%2e%2e/list.pptx", abs)
	require.Equal(t, "reports/%2e%2e/list.pptx", rel)
}

// TestSandboxWorkbenchWriteRejectsOversizedUpload keeps the 20 MiB browser
// upload bound honest at the service boundary, before any provider call.
func TestSandboxWorkbenchWriteRejectsOversizedUpload(t *testing.T) {
	svc, store, _, _, _ := newWorkbenchForTest(sandbox.SandboxTypeDocker)
	oversized := make([]byte, SandboxWorkbenchMaxUploadBytes+1)
	err := svc.WriteFile(context.Background(), "s-1", "big.bin", oversized)
	require.ErrorContains(t, err, "exceeds")
	require.Empty(t, store.writePath)
}
