package service

import (
	"context"
	"errors"
	"strings"
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
