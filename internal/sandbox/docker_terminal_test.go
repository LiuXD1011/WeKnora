package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

// TestExecStreamAllocatesPTYAndStreams verifies the interactive terminal
// contract end to end against the fake engine: a TTY exec with the pinned
// non-root account, raw output on Read, stdin on Write, resize relayouts the
// exec, and Close/Wait settle the session.
func TestExecStreamAllocatesPTYAndStreams(t *testing.T) {
	engine := newFakeDockerEngine()
	engine.execTTYOutput = "welcome\n"
	engine.execExit = 7
	docker := newTestDockerClient(t, engine)

	terminal, err := docker.ExecStream(context.Background(), testHandle("container-1"), RemoteStreamExecRequest{
		Command: []string{"bash", "-l"},
		WorkDir: "/workspace",
		User:    DefaultSandboxExecUser,
		Cols:    120,
		Rows:    36,
	})
	require.NoError(t, err)
	defer terminal.Close()

	// The exec must be a TTY with stdin attached, running the requested argv
	// (no /bin/sh wrapper, no timeout — that contract lives in Exec only),
	// seeded with the requested terminal size.
	require.Len(t, engine.execOptions, 1)
	created := engine.execOptions[0]
	require.True(t, created.TTY)
	require.True(t, created.AttachStdin)
	require.Equal(t, []string{"bash", "-l"}, created.Cmd)
	require.Equal(t, "/workspace", created.WorkingDir)
	require.Equal(t, DefaultSandboxExecUser, created.User)
	require.Equal(t, uint(36), created.ConsoleSize.Height)
	require.Equal(t, uint(120), created.ConsoleSize.Width)
	require.Empty(t, engine.resizeCalls)

	// Raw output arrives on Read.
	buf := make([]byte, 64)
	n, readErr := terminal.Read(buf)
	require.NoError(t, readErr)
	require.Equal(t, "welcome\n", string(buf[:n]))

	// Keystrokes land on the exec's stdin.
	_, writeErr := terminal.Write([]byte("ls -la\r"))
	require.NoError(t, writeErr)
	require.Equal(t, "ls -la\r", engine.execStdin.String())

	// Resize is forwarded to the exec.
	require.NoError(t, terminal.Resize(80, 24))
	require.Len(t, engine.resizeCalls, 1)
	require.Equal(t, uint(80), engine.resizeCalls[0].Width)
	require.Equal(t, uint(24), engine.resizeCalls[0].Height)

	// Settling: not running per the fake, so Wait reports the exit code.
	code, waitErr := terminal.Wait(context.Background())
	require.NoError(t, waitErr)
	require.Equal(t, 7, code)

	require.NoError(t, terminal.Close())
	// Close is idempotent.
	require.NoError(t, terminal.Close())
}

// TestExecStreamRejectsEmptyCommand keeps the argv contract strict: an
// interactive terminal without a shell argv is a caller bug, not an empty
// default shell.
func TestExecStreamRejectsEmptyCommand(t *testing.T) {
	docker := newTestDockerClient(t, newFakeDockerEngine())
	_, err := docker.ExecStream(context.Background(), testHandle("container-1"), RemoteStreamExecRequest{})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "command is required"))
}

// TestExecStreamRequiresRunningContainer exercises the not-running recovery
// path shared with Exec: the first create fails like a paused container, the
// adapter restarts it, and the terminal still opens.
func TestExecStreamRequiresRunningContainer(t *testing.T) {
	engine := newFakeDockerEngine()
	engine.inspect["container-1"] = container.InspectResponse{
		ID:    "container-1",
		State: &container.State{Status: "exited"},
	}
	engine.execNotRunningOnce = true
	docker := newTestDockerClient(t, engine)

	terminal, err := docker.ExecStream(context.Background(), testHandle("container-1"), RemoteStreamExecRequest{
		Command: []string{"bash", "-l"},
	})
	require.NoError(t, err)
	defer terminal.Close()
	require.Len(t, engine.execOptions, 2)
	require.Len(t, engine.started, 1)
}

// TestExecStreamWaitHonoursContext proves Wait does not hang past its context:
// with the fake reporting a still-running exec, a cancelled context ends Wait
// with the context error, which is what the broker's lease expiry relies on.
func TestExecStreamWaitHonoursContext(t *testing.T) {
	engine := newFakeDockerEngine()
	engine.execTTYRunning = true
	docker := newTestDockerClient(t, engine)

	terminal, err := docker.ExecStream(context.Background(), testHandle("container-1"), RemoteStreamExecRequest{
		Command: []string{"bash", "-l"},
	})
	require.NoError(t, err)
	defer terminal.Close()

	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, waitErr := terminal.Wait(waitCtx)
	require.ErrorIs(t, waitErr, context.DeadlineExceeded)
}

// Compile-time guard: the resize options type must keep matching what the
// engine interface forwards.
var _ = client.ExecResizeOptions{}
