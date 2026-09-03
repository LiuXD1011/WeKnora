// Interactive PTY terminals for the docker backend.
//
// ExecStream allocates a TTY exec and hands the hijacked connection back as a
// RemoteTerminalSession, which is what the sandbox workbench's WebSocket
// endpoint pumps browser keystrokes through. It differs from Exec in three
// deliberate ways:
//
//   - No wrapper, no in-container timeout. The wrapper's `timeout -s KILL`
//     would murder a shell the user is still typing into; the terminal's
//     lifetime is the caller's context (the broker's lease), nothing else.
//   - TTY mode means the stream is raw. One-shot execs demultiplex stdout and
//     stderr with stdcopy; a TTY merges them by definition, so the session
//     reads the hijacked connection directly.
//   - Activity is kept alive by the broker (periodic wrapper execs touch the
//     idle-sweep marker). The terminal shell itself never touches the marker,
//     so an unattended-but-open terminal would otherwise look idle.

package sandbox

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// dockerTerminalPollInterval bounds how often Wait re-inspects the exec while
// the foreground process is still running.
const dockerTerminalPollInterval = 750 * time.Millisecond

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

	execOpts := client.ExecCreateOptions{
		Cmd:          req.Command,
		User:         dockerExecUser(req.User),
		WorkingDir:   req.WorkDir,
		Env:          req.Env,
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

	return &dockerTerminalSession{
		api:       c.api,
		execID:    created.ID,
		reader:    attached.Reader,
		conn:      attached.Conn,
		pollEvery: dockerTerminalPollInterval,
	}, nil
}

// dockerTerminalSession is one live PTY exec. Read is owned by the output
// pump, Write/Resize by the input pump; the hijacked connection tolerates
// that concurrency, and ExecResize is an independent HTTP call.
type dockerTerminalSession struct {
	api dockerEngineAPI
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
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	return t.conn.Close()
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
