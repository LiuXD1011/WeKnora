package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
)

// This is a wire-level protocol fixture, NOT a real E2B sandbox. It exercises
// the pinned SDK, binary Connect framing, account/token headers and lifecycle.
type e2bPTYFixture struct {
	mu          sync.Mutex
	start       []byte
	input       []byte
	size        []byte
	cleanup     string
	cleanupFail bool
	kills       int
	output      chan []byte
	finish      chan struct{}
	finishOnce  sync.Once
}

type e2bPTYRedirect struct {
	target *url.URL
	next   http.RoundTripper
}

func (t e2bPTYRedirect) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = t.target.Scheme
	r.URL.Host = t.target.Host
	return t.next.RoundTrip(r)
}

func ptyBytes(field protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), value)
}
func ptyNumber(field protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, field, protowire.VarintType), value)
}
func ptyField(data []byte, want protowire.Number) []byte {
	for len(data) > 0 {
		field, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil
		}
		data = data[n:]
		if typ == protowire.BytesType {
			value, size := protowire.ConsumeBytes(data)
			if size < 0 {
				return nil
			}
			if field == want {
				return value
			}
			data = data[size:]
		} else {
			size := protowire.ConsumeFieldValue(field, typ, data)
			if size < 0 {
				return nil
			}
			data = data[size:]
		}
	}
	return nil
}
func ptyFrame(w http.ResponseWriter, event []byte) {
	var frame bytes.Buffer
	writeConnectFrame(&frame, ptyBytes(1, event))
	_, _ = w.Write(frame.Bytes())
	w.(http.Flusher).Flush()
}
func ptyEnd(w http.ResponseWriter, code uint64) {
	ptyFrame(w, ptyBytes(3, append(ptyNumber(1, code*2), ptyNumber(2, 1)...)))
	_, _ = w.Write([]byte{2, 0, 0, 0, 2, '{', '}'})
}

func newE2BPTYFixture(t *testing.T) (*E2BRemoteClient, RemoteSandboxHandle, *e2bPTYFixture) {
	t.Helper()
	f := &e2bPTYFixture{output: make(chan []byte, 8), finish: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sandboxes/") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": "fixture-sandbox", "envdAccessToken": "fixture-access-token", "domain": "example.test"})
			return
		}
		if strings.Contains(r.URL.Path, "/v2/sandboxes") {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
			return
		}
		require.Equal(t, "fixture-access-token", r.Header.Get("X-Access-Token"))
		require.Equal(t, basicAuthorizationFor(DefaultSandboxExecUser), r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		if r.URL.Path == "/process.Process/Start" {
			require.Equal(t, "application/connect+proto", r.Header.Get("Content-Type"))
			require.GreaterOrEqual(t, len(body), 5)
			require.Equal(t, int(binary.BigEndian.Uint32(body[1:5])), len(body)-5)
			body = body[5:]
			f.mu.Lock()
			failCleanup := f.cleanupFail && len(ptyField(body, 2)) == 0
			f.mu.Unlock()
			if failCleanup {
				http.Error(w, "fixture cleanup failure", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/connect+proto")
			ptyFrame(w, ptyBytes(1, ptyNumber(1, 123)))
			if len(ptyField(body, 2)) == 0 {
				f.mu.Lock()
				f.cleanup = string(ptyField(body, 1))
				f.mu.Unlock()
				ptyEnd(w, 0)
				return
			}
			f.mu.Lock()
			f.start = body
			f.mu.Unlock()
			ptyFrame(w, ptyBytes(2, ptyBytes(3, []byte("READY\n"))))
			for {
				select {
				case chunk := <-f.output:
					ptyFrame(w, ptyBytes(2, ptyBytes(3, chunk)))
				case <-f.finish:
					ptyEnd(w, 7)
					return
				case <-r.Context().Done():
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/proto")
		f.mu.Lock()
		switch r.URL.Path {
		case "/process.Process/SendInput":
			f.input = body
		case "/process.Process/Update":
			f.size = body
		case "/process.Process/SendSignal":
			f.kills++
			f.finishOnce.Do(func() { close(f.finish) })
		default:
			t.Errorf("unexpected RPC %s", r.URL.Path)
		}
		f.mu.Unlock()
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	client, err := newE2BRemoteClient(&Config{E2BAPIKey: "fixture-api-key", E2BAPIURL: server.URL, E2BHTTPTimeout: 40 * time.Millisecond}, e2bPTYRedirect{u, server.Client().Transport})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	handle, err := client.Connect(ctx, "fixture-sandbox")
	require.NoError(t, err)
	return client, handle, f
}

func TestE2BTerminalInteractiveProtocol(t *testing.T) {
	client, handle, f := newE2BPTYFixture(t)
	streamClient, ok := any(client).(RemoteStreamExecClient)
	require.True(t, ok, "E2B must advertise a real PTY adapter, not one-shot exec")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	term, err := streamClient.ExecStream(ctx, handle, RemoteStreamExecRequest{Command: []string{"bash", "-l"}, WorkDir: "/workspace/output", Cols: 80, Rows: 24})
	require.NoError(t, err)
	defer term.Close()
	reader := bufio.NewReader(term)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "READY\n", line)
	// The 40 ms HTTP timeout must not truncate the PTY output stream.
	time.Sleep(120 * time.Millisecond)
	f.output <- []byte("incremental\n")
	line, err = reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "incremental\n", line)
	n, err := term.Write([]byte("echo hello\r\x03"))
	require.NoError(t, err)
	require.Equal(t, 12, n)
	require.NoError(t, term.Resize(111, 37))
	f.mu.Lock()
	start, input, size := f.start, f.input, f.size
	f.mu.Unlock()
	require.Equal(t, "/bin/bash", string(ptyField(ptyField(start, 1), 1)))
	require.Equal(t, "/workspace/output", string(ptyField(ptyField(start, 1), 4)))
	require.Contains(t, string(start), "WEKNORA_TERMINAL_ID")
	require.Equal(t, []byte("echo hello\r\x03"), ptyField(ptyField(input, 2), 2))
	require.Equal(t, append(ptyNumber(1, 111), ptyNumber(2, 37)...), ptyField(ptyField(size, 2), 1))
	f.finishOnce.Do(func() { close(f.finish) })
	code, err := term.Wait(ctx)
	require.NoError(t, err)
	require.Equal(t, 7, code)
	_, err = reader.ReadByte()
	require.Equal(t, io.EOF, err)
	require.NoError(t, term.Close())
	require.NoError(t, term.Close())
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Contains(t, f.cleanup, "WEKNORA_TERMINAL_ID")
	require.Equal(t, 1, f.kills)
}

func TestE2BTerminalCancellationWithoutReader(t *testing.T) {
	client, handle, f := newE2BPTYFixture(t)
	streamClient, ok := any(client).(RemoteStreamExecClient)
	require.True(t, ok)
	ctx, cancel := context.WithCancel(context.Background())
	term, err := streamClient.ExecStream(ctx, handle, RemoteStreamExecRequest{})
	require.NoError(t, err)
	cancel()
	deadline, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	_, _ = term.Wait(deadline)
	require.NoError(t, deadline.Err(), "cancellation must release blocked output callback")
	require.NoError(t, term.Close())
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 1, f.kills)
	require.NotEmpty(t, f.cleanup)
}

func TestE2BTerminalCloseWithoutReader(t *testing.T) {
	client, handle, f := newE2BPTYFixture(t)
	streamClient, ok := any(client).(RemoteStreamExecClient)
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	term, err := streamClient.ExecStream(ctx, handle, RemoteStreamExecRequest{})
	require.NoError(t, err)
	require.NoError(t, term.Close())
	_, _ = term.Wait(ctx)
	require.NoError(t, ctx.Err())
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 1, f.kills)
}

func TestE2BTerminalReportsCleanupFailureAndStillKills(t *testing.T) {
	client, handle, f := newE2BPTYFixture(t)
	f.mu.Lock()
	f.cleanupFail = true
	f.mu.Unlock()
	streamClient, ok := any(client).(RemoteStreamExecClient)
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	term, err := streamClient.ExecStream(ctx, handle, RemoteStreamExecRequest{})
	require.NoError(t, err)
	require.Error(t, term.Close())
	_, _ = term.Wait(ctx)
	require.NoError(t, ctx.Err())
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 1, f.kills)
}

func TestE2BTerminalRejectsUnsupportedOptions(t *testing.T) {
	client, handle, _ := newE2BPTYFixture(t)
	streamClient, ok := any(client).(RemoteStreamExecClient)
	require.True(t, ok)
	for _, req := range []RemoteStreamExecRequest{{Command: []string{"sh", "-c", "echo ignored"}}, {User: "root"}, {Env: []string{"missing-equals"}}} {
		_, err := streamClient.ExecStream(context.Background(), handle, req)
		require.Error(t, err)
	}
}

func TestE2BTerminalKeepsUnaryHTTPTimeout(t *testing.T) {
	client, _, _ := newE2BPTYFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	require.Error(t, client.Health(ctx))
	require.Less(t, time.Since(started), 500*time.Millisecond)
}
