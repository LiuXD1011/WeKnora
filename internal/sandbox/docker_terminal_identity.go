package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The fixed bootstrap runs before user argv and reports a kernel process
// identity over the private attach stream. No writable PID file is trusted.
// A Docker TTY exec is its own POSIX session; jobs may have different process
// groups but retain this session ID. The marker also follows detached children.
const dockerTerminalBootstrap = `
IFS= read -r state < /proc/$$/stat || exit 125
state=${state##*) }
identity() {
  set -- $state
  sid=$4
  shift 19
  printf '\036weknora-terminal:%s:%s:%s\037' "$$" "$sid" "$1"
}
identity
exec "$@"
`

func readDockerTerminalIdentity(reader *bufio.Reader) (pid, sid, started string, err error) {
	header, err := reader.ReadSlice(0x1f)
	if err != nil {
		return "", "", "", fmt.Errorf("terminal bootstrap: %w", err)
	}
	if len(header) > 256 || !strings.HasPrefix(string(header), "\x1eweknora-terminal:") {
		return "", "", "", errors.New("invalid terminal identity header")
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(string(header), "\x1eweknora-terminal:"), "\x1f"), ":")
	if len(parts) != 3 {
		return "", "", "", errors.New("invalid terminal identity fields")
	}
	for _, value := range parts {
		if n, parseErr := strconv.ParseUint(value, 10, 64); parseErr != nil || n <= 1 {
			return "", "", "", errors.New("invalid terminal identity number")
		}
	}
	if parts[0] != parts[1] {
		return "", "", "", errors.New("terminal is not an isolated POSIX session")
	}
	return parts[0], parts[1], parts[2], nil
}

// This helper runs as the same non-root account in the same container. It
// verifies kernel start time or the inherited random marker before signalling
// collected processes, never accepts a caller-supplied PID/path, and does not
// kill sibling terminals with a different session and marker.
func dockerTerminalCleanupCommand(pid, sid, started, token string) string {
	return fmt.Sprintf(`
found=0
candidates=''
for stat in /proc/[0-9]*/stat; do
  IFS= read -r state < "$stat" 2>/dev/null || continue
  p=${stat#/proc/}; p=${p%%/stat}
  state=${state##*) }; set -- $state
  session=$4; shift 19; started=$1
  marked=0
  if grep -zFxq -- 'WEKNORA_TERMINAL_ID=%s' "/proc/$p/environ" 2>/dev/null; then marked=1; fi
  if [ "$session" = '%s' ] || [ "$marked" = 1 ]; then
    candidates="$candidates $p"
    if [ "$marked" = 1 ] || { [ "$p" = '%s' ] && [ "$started" = '%s' ]; }; then found=1; fi
  fi
done
[ "$found" = 1 ] || exit 0
for p in $candidates; do kill -KILL "$p" 2>/dev/null || true; done
`, token, sid, pid, started)
}
