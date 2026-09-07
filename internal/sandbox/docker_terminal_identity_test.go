package sandbox

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestDockerTerminalIdentityConsumesOnlyPrivateHeader(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\x1eweknora-terminal:42:42:999\x1fwelcome\n"))
	pid, sid, start, err := readDockerTerminalIdentity(r)
	if err != nil || pid != "42" || sid != "42" || start != "999" {
		t.Fatalf("unexpected identity %q %q %q: %v", pid, sid, start, err)
	}
	rest, err := io.ReadAll(r)
	if err != nil || string(rest) != "welcome\n" {
		t.Fatalf("lost user output: %q, %v", rest, err)
	}
}

func TestDockerTerminalIdentityRejectsMalformedAndSharedSessions(t *testing.T) {
	for _, input := range []string{
		"", "welcome\x1f", "\x1eweknora-terminal:1:1:999\x1f",
		"\x1eweknora-terminal:42:43:999\x1f", "\x1eweknora-terminal:42:42:0\x1f",
		"\x1eweknora-terminal:42:42;kill:999\x1f", "\x1eweknora-terminal:-42:42:999\x1f",
		"\x1eweknora-terminal:42:42:999:extra\x1f", strings.Repeat("x", 5000) + "\x1f",
	} {
		if _, _, _, err := readDockerTerminalIdentity(bufio.NewReader(strings.NewReader(input))); err == nil {
			t.Fatalf("accepted malformed identity %q", input)
		}
	}
}

func TestDockerTerminalCleanupRequiresKernelOrMarkerIdentity(t *testing.T) {
	command := dockerTerminalCleanupCommand("42", "42", "999", "aabbcc")
	for _, required := range []string{
		"WEKNORA_TERMINAL_ID=aabbcc", "[ \"$started\" = '999' ]", "[ \"$session\" = '42' ]",
		"[ \"$found\" = 1 ] || exit 0", "kill -KILL \"$p\"", "p=${p%/stat}",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("missing identity gate %q", required)
		}
	}
	if strings.Contains(command, "%!") {
		t.Fatalf("malformed generated command: %s", command)
	}
}
