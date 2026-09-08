//go:build workbench_integration && e2b_integration

package sandbox

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Requires a real E2B-compatible endpoint and standard WeKnora template.
// Missing prerequisites fail rather than silently passing via Skip.
func TestE2BTerminalRealIntegration(t *testing.T) {
	require.NotEmpty(t, firstNonEmptyEnvironment("E2B_INTEGRATION_API_KEY", "E2B_API_KEY"))
	require.NotEmpty(t, firstNonEmptyEnvironment("E2B_INTEGRATION_TEMPLATE", "E2B_TEMPLATE"))
	cfg := e2bCompatibleConfig(t)
	client, err := NewE2BRemoteClientWithPool(cfg, NewSandboxGatewayTransportPoolWithPolicy(nil, OutboundURLPolicy{AllowPrivate: true}))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	handle, err := client.Create(ctx, RemoteCreateRequest{Metadata: map[string]string{"weknora.test": "e2b-terminal-conformance"}})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		require.NoError(t, client.Delete(cleanup, handle.ID()))
	})
	workdir := path.Join(SessionOutputRoot, fmt.Sprintf("terminal-test-%d", time.Now().UnixNano()))
	setup, err := client.Exec(ctx, handle, RemoteExecRequest{Command: "mkdir", Args: []string{"-p", workdir}, User: DefaultSandboxExecUser, Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, 0, setup.ExitCode)
	open := func(t *testing.T, parent context.Context) (RemoteTerminalSession, *terminalOutput) {
		term, err := client.ExecStream(parent, handle, RemoteStreamExecRequest{WorkDir: workdir, Cols: 80, Rows: 24})
		require.NoError(t, err)
		return term, collectTerminalOutput(t, term)
	}
	waitText := func(t *testing.T, out *terminalOutput, text string) {
		require.Eventually(t, func() bool { return strings.Contains(out.plain(), text) }, 10*time.Second, 20*time.Millisecond, "missing %q", text)
	}
	write := func(t *testing.T, term RemoteTerminalSession, text string) {
		_, err := term.Write([]byte(text))
		require.NoError(t, err)
	}
	first, out := open(t, ctx)
	sibling, other := open(t, ctx)
	write(t, first, "printf 'READY_%s\\n' first\r")
	waitText(t, out, "\nREADY_first\n")
	write(t, sibling, "printf 'READY_%s\\n' sibling\r")
	waitText(t, other, "\nREADY_sibling\n")
	write(t, first, "id -u\r")
	waitText(t, out, "\n1000\n")
	require.NoError(t, first.Resize(111, 37))
	write(t, first, "stty size\r")
	waitText(t, out, "\n37 111\n")
	write(t, first, "for n in 1 2 3; do printf 'tick_%s\\n' \"$n\"; sleep 0.3; done\r")
	waitText(t, out, "\ntick_1\n")
	require.NotContains(t, out.plain(), "\ntick_3\n")
	waitText(t, out, "\ntick_3\n")
	write(t, first, "printf 'RUNNING_%s\\n' job; sleep 600\r")
	waitText(t, out, "\nRUNNING_job\n")
	write(t, first, "\x03")
	write(t, first, "printf 'INTERRUPTED_%s\\n' \"$?\"\r")
	waitText(t, out, "\nINTERRUPTED_130\n")
	require.NoError(t, first.Close())
	write(t, sibling, "printf 'SIBLING_%s\\n' alive\r")
	waitText(t, other, "\nSIBLING_alive\n")
	require.NoError(t, sibling.Close())
	for _, mode := range []string{"close", "cancel", "deadline", "normal_exit"} {
		t.Run(mode, func(t *testing.T) {
			parent, stop := context.WithTimeout(ctx, 20*time.Second)
			defer stop()
			if mode == "deadline" {
				stop()
				parent, stop = context.WithTimeout(ctx, 4*time.Second)
				defer stop()
			}
			term, output := open(t, parent)
			file := path.Join(workdir, mode+".pid")
			write(t, term, "setsid sleep 600 & echo $! > "+ShellQuote(file)+"; printf 'CHILD_%s\\n' ready\r")
			waitText(t, output, "\nCHILD_ready\n")
			data, err := client.ReadFile(ctx, handle, file)
			require.NoError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			require.NoError(t, err)
			require.Greater(t, pid, 1)
			switch mode {
			case "close":
				require.NoError(t, term.Close())
			case "cancel":
				stop()
			case "normal_exit":
				write(t, term, "exit 7\r")
			}
			wait, waitStop := context.WithTimeout(ctx, 10*time.Second)
			defer waitStop()
			code, err := term.Wait(wait)
			require.NoError(t, wait.Err())
			if mode == "normal_exit" {
				require.NoError(t, err)
				require.Equal(t, 7, code)
			}
			require.NoError(t, term.Close())
			probe, err := client.Exec(ctx, handle, RemoteExecRequest{Command: "sh", Args: []string{"-c", fmt.Sprintf("if [ -r /proc/%d/stat ]; then state=$(cat /proc/%d/stat); state=${state##*) }; set -- $state; test \"$1\" = Z; fi", pid, pid)}, User: DefaultSandboxExecUser, Timeout: 10 * time.Second})
			require.NoError(t, err)
			require.Equal(t, 0, probe.ExitCode, "detached child must be gone or reaped")
		})
	}
	t.Log("real E2B: uid=1000; resize=37x111; streaming; Ctrl-C=130; sibling isolation; close/cancel/deadline/normal-exit detached-child cleanup")
}
