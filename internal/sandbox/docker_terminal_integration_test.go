//go:build workbench_integration

package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

// This test is opt-in and fails (never silently skips) if its daemon/image
// prerequisite is missing. It owns one no-network, bounded container per case.
func realTerminalContainer(t *testing.T) (*DockerRemoteClient, RemoteSandboxHandle) {
	t.Helper()
	image := os.Getenv("WORKBENCH_INTEGRATION_IMAGE")
	require.NotEmpty(t, image, "set WORKBENCH_INTEGRATION_IMAGE")
	cfg := DefaultConfig()
	cfg.DockerImage = image
	cfg.DockerHost = "unix:///var/run/docker.sock"
	docker, err := NewDockerRemoteClient(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, docker.Health(ctx))
	init, pids := true, int64(64)
	created, err := docker.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: image, Cmd: []string{"sleep", "infinity"}, User: DefaultSandboxExecUser,
			WorkingDir: SessionWorkspaceRoot, Labels: map[string]string{"weknora.test": "terminal-conformance"}},
		HostConfig: &container.HostConfig{NetworkMode: "none", Init: &init,
			Resources: container.Resources{Memory: 64 << 20, MemorySwap: 64 << 20, NanoCPUs: 500000000, PidsLimit: &pids}},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := docker.api.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		require.NoError(t, err)
	})
	_, err = docker.api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
	require.NoError(t, err)
	return docker, &dockerSandboxHandle{id: created.ID}
}

func TestDockerTerminalCloseAndLeaseIntegration(t *testing.T) {
	for _, mode := range []string{"close", "cancel", "deadline", "detached", "shell_exit"} {
		t.Run(mode, func(t *testing.T) {
			docker, handle := realTerminalContainer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 700*time.Millisecond)
				defer cancel()
			}
			command := "trap '' HUP; echo READY; sleep 600 & wait"
			if mode == "detached" {
				command = "trap '' HUP; echo READY; setsid sleep 600 & wait"
			}
			if mode == "shell_exit" {
				command = "trap '' HUP; echo READY; sleep 600 & exit 0"
			}
			terminal, err := docker.ExecStream(ctx, handle, RemoteStreamExecRequest{
				Command: []string{"bash", "-c", command},
				User:    DefaultSandboxExecUser, WorkDir: SessionWorkspaceRoot, Cols: 80, Rows: 24,
			})
			require.NoError(t, err)
			defer terminal.Close()
			line, err := bufio.NewReader(terminal).ReadString('\n')
			require.NoError(t, err)
			require.Contains(t, line, "READY")
			if mode == "close" || mode == "detached" {
				require.NoError(t, terminal.Close())
			} else if mode == "cancel" {
				cancel()
			}
			waitCtx, cancelWait := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancelWait()
			code, err := terminal.Wait(waitCtx)
			require.NoError(t, err, "transport close/cancellation must stop the exec, not just its socket")
			t.Logf("mode=%s exit=%d", mode, code)
			require.NoError(t, terminal.Close()) // also clean jobs after a normally exiting shell
			// Assert on actual processes, not only the parent exec status.
			result, err := docker.Exec(context.Background(), handle, RemoteExecRequest{
				Command: "for p in /proc/[0-9]*/cmdline; do tr '\\0' ' ' < \"$p\" 2>/dev/null; echo; done",
				Shell:   true, User: DefaultSandboxExecUser, Timeout: 3 * time.Second,
			})
			require.NoError(t, err)
			for _, line := range strings.Split(result.Stdout, "\n") {
				require.NotEqual(t, "sleep 600", strings.TrimSpace(line), "background child survived terminal shutdown")
			}
			t.Log(fmt.Sprintf("mode=%s no live sleep 600 child", mode))
		})
	}
}

type terminalOutput struct {
	mu   sync.Mutex
	text strings.Builder
}

func (o *terminalOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.Write(p)
}
func (o *terminalOutput) snapshot() string { o.mu.Lock(); defer o.mu.Unlock(); return o.text.String() }

var terminalANSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func (o *terminalOutput) plain() string {
	return strings.ReplaceAll(terminalANSI.ReplaceAllString(o.snapshot(), ""), "\r", "")
}
func collectTerminalOutput(t *testing.T, terminal RemoteTerminalSession) *terminalOutput {
	t.Helper()
	out := &terminalOutput{}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(out, terminal) }()
	t.Cleanup(func() {
		_ = terminal.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("PTY output reader leaked")
		}
	})
	return out
}

func TestDockerTerminalInteractiveIntegration(t *testing.T) {
	docker, handle := realTerminalContainer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	open := func() (RemoteTerminalSession, *terminalOutput) {
		terminal, err := docker.ExecStream(ctx, handle, RemoteStreamExecRequest{
			Command: []string{"bash", "--noprofile", "--norc", "-i"},
			User:    DefaultSandboxExecUser, WorkDir: SessionWorkspaceRoot, Cols: 80, Rows: 24,
		})
		require.NoError(t, err)
		return terminal, collectTerminalOutput(t, terminal)
	}
	first, out := open()
	sibling, other := open()
	write := func(terminal RemoteTerminalSession, command string) {
		_, err := terminal.Write([]byte(command))
		require.NoError(t, err)
	}
	contains := func(output *terminalOutput, text string) {
		text = strings.ReplaceAll(text, "\r", "")
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(output.plain(), text) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("missing %q; complete terminal output=%q", text, output.plain())
	}
	contains(out, "$ ")
	contains(other, "$ ")
	write(first, "id -u\r")
	contains(out, "\r\n1000\r\n")
	require.NoError(t, first.Resize(111, 37))
	write(first, "stty size\r")
	contains(out, "\r\n37 111\r\n")
	write(first, "for n in 1 2 3; do printf 'tick_%s\\n' \"$n\"; sleep 0.2; done\r")
	contains(out, "\r\ntick_1\r\n")
	require.NotContains(t, out.snapshot(), "\r\ntick_3\r\n", "output must stream before command completes")
	contains(out, "\r\ntick_3\r\n")
	write(first, "echo RUNNING; sleep 600\r")
	contains(out, "\r\nRUNNING\r\n")
	write(first, "\x03")
	write(first, "printf 'INTERRUPTED_%s\\n' \"$?\"\r")
	contains(out, "\r\nINTERRUPTED_130\r\n")
	require.NoError(t, first.Close())
	write(sibling, "printf 'SIBLING_%s\\n' alive\r")
	contains(other, "\r\nSIBLING_alive\r\n")
	t.Log("uid=1000; resize=37x111; streaming-before-completion; Ctrl-C=130; sibling terminal remains alive")
}
