//go:build final_acceptance

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestDockerFinalAcceptanceTenantAndResourceIsolation(t *testing.T) {
	image := strings.TrimSpace(os.Getenv("FINAL_ACCEPTANCE_IMAGE"))
	require.NotEmpty(t, image, "set FINAL_ACCEPTANCE_IMAGE")
	cfg := DefaultConfig()
	cfg.Type = SandboxTypeDocker
	cfg.DockerHost = "unix:///var/run/docker.sock"
	cfg.DockerImage = image
	cfg.DockerCPULimit = 0.25
	cfg.DockerMemoryBytes = 96 * 1024 * 1024
	cfg.DockerPidsLimit = 64
	cfg.DefaultTimeout = time.Minute

	remote, err := NewDockerRemoteClient(cfg)
	require.NoError(t, err)
	manager, err := NewSessionBoundManager(SessionBoundManagerConfig{
		Config: cfg, Client: remote,
		Store: NewMemorySessionSandboxBindingStore(), Checker: PermissiveSessionExistenceChecker{},
		ConfigID: "final-acceptance", SkipHealthProbe: true,
	})
	require.NoError(t, err)

	const sharedSession = "same-visible-session"
	ctxA := types.WithSandboxTenantID(context.Background(), 2101)
	ctxB := types.WithSandboxTenantID(context.Background(), 2102)
	cleanup := func(ctx context.Context, sessionID string) {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		require.NoError(t, manager.DestroySession(bounded, sessionID))
	}
	t.Cleanup(func() {
		cleanup(ctxA, sharedSession)
		cleanup(ctxB, sharedSession)
	})

	writeScript := func(value string) string {
		return fmt.Sprintf(`
from pathlib import Path
Path(%q).write_text(%q, encoding="utf-8")
print(%q)
`, path.Join(SessionOutputRoot, "tenant.txt"), value, value)
	}
	readIdentity := func(ctx context.Context) string {
		result := executeFinalAcceptance(t, ctx, manager, sharedSession, fmt.Sprintf(`
from pathlib import Path
print(Path(%q).read_text(encoding="utf-8"))
`, path.Join(SessionOutputRoot, "tenant.txt")), 30*time.Second)
		require.True(t, result.IsSuccess(), "%#v", result)
		return strings.TrimSpace(result.Stdout)
	}

	type concurrentWrite struct {
		value  string
		result *ExecuteResult
		err    error
	}
	start := make(chan struct{})
	writes := make(chan concurrentWrite, 2)
	for _, item := range []struct {
		ctx   context.Context
		value string
	}{{ctxA, "tenant-2101-only"}, {ctxB, "tenant-2102-only"}} {
		go func(ctx context.Context, value string) {
			<-start
			result, err := manager.Execute(ctx, &ExecuteConfig{
				Script: "tenant_write.py", ScriptContent: writeScript(value), SessionID: sharedSession,
				Timeout: 30 * time.Second, SkipValidation: true,
				Env: map[string]string{skillOutputEnvVar: SessionOutputRoot},
			})
			writes <- concurrentWrite{value: value, result: result, err: err}
		}(item.ctx, item.value)
	}
	close(start)
	for range 2 {
		write := <-writes
		require.NoError(t, write.err)
		require.NotNil(t, write.result)
		require.True(t, write.result.IsSuccess(), "%s: %#v", write.value, write.result)
		require.Equal(t, write.value, strings.TrimSpace(write.result.Stdout))
	}
	t.Log("CONCURRENT_WRITES=PASS")
	require.Equal(t, "tenant-2101-only", readIdentity(ctxA))
	require.Equal(t, "tenant-2102-only", readIdentity(ctxB))

	summaries, err := remote.List(context.Background(), RemoteListFilter{
		Metadata: map[string]string{remoteMetadataSessionID: sharedSession},
	})
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	tenants := []string{
		summaries[0].Metadata[remoteMetadataTenantID],
		summaries[1].Metadata[remoteMetadataTenantID],
	}
	sort.Strings(tenants)
	require.Equal(t, []string{"2101", "2102"}, tenants)
	require.NotEqual(t, summaries[0].ID, summaries[1].ID)
	t.Logf("TENANT_ISOLATION=PASS containers=%s,%s tenant_labels=%s", summaries[0].ID, summaries[1].ID, strings.Join(tenants, ","))

	cleanup(ctxA, sharedSession)
	require.Equal(t, "tenant-2102-only", readIdentity(ctxB), "destroying tenant A must not affect tenant B")

	const resourceSession = "resource-limits"
	ctxResource := types.WithSandboxTenantID(context.Background(), 2201)
	t.Cleanup(func() { cleanup(ctxResource, resourceSession) })
	probe := executeFinalAcceptance(t, ctxResource, manager, resourceSession, `
from pathlib import Path
root = Path("/sys/fs/cgroup")
def read_first(*names):
    for name in names:
        direct = root / name
        if direct.exists():
            return direct.read_text().strip()
        found = next(root.rglob(name), None)
        if found:
            return found.read_text().strip()
    raise FileNotFoundError(names)
cpu = read_first("cpu.max") if (root / "cpu.max").exists() else read_first("cpu.cfs_quota_us") + " " + read_first("cpu.cfs_period_us")
print("cpu=" + cpu)
print("memory=" + read_first("memory.max", "memory.limit_in_bytes"))
print("pids=" + read_first("pids.max"))
`, 30*time.Second)
	require.True(t, probe.IsSuccess(), "%#v", probe)
	require.Contains(t, probe.Stdout, "cpu=25000 100000")
	require.Contains(t, probe.Stdout, "memory=100663296")
	require.Contains(t, probe.Stdout, "pids=64")

	resourceContainers, err := remote.List(context.Background(), RemoteListFilter{
		Metadata: map[string]string{remoteMetadataSessionID: resourceSession},
	})
	require.NoError(t, err)
	require.Len(t, resourceContainers, 1)
	api, err := sharedDockerEngineClients.get(dockerEndpoint{Host: cfg.DockerHost, Timeout: DefaultDockerHTTPTimeout})
	require.NoError(t, err)
	inspected, err := api.ContainerInspect(context.Background(), resourceContainers[0].ID, client.ContainerInspectOptions{})
	require.NoError(t, err)
	require.NotNil(t, inspected.Container.HostConfig)
	resources := inspected.Container.HostConfig.Resources
	require.Equal(t, int64(250_000_000), resources.NanoCPUs)
	require.Equal(t, int64(100_663_296), resources.Memory)
	require.Equal(t, int64(100_663_296), resources.MemorySwap)
	require.NotNil(t, resources.PidsLimit)
	require.Equal(t, int64(64), *resources.PidsLimit)
	t.Logf("RESOURCE_CGROUP=PASS cpu=%d memory=%d swap=%d pids=%d", resources.NanoCPUs, resources.Memory, resources.MemorySwap, *resources.PidsLimit)

	timed := executeFinalAcceptance(t, ctxResource, manager, resourceSession, `
import os
import time
from pathlib import Path
Path("/workspace/output/timed.pid").write_text(str(os.getpid()))
time.sleep(60)
`, 2*time.Second)
	require.True(t, timed.Killed)
	require.Contains(t, []int{-1, 124, 137}, timed.ExitCode)
	t.Logf("TIME_LIMIT=PASS exit=%d killed=%v", timed.ExitCode, timed.Killed)
	timedProbe := executeFinalAcceptance(t, ctxResource, manager, resourceSession, `
from pathlib import Path
pid = Path("/workspace/output/timed.pid").read_text().strip()
print("survivor=" + ("1" if Path("/proc", pid).exists() else "0"))
`, 30*time.Second)
	require.True(t, timedProbe.IsSuccess(), "%#v", timedProbe)
	require.Equal(t, "survivor=0", strings.TrimSpace(timedProbe.Stdout))

	memory := executeFinalAcceptance(t, ctxResource, manager, resourceSession, `
import os
from pathlib import Path
Path("/workspace/output/memory.pid").write_text(str(os.getpid()))
chunks = []
while True:
    chunks.append(bytearray(16 * 1024 * 1024))
`, 30*time.Second)
	require.True(t, memory.Killed, "%#v", memory)
	require.Contains(t, []int{-1, 137}, memory.ExitCode)
	t.Logf("MEMORY_LIMIT=PASS exit=%d killed=%v", memory.ExitCode, memory.Killed)

	// The persistent sandbox must recover after the over-limit process dies.
	recovered := executeFinalAcceptance(t, ctxResource, manager, resourceSession, `
from pathlib import Path
pid = Path("/workspace/output/memory.pid").read_text().strip()
print("recovered survivor=" + ("1" if Path("/proc", pid).exists() else "0"))
oom_kill = 0
for candidate in list(Path("/sys/fs/cgroup").rglob("memory.events")) + list(Path("/sys/fs/cgroup").rglob("memory.oom_control")):
    for line in candidate.read_text().splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[0] == "oom_kill":
            oom_kill = max(oom_kill, int(fields[1]))
print("oom_kill=" + str(oom_kill))
`, 30*time.Second)
	require.True(t, recovered.IsSuccess(), "%#v", recovered)
	require.Contains(t, recovered.Stdout, "recovered survivor=0")
	require.NotContains(t, recovered.Stdout, "oom_kill=0")
	t.Log("RECOVERY=PASS")
}

func executeFinalAcceptance(
	t *testing.T, ctx context.Context, manager *SessionBoundManager,
	sessionID, script string, timeout time.Duration,
) *ExecuteResult {
	t.Helper()
	result, err := manager.Execute(ctx, &ExecuteConfig{
		Script: "final_acceptance.py", ScriptContent: script, SessionID: sessionID,
		Timeout: timeout, SkipValidation: true,
		Env: map[string]string{skillOutputEnvVar: SessionOutputRoot},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	t.Logf("session=%s exit=%s killed=%v stdout=%q stderr=%q", sessionID, strconv.Itoa(result.ExitCode), result.Killed, result.Stdout, result.Stderr)
	return result
}
