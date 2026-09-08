//go:build workbench_integration

package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
)

// These tests run the real service path rules and Docker file/exec adapter.
// Session ownership/resolution is a fixture, not authentication acceptance.
type artifactDockerHandle string

func (h artifactDockerHandle) ID() string                       { return string(h) }
func (h artifactDockerHandle) Provider() sandbox.RemoteProvider { return sandbox.SandboxTypeDocker }
func (h artifactDockerHandle) Metadata() map[string]string      { return nil }

type artifactFileFallback interface{ sandbox.SessionFileStore }

type artifactDockerManager struct {
	sandbox.Manager
	artifactFileFallback
	client *sandbox.DockerRemoteClient
	handle sandbox.RemoteSandboxHandle
}

func (m *artifactDockerManager) GetType() sandbox.SandboxType                       { return sandbox.SandboxTypeDocker }
func (m *artifactDockerManager) SessionShellExecutor() sandbox.SessionShellExecutor { return m }
func (m *artifactDockerManager) SessionFileStore() sandbox.SessionFileStore         { return m }
func (m *artifactDockerManager) ExecShellCommand(ctx context.Context, _ string, command, workdir string, timeout time.Duration, env map[string]string) (*sandbox.ExecuteResult, error) {
	r, err := m.client.Exec(ctx, m.handle, sandbox.RemoteExecRequest{Command: command, Shell: true, WorkDir: workdir, Timeout: timeout, Env: env, User: sandbox.DefaultSandboxExecUser})
	if err != nil {
		return nil, err
	}
	return &sandbox.ExecuteResult{Stdout: r.Stdout, Stderr: r.Stderr, ExitCode: r.ExitCode, Duration: r.Duration}, nil
}
func (m *artifactDockerManager) ListSessionFiles(ctx context.Context, _ string, p string) ([]sandbox.RemoteDirEntry, error) {
	return m.client.ListDir(ctx, m.handle, p)
}
func (m *artifactDockerManager) StatSessionFile(ctx context.Context, _ string, p string) (*sandbox.RemoteStatEntry, error) {
	return m.client.Stat(ctx, m.handle, p)
}
func (m *artifactDockerManager) ReadSessionFile(ctx context.Context, _ string, p string) ([]byte, error) {
	return m.client.ReadFile(ctx, m.handle, p)
}
func (m *artifactDockerManager) WriteSessionWorkspaceFile(ctx context.Context, _ string, p string, b []byte) error {
	return m.client.WriteFile(ctx, m.handle, p, b)
}

func realArtifactWorkbench(t *testing.T) (*SandboxWorkbenchService, *artifactDockerManager, context.Context) {
	t.Helper()
	image := os.Getenv("WORKBENCH_INTEGRATION_IMAGE")
	require.NotEmpty(t, image, "set WORKBENCH_INTEGRATION_IMAGE")
	api, err := client.New(client.WithHost("unix:///var/run/docker.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = api.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	init, pids := true, int64(64)
	created, err := api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: image, Cmd: []string{"sleep", "infinity"}, User: sandbox.DefaultSandboxExecUser, WorkingDir: sandbox.SessionWorkspaceRoot, Labels: map[string]string{"weknora.test": "artifact-path-conformance"}},
		HostConfig: &container.HostConfig{NetworkMode: "none", Init: &init, Resources: container.Resources{Memory: 64 << 20, MemorySwap: 64 << 20, NanoCPUs: 500000000, PidsLimit: &pids}},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, err := api.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true})
		require.NoError(t, err)
	})
	_, err = api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
	require.NoError(t, err)
	cfg := sandbox.DefaultConfig()
	cfg.DockerHost = "unix:///var/run/docker.sock"
	docker, err := sandbox.NewDockerRemoteClient(cfg)
	require.NoError(t, err)
	mgr := &artifactDockerManager{client: docker, handle: artifactDockerHandle(created.ID)}
	svc := NewSandboxWorkbenchService(&workbenchSessionService{session: &types.Session{ID: "artifact-session", TenantID: 7, SandboxConfigID: "artifact-config"}}, nil, &workbenchResolver{mgr: mgr}, nil, nil, nil)
	return svc, mgr, ctx
}

func artifactSetup(t *testing.T, ctx context.Context, mgr *artifactDockerManager, command string) {
	t.Helper()
	r, err := mgr.ExecShellCommand(ctx, "artifact-session", command, sandbox.SessionWorkspaceRoot, 10*time.Second, nil)
	require.NoError(t, err)
	require.Equal(t, 0, r.ExitCode, r.Stderr)
}

func TestWorkbenchArtifactRealCRUD(t *testing.T) {
	svc, _, ctx := realArtifactWorkbench(t)
	payload := []byte{0, 1, 2, 255, 10, 39, 34}
	require.NoError(t, svc.WriteFile(ctx, "artifact-session", "reports/data.bin", payload))
	files, err := svc.ListFiles(ctx, "artifact-session", "reports")
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "reports/data.bin", files[0].Path)
	data, stat, err := svc.ReadFile(ctx, "artifact-session", "reports/data.bin")
	require.NoError(t, err)
	require.Equal(t, payload, data)
	require.Equal(t, int64(len(payload)), stat.Size)
	require.NoError(t, svc.RenameFile(ctx, "artifact-session", "reports/data.bin", "moved/name ' quoted.bin"))
	data, _, err = svc.ReadFile(ctx, "artifact-session", "moved/name ' quoted.bin")
	require.NoError(t, err)
	require.Equal(t, payload, data)
	require.NoError(t, svc.DeleteFile(ctx, "artifact-session", "moved/name ' quoted.bin"))
	_, _, err = svc.ReadFile(ctx, "artifact-session", "moved/name ' quoted.bin")
	require.Error(t, err)
	_, err = svc.ListFiles(ctx, "artifact-session", "")
	require.NoError(t, err, "artifact root listing must remain usable")
}

func TestWorkbenchArtifactRealRejectsSymlinkDirectory(t *testing.T) {
	svc, mgr, ctx := realArtifactWorkbench(t)
	artifactSetup(t, ctx, mgr, "mkdir -p /workspace/private/sub; printf marker > /workspace/private/sub/private.txt; ln -s /workspace/private /workspace/output/jump")
	files, err := svc.ListFiles(ctx, "artifact-session", "jump/sub")
	require.ErrorIs(t, err, ErrSandboxWorkbenchPath, "outside-root entries must not be listed: %v", files)
	_, _, err = svc.ReadFile(ctx, "artifact-session", "jump/sub/private.txt")
	require.ErrorIs(t, err, ErrSandboxWorkbenchPath)
	require.ErrorIs(t, svc.WriteFile(ctx, "artifact-session", "jump/sub/new.txt", []byte("no")), ErrSandboxWorkbenchPath)
	require.ErrorIs(t, svc.DeleteFile(ctx, "artifact-session", "jump/sub/private.txt"), ErrSandboxWorkbenchPath)
	require.ErrorIs(t, svc.RenameFile(ctx, "artifact-session", "jump/sub/private.txt", "stolen.txt"), ErrSandboxWorkbenchPath)
	data, err := mgr.client.ReadFile(ctx, mgr.handle, "/workspace/private/sub/private.txt")
	require.NoError(t, err)
	require.Equal(t, "marker", string(data))
}

func TestWorkbenchArtifactRealRejectsRedirectedRoot(t *testing.T) {
	svc, mgr, ctx := realArtifactWorkbench(t)
	artifactSetup(t, ctx, mgr, "mkdir -p /workspace/private/sub; mv /workspace/output /workspace/original-output; ln -s /workspace/private /workspace/output")
	_, err := svc.ListFiles(ctx, "artifact-session", "")
	require.ErrorIs(t, err, ErrSandboxWorkbenchPath)
}

func TestWorkbenchArtifactRealRejectsLexicalEscapes(t *testing.T) {
	svc, _, ctx := realArtifactWorkbench(t)
	for _, p := range []string{"../private", "/etc/passwd", `..\private`} {
		_, err := svc.ListFiles(ctx, "artifact-session", p)
		require.ErrorIs(t, err, ErrSandboxWorkbenchPath)
		_, _, err = svc.ReadFile(ctx, "artifact-session", p)
		require.ErrorIs(t, err, ErrSandboxWorkbenchPath)
		require.ErrorIs(t, svc.WriteFile(ctx, "artifact-session", p, []byte("no")), ErrSandboxWorkbenchPath)
		require.ErrorIs(t, svc.DeleteFile(ctx, "artifact-session", p), ErrSandboxWorkbenchPath)
	}
}
