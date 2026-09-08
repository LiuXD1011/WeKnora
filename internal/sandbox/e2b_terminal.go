package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	e2b "github.com/matiasinsaurralde/go-e2b"
)

// Only this adapter can exempt the long-lived PTY Start response from the
// ordinary request timeout. All unary calls keep that timeout, including
// SendInput/Resize/Kill. The stream still has the caller's terminal lease.
type e2bPTYStreamKey struct{}

type e2bRequestTimeoutTransport struct {
	next    http.RoundTripper
	timeout time.Duration
}

func (t *e2bRequestTimeoutTransport) CloseIdleConnections() {
	if next, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		next.CloseIdleConnections()
	}
}

type e2bTimedBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *e2bTimedBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }

func (t *e2bRequestTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/process.Process/Start" && req.Context().Value(e2bPTYStreamKey{}) == true {
		return t.next.RoundTrip(req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), t.timeout)
	resp, err := t.next.RoundTrip(req.Clone(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &e2bTimedBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// ExecStream uses the SDK's PTY service, not aggregate Commands.Run. The SDK
// explicitly supports only its bash login shell; reject custom argv rather
// than silently running a different command.
func (c *E2BRemoteClient) ExecStream(ctx context.Context, handle RemoteSandboxHandle, req RemoteStreamExecRequest) (RemoteTerminalSession, error) {
	sbx, err := e2bHandleSandbox("ExecStream", handle)
	if err != nil {
		return nil, err
	}
	if len(req.Command) != 0 && !(len(req.Command) == 2 && (req.Command[0] == "bash" || req.Command[0] == "/bin/bash") && req.Command[1] == "-l") {
		return nil, e2bInvalidRequest("ExecStream", "E2B PTY supports the default bash login shell only", nil)
	}
	user := req.User
	if user == "" {
		user = DefaultSandboxExecUser
	}
	if user != DefaultSandboxExecUser {
		return nil, e2bInvalidRequest("ExecStream", "terminal must use the sandbox execution account", nil)
	}
	env := map[string]string{}
	for _, entry := range req.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(entry, '\x00') {
			return nil, e2bInvalidRequest("ExecStream", "invalid environment entry", nil)
		}
		env[key] = value
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	marker := hex.EncodeToString(token[:])
	env["WEKNORA_TERMINAL_ID"] = marker
	cols, rows := req.Cols, req.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	streamCtx, cancel := context.WithCancel(context.WithValue(ctx, e2bPTYStreamKey{}, true))
	r, w := io.Pipe()
	s := &e2bTerminalSession{client: c, handle: handle, sandbox: sbx, ctx: streamCtx, cancel: cancel,
		reader: r, writer: w, done: make(chan struct{}), marker: marker, code: -1}
	// Bound the initial acknowledgement separately from the lifetime stream.
	startupTimer := time.AfterFunc(10*time.Second, cancel)
	command, err := sbx.Pty.Create(streamCtx, uint32(cols), uint32(rows),
		e2b.WithPtyUser(user), e2b.WithPtyCwd(req.WorkDir), e2b.WithPtyEnv(env),
		e2b.WithPtyTimeout(0), e2b.WithPtyOnData(func(data []byte) { _, _ = w.Write(data) }))
	startupTimer.Stop()
	if err != nil {
		cancel()
		_ = r.Close()
		_ = w.Close()
		// A missing acknowledgement does not prove that the remote process
		// was never created. Reclaim by the server-generated random marker.
		cleanupErr := s.cleanup()
		return nil, errors.Join(normalizeE2BError("ExecStream", err), cleanupErr)
	}
	s.command = command
	go func() {
		result, err := command.Wait(streamCtx)
		var exitErr *e2b.CommandExitError
		if result != nil {
			s.code = result.ExitCode
		}
		if errors.As(err, &exitErr) {
			s.code = exitErr.ExitCode
			err = nil
		}
		s.waitErr = err
		_ = w.CloseWithError(err)
		close(s.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
		case <-s.done:
		}
		_ = s.Close()
	}()
	return s, nil
}

type e2bTerminalSession struct {
	client    *E2BRemoteClient
	handle    RemoteSandboxHandle
	sandbox   *e2b.Sandbox
	command   *e2b.CommandHandle
	ctx       context.Context
	cancel    context.CancelFunc
	reader    *io.PipeReader
	writer    *io.PipeWriter
	done      chan struct{}
	marker    string
	code      int
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

func (s *e2bTerminalSession) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *e2bTerminalSession) Write(p []byte) (int, error) {
	err := s.sandbox.Pty.SendInput(s.ctx, s.command.PID(), p)
	if err != nil {
		return 0, normalizeE2BError("TerminalWrite", err)
	}
	return len(p), nil
}
func (s *e2bTerminalSession) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return e2bInvalidRequest("TerminalResize", "terminal size must be positive", nil)
	}
	return normalizeE2BError("TerminalResize", s.sandbox.Pty.Resize(s.ctx, s.command.PID(), uint32(cols), uint32(rows)))
}
func (s *e2bTerminalSession) Wait(ctx context.Context) (int, error) {
	select {
	case <-s.done:
		return s.code, s.waitErr
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (s *e2bTerminalSession) cleanup() error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	// Marker matching covers normal and setsid descendants without trusting
	// a writable PID file or signalling another terminal in this sandbox.
	cmd := fmt.Sprintf(`for f in /proc/[0-9]*/environ; do
  if grep -zFxq -- 'WEKNORA_TERMINAL_ID=%s' "$f" 2>/dev/null; then
    p=${f#/proc/}; p=${p%%/environ}; kill -KILL "$p" 2>/dev/null || true
  fi
done`, s.marker)
	result, err := s.client.Exec(ctx, s.handle, RemoteExecRequest{Command: "sh", Args: []string{"-c", cmd}, User: DefaultSandboxExecUser, Timeout: 5 * time.Second})
	if err != nil {
		return err
	}
	if result == nil || result.ExitCode != 0 || result.Killed {
		return errors.New("E2B terminal process cleanup failed")
	}
	return nil
}

func (s *e2bTerminalSession) Close() error {
	s.closeOnce.Do(func() {
		// Release a blocked output callback even if nobody is reading. Remote
		// cleanup uses an independent context after cancellation/disconnect.
		select {
		case <-s.done: // Natural EOF has already drained the synchronous pipe.
		default:
			_ = s.reader.Close()
		}
		s.closeErr = s.cleanup()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
		_, err := s.sandbox.Pty.Kill(ctx, s.command.PID())
		cancel()
		s.closeErr = errors.Join(s.closeErr, err)
		s.cancel()
		_ = s.writer.Close()
	})
	return s.closeErr
}

var _ RemoteStreamExecClient = (*E2BRemoteClient)(nil)
var _ RemoteTerminalSession = (*e2bTerminalSession)(nil)
