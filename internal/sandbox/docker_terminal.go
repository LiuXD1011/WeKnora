// Interactive PTY terminals for the docker backend.
//
// ExecStream allocates a TTY exec and hands the hijacked connection back as a
// RemoteTerminalSession, which is what the sandbox workbench's WebSocket
// endpoint pumps browser keystrokes through. It differs from Exec in three
// deliberate ways:
//
//   - A fixed bootstrap reports the kernel process/session identity before
//     executing the shell. Context cancellation and Close explicitly clean
//     up this process scope; dropping Docker's attach alone does not kill it.
//   - TTY mode means the stream is raw. One-shot execs demultiplex stdout and
//     stderr with stdcopy; a TTY merges them by definition, so the session
//     reads the hijacked connection directly.
//   - Activity is kept alive by the broker (periodic wrapper execs touch the
//     idle-sweep marker). The terminal shell itself never touches the marker,
//     so an unattended-but-open terminal would otherwise look idle.

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// dockerTerminalPollInterval bounds how often Wait re-inspects the exec while
// the foreground process is still running.
const dockerTerminalPollInterval = 100 * time.Millisecond

// ExecStream opens an interactive TTY exec inside the sandbox and returns it
// as a streaming terminal session. DockerRemoteClient satisfies
// RemoteStreamExecClient through this method.
func (c *DockerRemoteClient) ExecStream(
	ctx context.Context,
	handle RemoteSandboxHandle,
	req RemoteStreamExecRequest,
) (RemoteTerminalSession, error) {
	id, err := dockerHandleID("ExecStream", handle)
	if err != nil {
		return nil, err
	}
	if len(req.Command) == 0 {
		return nil, dockerInvalidRequest("ExecStream", "command is required")
	}

	initialSize := client.ConsoleSize{}
	if req.Cols > 0 && req.Rows > 0 {
		initialSize = client.ConsoleSize{Height: uint(req.Rows), Width: uint(req.Cols)}
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(tokenBytes)
	argv := append([]string{"/bin/sh", "-c", dockerTerminalBootstrap, "weknora-terminal"}, req.Command...)
	env := append(append([]string(nil), req.Env...), "WEKNORA_TERMINAL_ID="+token)
	execOpts := client.ExecCreateOptions{
		Cmd:          argv,
		User:         dockerExecUser(req.User),
		WorkingDir:   req.WorkDir,
		Env:          env,
		TTY:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		ConsoleSize:  initialSize,
	}
	created, err := c.api.ExecCreate(ctx, id, execOpts)
	if err != nil && dockerContainerNotRunning(err) {
		if readyErr := c.ensureRunning(ctx, id, "ExecStream"); readyErr != nil {
			return nil, readyErr
		}
		created, err = c.api.ExecCreate(ctx, id, execOpts)
	}
	if err != nil {
		return nil, dockerError("ExecStream", err)
	}

	// TTY: true tells the transport the stream is raw — no stdcopy
	// demultiplexing — and carries the initial layout with the attach.
	attached, err := c.api.ExecAttach(ctx, created.ID, client.ExecAttachOptions{
		TTY:         true,
		ConsoleSize: initialSize,
	})
	if err != nil {
		return nil, dockerError("ExecStream", err)
	}

	// Bound bootstrap reads independently; clear the transport deadline before
	// handing a long-lived terminal to the broker.
	_ = attached.Conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	pid, sid, started, err := readDockerTerminalIdentity(attached.Reader)
	_ = attached.Conn.SetReadDeadline(time.Time{})
	if err != nil {
		attached.Close()
		// Even if the handshake was interrupted, the random inherited marker
		// identifies the process we just launched without trusting a PID file.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _ = c.Exec(cleanupCtx, handle, RemoteExecRequest{
			Command: dockerTerminalCleanupCommand("", "", "", token),
			Shell:   true, User: dockerExecUser(req.User), Timeout: 5 * time.Second,
		})
		return nil, dockerError("ExecStream", err)
	}
	terminal := &dockerTerminalSession{
		api: c.api, owner: c, handle: handle, user: dockerExecUser(req.User),
		execID: created.ID, reader: attached.Reader, conn: attached.Conn,
		pollEvery: dockerTerminalPollInterval, pid: pid, sid: sid,
		started: started, token: token, done: make(chan struct{}),
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = terminal.Close()
		case <-terminal.done:
		}
	}()
	return terminal, nil
}

// dockerTerminalSession is one live PTY exec. Read is owned by the output
// pump, Write/Resize by the input pump; the hijacked connection tolerates
// that concurrency, and ExecResize is an independent HTTP call.
type dockerTerminalSession struct {
	api                      dockerEngineAPI
	owner                    *DockerRemoteClient
	handle                   RemoteSandboxHandle
	user                     string
	pid, sid, started, token string
	done                     chan struct{}
	closeOnce                sync.Once
	closeErr                 error
	// reader carries the raw merged TTY output (the hijack's stream side);
	// conn carries stdin and the close path.
	reader    io.Reader
	execID    string
	conn      io.ReadWriteCloser
	pollEvery time.Duration

	mu     sync.Mutex
	closed bool
}

func (t *dockerTerminalSession) Read(p []byte) (int, error) {
	n, err := t.reader.Read(p)
	if err != nil && t.isClosedConnError(err) {
		return n, io.EOF
	}
	return n, err
}

func (t *dockerTerminalSession) Write(p []byte) (int, error) {
	return t.conn.Write(p)
}

func (t *dockerTerminalSession) Resize(cols, rows uint16) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := t.api.ExecResize(ctx, t.execID, client.ExecResizeOptions{
		Width: uint(cols), Height: uint(rows),
	})
	if err != nil {
		return dockerError("ExecStream.Resize", err)
	}
	return nil
}

func (t *dockerTerminalSession) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		// Closing the hijack only disconnects stdin/stdout; Docker does NOT
		// kill the exec. Explicitly terminate its verified process scope.
		_ = t.conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		result, err := t.owner.Exec(ctx, t.handle, RemoteExecRequest{
			Command: dockerTerminalCleanupCommand(t.pid, t.sid, t.started, t.token),
			Shell:   true, User: t.user, Timeout: 5 * time.Second,
		})
		if err != nil {
			t.closeErr = err
		} else if result.ExitCode != 0 {
			t.closeErr = fmt.Errorf("terminal cleanup exited %d: %s", result.ExitCode, result.Stderr)
		}
		close(t.done)
	})
	return t.closeErr
}

// Wait polls ExecInspect until the terminal process exits. Closing the
// connection does not immediately flip the exec's Running flag, so callers
// that Close first observe a provider-specific exit code rather than a clean
// one — that is the documented contract of SessionTerminalSession.
func (t *dockerTerminalSession) Wait(ctx context.Context) (int, error) {
	ticker := time.NewTicker(t.pollEvery)
	defer ticker.Stop()
	for {
		inspect, err := t.api.ExecInspect(ctx, t.execID, client.ExecInspectOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return -1, ctx.Err()
			}
			return -1, dockerError("ExecStream.Wait", err)
		}
		if !inspect.Running {
			return inspect.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-ticker.C:
		}
	}
}

// isClosedConnError reports whether err is the read/write failure expected
// after the terminal was closed locally. Those are translated to io.EOF
// instead of surfacing as transport errors on the output pump.
func (t *dockerTerminalSession) isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	msg := err.Error()
	return closed && (strings.Contains(msg, "closed") || strings.Contains(msg, "broken pipe"))
}

var (
	_ RemoteTerminalSession  = (*dockerTerminalSession)(nil)
	_ RemoteStreamExecClient = (*DockerRemoteClient)(nil)
)
