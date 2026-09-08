package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	// SandboxWorkbenchMaxUploadBytes bounds one browser upload before it reaches
	// a provider adapter. The handler applies the same limit while reading the
	// multipart body so an oversized request is never buffered in memory.
	SandboxWorkbenchMaxUploadBytes int64 = 20 << 20

	// Terminal calls are deliberately short lived. The provider's CPU, memory
	// and process limits remain authoritative; this cap prevents a browser tab
	// from turning the request handler into an unbounded job runner.
	SandboxWorkbenchMaxCommandTimeout = 5 * time.Minute

	// SandboxWorkbenchMaxTerminalsPerSession caps concurrent interactive
	// terminals per session. Two covers the "watch the agent while typing"
	// case without letting abandoned tabs pin provider connections forever.
	SandboxWorkbenchMaxTerminalsPerSession = 2

	// SandboxWorkbenchTerminalLease is the hard lifetime of one interactive
	// terminal. It is the 时长 half of the resource-limit acceptance item:
	// when the lease ends the PTY dies with its context, and the browser
	// receives an exit event whose reason says so.
	SandboxWorkbenchTerminalLease = 30 * time.Minute

	// SandboxWorkbenchTerminalKeepAliveInterval refreshes the docker
	// idle-sweep activity marker while a terminal is open. The terminal shell
	// runs without the exec wrapper, so nothing else would touch the marker
	// and the sweeper would reap the container after DefaultDockerIdleTTL.
	SandboxWorkbenchTerminalKeepAliveInterval = 4 * time.Minute

	// SandboxWorkbenchTerminalAuditOSCPrefix starts a private OSC record emitted
	// by the bash prompt hook after a command actually finishes. The handler
	// removes the record from terminal output and persists its command/status.
	SandboxWorkbenchTerminalAuditOSCPrefix = "\x1b]6973;"

	// The first prompt only arms the hook. Later prompts read bash's committed
	// history entry, so readline history selection, completion and cursor edits
	// are audited as the command that actually ran rather than raw keystrokes.
	// Payload: OSC 6973 ; exit_status ; history_number ; base64(command) BEL.
	workbenchTerminalAuditPromptCommand = `__wk_rc=$?; HISTCONTROL=; HISTIGNORE=; __wk_hist="$(HISTTIMEFORMAT= builtin history 1 2>/dev/null)"; read -r __wk_num __wk_cmd <<< "$__wk_hist"; if [ -z "${__WEKNORA_AUDIT_READY:-}" ]; then __WEKNORA_AUDIT_READY=1; elif [ -n "$__wk_num" ] && [ "$__wk_num" != "${__WEKNORA_LAST_HIST:-}" ]; then __wk_b64="$(printf %s "$__wk_cmd" | base64 | tr -d '\n')"; printf '\033]6973;%s;%s;%s\007' "$__wk_rc" "$__wk_num" "$__wk_b64"; fi; __WEKNORA_LAST_HIST="$__wk_num"`
)

var (
	ErrSandboxWorkbenchNotReady    = errors.New("sandbox workbench: session has no live sandbox")
	ErrSandboxWorkbenchUnsupported = errors.New("sandbox workbench: backend does not support this capability")
	ErrSandboxWorkbenchPath        = errors.New("sandbox workbench: path must stay inside the artifact directory")
)

// ErrSandboxWorkbenchTerminalLimit rejects a new interactive terminal when the
// session already holds the maximum number of live ones.
var ErrSandboxWorkbenchTerminalLimit = errors.New(
	"sandbox workbench: session already has the maximum number of terminals",
)

// SandboxWorkbenchFile is the browser-safe projection of a provider entry.
// Path is always relative to /workspace/output; provider identifiers and
// absolute sandbox paths never cross the API boundary.
type SandboxWorkbenchFile struct {
	Name                 string                     `json:"name"`
	Path                 string                     `json:"path"`
	Type                 sandbox.RemoteDirEntryType `json:"type"`
	Size                 int64                      `json:"size"`
	ModTime              time.Time                  `json:"mod_time"`
	ArtifactType         string                     `json:"artifact_type,omitempty"`
	PreviewFormat        string                     `json:"preview_format,omitempty"`
	MediaType            string                     `json:"media_type,omitempty"`
	ClassificationSource string                     `json:"classification_source,omitempty"`
}

// SandboxWorkbenchCommand is one terminal invocation. WorkDir is relative to
// /workspace; an empty value selects /workspace itself.
type SandboxWorkbenchCommand struct {
	Command        string `json:"command"`
	WorkDir        string `json:"work_dir,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// SandboxWorkbenchCommandResult is provider-neutral and intentionally mirrors
// the fields the terminal UI needs rather than leaking a backend SDK response.
type SandboxWorkbenchCommandResult struct {
	Stdout     string        `json:"stdout"`
	Stderr     string        `json:"stderr"`
	ExitCode   int           `json:"exit_code"`
	Duration   time.Duration `json:"-"`
	DurationMS int64         `json:"duration_ms"`
	Killed     bool          `json:"killed"`
	Error      string        `json:"error,omitempty"`
}

type SandboxWorkbenchInfo struct {
	Backend      string `json:"backend"`
	ArtifactRoot string `json:"artifact_root"`
	Terminal     bool   `json:"terminal"`
	Files        bool   `json:"files"`
	// Interactive reports that the backend offers a real PTY over the
	// WebSocket terminal. When false the frontend degrades to one-shot exec
	// commands instead of pretending to be an interactive shell.
	Interactive bool `json:"interactive"`
}

// SandboxWorkbenchService exposes the live session sandbox without letting a
// browser choose a provider handle. Every operation first resolves the owned
// session and its server-side sandbox_config_id pin.
type SandboxWorkbenchService struct {
	sessions interfaces.SessionService
	fallback sandbox.Manager
	resolver sandbox.TenantSandboxResolver
	pinner   *SessionSandboxPinner
	policy   WorkspaceSandboxPolicy
	audit    interfaces.AuditLogService

	// terminalsMu guards terminalCounts, the per-session live-terminal census
	// behind SandboxWorkbenchMaxTerminalsPerSession.
	terminalsMu    sync.Mutex
	terminalCounts map[string]int
}

func NewSandboxWorkbenchService(
	sessions interfaces.SessionService,
	fallback sandbox.Manager,
	resolver sandbox.TenantSandboxResolver,
	pinner *SessionSandboxPinner,
	policy WorkspaceSandboxPolicy,
	audit interfaces.AuditLogService,
) *SandboxWorkbenchService {
	return &SandboxWorkbenchService{
		sessions:       sessions,
		fallback:       fallback,
		resolver:       resolver,
		pinner:         pinner,
		policy:         policy,
		audit:          audit,
		terminalCounts: make(map[string]int),
	}
}

// cleanArtifactRelativePath accepts only a relative path and anchors it under
// /workspace/output. Absolute paths, backslashes, null bytes and traversal
// are rejected rather than normalised, so callers get a clear server-side
// denial. URL-encoded traversals arrive here already decoded by the HTTP
// layer and fold into the same checks; double-encoded sequences stay as
// literal file names, which cannot escape the root.
func cleanArtifactRelativePath(value string, allowRoot bool) (string, string, error) {
	raw := strings.TrimSpace(value)
	if strings.Contains(raw, "\\") || strings.HasPrefix(raw, "/") {
		return "", "", ErrSandboxWorkbenchPath
	}
	if strings.ContainsRune(raw, '\x00') {
		return "", "", ErrSandboxWorkbenchPath
	}
	clean := path.Clean(raw)
	if clean == "." {
		if allowRoot && raw == "" {
			return sandbox.SessionOutputRoot, "", nil
		}
		return "", "", ErrSandboxWorkbenchPath
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", "", ErrSandboxWorkbenchPath
	}
	abs := path.Join(sandbox.SessionOutputRoot, clean)
	if abs == sandbox.SessionOutputRoot || !strings.HasPrefix(abs, sandbox.SessionOutputRoot+"/") {
		return "", "", ErrSandboxWorkbenchPath
	}
	return abs, clean, nil
}

func cleanWorkbenchDir(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return sandbox.SessionWorkspaceRoot, nil
	}
	if strings.Contains(raw, "\\") || strings.HasPrefix(raw, "/") {
		return "", ErrSandboxWorkbenchPath
	}
	clean := path.Clean(raw)
	if clean == "." {
		return sandbox.SessionWorkspaceRoot, nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", ErrSandboxWorkbenchPath
	}
	abs := path.Join(sandbox.SessionWorkspaceRoot, clean)
	if !strings.HasPrefix(abs, sandbox.SessionWorkspaceRoot+"/") {
		return "", ErrSandboxWorkbenchPath
	}
	return abs, nil
}

func (s *SandboxWorkbenchService) ownedManager(
	ctx context.Context, sessionID string,
) (sandbox.Manager, *types.Session, error) {
	if s == nil || s.sessions == nil || strings.TrimSpace(sessionID) == "" {
		return nil, nil, ErrSandboxWorkbenchNotReady
	}
	// GetOwnedSession is the critical tenant/user isolation gate. GetSession is
	// intentionally not used: admins may read channel sessions but must not run
	// commands or inspect their live files.
	session, err := s.sessions.GetOwnedSession(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		return nil, nil, err
	}
	configID := strings.TrimSpace(session.SandboxConfigID)
	if configID == "" && s.pinner != nil {
		configID, err = s.pinner.Read(ctx, session.ID)
		if err != nil {
			return nil, nil, err
		}
	}
	if configID == "" {
		return nil, session, ErrSandboxWorkbenchNotReady
	}
	mgr, err := resolveTenantSandboxForConfig(
		ctx, s.resolver, s.fallback, session.TenantID, configID, s.policy,
	)
	if err != nil {
		return nil, session, err
	}
	if mgr == nil || !sandbox.IsNamedSandboxBackendType(string(mgr.GetType())) {
		return nil, session, ErrSandboxWorkbenchNotReady
	}
	return mgr, session, nil
}

func (s *SandboxWorkbenchService) fileStore(
	ctx context.Context, sessionID string,
) (sandbox.SessionFileStore, *types.Session, error) {
	mgr, session, err := s.ownedManager(ctx, sessionID)
	if err != nil {
		return nil, session, err
	}
	provider, ok := mgr.(sandbox.SessionCapabilityProvider)
	if !ok || provider.SessionFileStore() == nil {
		return nil, session, ErrSandboxWorkbenchUnsupported
	}
	return provider.SessionFileStore(), session, nil
}

func (s *SandboxWorkbenchService) shellExecutor(
	ctx context.Context, sessionID string,
) (sandbox.SessionShellExecutor, *types.Session, error) {
	mgr, session, err := s.ownedManager(ctx, sessionID)
	if err != nil {
		return nil, session, err
	}
	provider, ok := mgr.(sandbox.SessionCapabilityProvider)
	if !ok || provider.SessionShellExecutor() == nil {
		return nil, session, ErrSandboxWorkbenchUnsupported
	}
	return provider.SessionShellExecutor(), session, nil
}

// assertResolvedArtifactPath rejects pre-existing symlink escapes left by
// lexical cleaning. realpath -m resolves existing components even when the
// final upload target does not exist; the returned canonical path must still
// be below /workspace/output. The check happens inside the selected sandbox,
// never against the WeKnora host filesystem. Resolution and the subsequent
// operation are separate RPCs: this is not an atomic no-symlink filesystem
// primitive, and does not defend against concurrent component replacement.
func (s *SandboxWorkbenchService) assertResolvedArtifactPath(
	ctx context.Context, sessionID, absolutePath string, allowRoot bool,
) error {
	executor, _, err := s.shellExecutor(ctx, sessionID)
	if err != nil {
		return err
	}
	result, err := executor.ExecShellCommand(
		ctx, sessionID, "realpath -m -- "+sandbox.ShellQuote(absolutePath),
		sandbox.SessionWorkspaceRoot, 10*time.Second, nil,
	)
	if err != nil {
		return err
	}
	if result == nil || !result.IsSuccess() {
		return fmt.Errorf("sandbox workbench: cannot resolve artifact path")
	}
	resolved := strings.TrimSpace(result.Stdout)
	if allowRoot && resolved == sandbox.SessionOutputRoot {
		return nil
	}
	if resolved == sandbox.SessionOutputRoot || !strings.HasPrefix(resolved, sandbox.SessionOutputRoot+"/") {
		return ErrSandboxWorkbenchPath
	}
	return nil
}

func (s *SandboxWorkbenchService) Info(
	ctx context.Context, sessionID string,
) (*SandboxWorkbenchInfo, error) {
	mgr, _, err := s.ownedManager(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	info := &SandboxWorkbenchInfo{
		Backend:      string(mgr.GetType()),
		ArtifactRoot: sandbox.SessionOutputRoot,
	}
	if provider, ok := mgr.(sandbox.SessionCapabilityProvider); ok {
		info.Terminal = provider.SessionShellExecutor() != nil
		info.Files = provider.SessionFileStore() != nil
	}
	if provider, ok := mgr.(sandbox.SessionTerminalCapabilityProvider); ok {
		info.Interactive = provider.SessionTerminalProvider() != nil
	}
	return info, nil
}

// ListFiles returns only entries that remain inside the artifact root.
func (s *SandboxWorkbenchService) ListFiles(
	ctx context.Context, sessionID, relativeDir string,
) ([]SandboxWorkbenchFile, error) {
	absDir, _, err := cleanArtifactRelativePath(relativeDir, true)
	if err != nil {
		return nil, err
	}
	store, _, err := s.fileStore(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// Directory traversal resolves intermediate symlinks just like a read.
	// Lexical filtering of the returned names alone does not confine it.
	if err := s.assertResolvedArtifactPath(ctx, sessionID, absDir, true); err != nil {
		return nil, err
	}
	entries, err := store.ListSessionFiles(ctx, sessionID, absDir)
	if err != nil {
		return nil, err
	}
	var declarations map[string]workbenchArtifactDeclaration
	if data, readErr := store.ReadSessionFile(ctx, sessionID, path.Join(absDir, workbenchArtifactManifestName)); readErr == nil {
		declarations = parseWorkbenchArtifactManifest(data)
	}
	files := make([]SandboxWorkbenchFile, 0, len(entries))
	for _, entry := range entries {
		if entry.Name == workbenchArtifactManifestName || entry.Name == workbenchArtifactManifestName+".tmp" {
			continue
		}
		clean := path.Clean(entry.Path)
		if clean == sandbox.SessionOutputRoot || !strings.HasPrefix(clean, sandbox.SessionOutputRoot+"/") {
			continue
		}
		file := SandboxWorkbenchFile{
			Name:    entry.Name,
			Path:    strings.TrimPrefix(clean, sandbox.SessionOutputRoot+"/"),
			Type:    entry.Type,
			Size:    entry.Size,
			ModTime: entry.ModTime,
		}
		classifyWorkbenchArtifact(&file)
		if declared, ok := declarations[entry.Name]; ok {
			applyWorkbenchArtifactDeclaration(&file, declared)
		}
		files = append(files, file)
	}
	return files, nil
}

func (s *SandboxWorkbenchService) ReadFile(
	ctx context.Context, sessionID, relativePath string,
) ([]byte, *sandbox.RemoteStatEntry, error) {
	absPath, _, err := cleanArtifactRelativePath(relativePath, false)
	if err != nil {
		return nil, nil, err
	}
	store, _, err := s.fileStore(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.assertResolvedArtifactPath(ctx, sessionID, absPath, false); err != nil {
		return nil, nil, err
	}
	stat, err := store.StatSessionFile(ctx, sessionID, absPath)
	if err != nil {
		return nil, nil, err
	}
	if stat == nil || stat.Type != sandbox.RemoteEntryFile {
		return nil, nil, fmt.Errorf("sandbox workbench: %s is not a regular file", relativePath)
	}
	if stat.Size > SandboxWorkbenchMaxUploadBytes {
		return nil, nil, fmt.Errorf("sandbox workbench: file exceeds %d bytes", SandboxWorkbenchMaxUploadBytes)
	}
	data, err := store.ReadSessionFile(ctx, sessionID, absPath)
	return data, stat, err
}

func (s *SandboxWorkbenchService) WriteFile(
	ctx context.Context, sessionID, relativePath string, content []byte,
) error {
	if int64(len(content)) > SandboxWorkbenchMaxUploadBytes {
		return fmt.Errorf("sandbox workbench: upload exceeds %d bytes", SandboxWorkbenchMaxUploadBytes)
	}
	absPath, _, err := cleanArtifactRelativePath(relativePath, false)
	if err != nil {
		return err
	}
	store, session, err := s.fileStore(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := s.assertResolvedArtifactPath(ctx, sessionID, absPath, false); err != nil {
		return err
	}
	err = store.WriteSessionWorkspaceFile(ctx, sessionID, absPath, content)
	s.auditFileMutation(ctx, session, "sandbox.file_written", relativePath, err)
	return err
}

func (s *SandboxWorkbenchService) RenameFile(
	ctx context.Context, sessionID, oldRelativePath, newRelativePath string,
) error {
	oldAbs, _, err := cleanArtifactRelativePath(oldRelativePath, false)
	if err != nil {
		return err
	}
	newAbs, _, err := cleanArtifactRelativePath(newRelativePath, false)
	if err != nil {
		return err
	}
	executor, session, err := s.shellExecutor(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := s.assertResolvedArtifactPath(ctx, sessionID, oldAbs, false); err != nil {
		return err
	}
	if err := s.assertResolvedArtifactPath(ctx, sessionID, newAbs, false); err != nil {
		return err
	}
	command := "mkdir -p -- " + sandbox.ShellQuote(path.Dir(newAbs)) +
		" && mv -- " + sandbox.ShellQuote(oldAbs) + " " + sandbox.ShellQuote(newAbs)
	result, err := executor.ExecShellCommand(ctx, sessionID, command, sandbox.SessionWorkspaceRoot, 30*time.Second, nil)
	if err == nil && (result == nil || !result.IsSuccess()) {
		if result == nil {
			err = errors.New("sandbox workbench: rename returned no result")
		} else {
			err = fmt.Errorf("sandbox workbench: rename failed: %s", strings.TrimSpace(result.Stderr+" "+result.Error))
		}
	}
	s.auditFileMutation(ctx, session, "sandbox.file_renamed", oldRelativePath+" -> "+newRelativePath, err)
	return err
}

func (s *SandboxWorkbenchService) DeleteFile(
	ctx context.Context, sessionID, relativePath string,
) error {
	absPath, _, err := cleanArtifactRelativePath(relativePath, false)
	if err != nil {
		return err
	}
	executor, session, err := s.shellExecutor(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := s.assertResolvedArtifactPath(ctx, sessionID, absPath, false); err != nil {
		return err
	}
	result, err := executor.ExecShellCommand(
		ctx, sessionID, "rm -rf -- "+sandbox.ShellQuote(absPath),
		sandbox.SessionWorkspaceRoot, 30*time.Second, nil,
	)
	if err == nil && (result == nil || !result.IsSuccess()) {
		if result == nil {
			err = errors.New("sandbox workbench: delete returned no result")
		} else {
			err = fmt.Errorf("sandbox workbench: delete failed: %s", strings.TrimSpace(result.Stderr+" "+result.Error))
		}
	}
	s.auditFileMutation(ctx, session, "sandbox.file_deleted", relativePath, err)
	return err
}

func (s *SandboxWorkbenchService) ExecuteCommand(
	ctx context.Context, sessionID string, request SandboxWorkbenchCommand,
) (*SandboxWorkbenchCommandResult, error) {
	request.Command = strings.TrimSpace(request.Command)
	if request.Command == "" {
		return nil, errors.New("sandbox workbench: command is required")
	}
	workDir, err := cleanWorkbenchDir(request.WorkDir)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(request.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > SandboxWorkbenchMaxCommandTimeout {
		timeout = SandboxWorkbenchMaxCommandTimeout
	}
	executor, session, err := s.shellExecutor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	result, execErr := executor.ExecShellCommand(ctx, sessionID, request.Command, workDir, timeout, nil)
	if result == nil {
		result = &sandbox.ExecuteResult{ExitCode: -1}
	}
	if result.Duration <= 0 {
		result.Duration = time.Since(started)
	}
	response := &SandboxWorkbenchCommandResult{
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		ExitCode:   result.ExitCode,
		Duration:   result.Duration,
		DurationMS: result.Duration.Milliseconds(),
		Killed:     result.Killed,
		Error:      result.Error,
	}
	s.auditCommand(ctx, session, request, response, execErr)
	return response, execErr
}

func (s *SandboxWorkbenchService) auditFileMutation(
	ctx context.Context, session *types.Session, action, target string, operationErr error,
) {
	if s == nil || s.audit == nil || session == nil {
		return
	}
	details, _ := json.Marshal(map[string]any{"path": target})
	outcome := types.AuditOutcomeSuccess
	if operationErr != nil {
		outcome = types.AuditOutcomeFailed
	}
	actor, _ := types.UserIDFromContext(ctx)
	_ = s.audit.Log(ctx, &types.AuditLog{
		TenantID: session.TenantID, ActorUserID: actor,
		ActorRole: string(types.TenantRoleFromContext(ctx)),
		Action:    types.AuditAction(action), ScopeType: "session", ScopeID: session.ID,
		TargetType: "sandbox_file", TargetID: target, Outcome: outcome,
		Details: types.JSON(details),
	})
}

func (s *SandboxWorkbenchService) auditCommand(
	ctx context.Context, session *types.Session, request SandboxWorkbenchCommand,
	result *SandboxWorkbenchCommandResult, operationErr error,
) {
	if s == nil || s.audit == nil || session == nil {
		return
	}
	outcome := types.AuditOutcomeSuccess
	if operationErr != nil || result == nil || result.ExitCode != 0 || result.Killed {
		outcome = types.AuditOutcomeFailed
	}
	detailsMap := map[string]any{
		"command":         request.Command,
		"work_dir":        request.WorkDir,
		"timeout_seconds": request.TimeoutSeconds,
	}
	if result != nil {
		detailsMap["exit_code"] = result.ExitCode
		detailsMap["duration_ms"] = result.DurationMS
		detailsMap["killed"] = result.Killed
	}
	details, _ := json.Marshal(detailsMap)
	actor, _ := types.UserIDFromContext(ctx)
	_ = s.audit.Log(ctx, &types.AuditLog{
		TenantID: session.TenantID, ActorUserID: actor,
		ActorRole: string(types.TenantRoleFromContext(ctx)),
		Action:    types.AuditAction("sandbox.terminal_command"),
		ScopeType: "session", ScopeID: session.ID,
		TargetType: "sandbox_session", TargetID: session.ID,
		Outcome: outcome, Details: types.JSON(details),
	})
}

// WorkbenchTerminal is one live interactive terminal opened through the
// broker. Close is idempotent and must be called by the WebSocket pump when
// the browser disconnects, the lease expires, or the process exits.
type WorkbenchTerminal struct {
	ID      string
	Backend string
	Session sandbox.SessionTerminalSession
	// Context carries the actual lease, not just the HTTP request lifetime.
	// Transport pumps must observe it even while the browser sends no input.
	Context context.Context

	closeOnce sync.Once
	closeFunc func(reason string, exitCode int)
}

// Close releases the terminal: it tears down the PTY, stops the keep-alive,
// frees the per-session slot, and writes the close audit entry. The reason
// and exit code land in the audit details verbatim.
func (t *WorkbenchTerminal) Close(reason string, exitCode int) {
	t.closeOnce.Do(func() { t.closeFunc(reason, exitCode) })
}

func newTerminalID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("term-%d", time.Now().UnixNano())
	}
	return "term-" + hex.EncodeToString(buf)
}

// terminalProvider resolves the owned session, its interactive-terminal
// capability and the backend label, mirroring the gate chain of
// fileStore/shellExecutor.
func (s *SandboxWorkbenchService) terminalProvider(
	ctx context.Context, sessionID string,
) (sandbox.SessionTerminalProvider, *types.Session, sandbox.SandboxType, error) {
	mgr, session, err := s.ownedManager(ctx, sessionID)
	if err != nil {
		return nil, session, "", err
	}
	provider, ok := mgr.(sandbox.SessionTerminalCapabilityProvider)
	if !ok || provider.SessionTerminalProvider() == nil {
		return nil, session, "", ErrSandboxWorkbenchUnsupported
	}
	return provider.SessionTerminalProvider(), session, mgr.GetType(), nil
}

// OpenTerminal opens one interactive PTY inside the session's sandbox under a
// hard lease. The lease context handed to the provider is the lifetime
// guarantee: when it expires the PTY dies and the browser's output pump sees
// EOF, so no orphan shell can outlive the workbench.
func (s *SandboxWorkbenchService) OpenTerminal(
	ctx context.Context, sessionID string, cols, rows uint16,
) (*WorkbenchTerminal, error) {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	sessionID = strings.TrimSpace(sessionID)

	s.terminalsMu.Lock()
	if s.terminalCounts[sessionID] >= SandboxWorkbenchMaxTerminalsPerSession {
		s.terminalsMu.Unlock()
		return nil, ErrSandboxWorkbenchTerminalLimit
	}
	s.terminalsMu.Unlock()

	leaseCtx, cancelLease := context.WithTimeout(ctx, SandboxWorkbenchTerminalLease)
	provider, session, backend, err := s.terminalProvider(leaseCtx, sessionID)
	if err != nil {
		cancelLease()
		return nil, err
	}
	terminalSession, err := provider.OpenSessionTerminal(leaseCtx, sessionID, sandbox.SessionTerminalOptions{
		WorkDir: sandbox.SessionWorkspaceRoot,
		Env: []string{
			"PROMPT_COMMAND=" + workbenchTerminalAuditPromptCommand,
			"HISTCONTROL=",
			"HISTIGNORE=",
			"HISTFILE=/dev/null",
		},
		Cols: cols,
		Rows: rows,
	})
	if err != nil {
		cancelLease()
		return nil, err
	}

	s.terminalsMu.Lock()
	// Re-check under the lock: two concurrent opens could otherwise both pass
	// the pre-check and exceed the cap.
	if s.terminalCounts[sessionID] >= SandboxWorkbenchMaxTerminalsPerSession {
		s.terminalsMu.Unlock()
		_ = terminalSession.Close()
		cancelLease()
		return nil, ErrSandboxWorkbenchTerminalLimit
	}
	s.terminalCounts[sessionID]++
	s.terminalsMu.Unlock()

	terminal := &WorkbenchTerminal{
		ID:      newTerminalID(),
		Backend: string(backend),
		Session: terminalSession,
		Context: leaseCtx,
	}
	terminal.closeFunc = func(reason string, exitCode int) {
		cleanupErr := terminalSession.Close()
		cancelLease()
		s.terminalsMu.Lock()
		if s.terminalCounts[sessionID] > 0 {
			s.terminalCounts[sessionID]--
		}
		if s.terminalCounts[sessionID] == 0 {
			delete(s.terminalCounts, sessionID)
		}
		s.terminalsMu.Unlock()
		// Disconnection/expiry has often cancelled ctx already. Keep actor and
		// tenant values but give persistence an independent bounded deadline.
		auditCtx, cancelAudit := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelAudit()
		details := map[string]any{
			"reason":    reason,
			"exit_code": exitCode,
		}
		if cleanupErr != nil {
			details["cleanup_error"] = cleanupErr.Error()
		}
		s.auditTerminalEvent(auditCtx, session, "sandbox.terminal_closed", terminal.ID, details)
	}
	s.auditTerminalEvent(leaseCtx, session, "sandbox.terminal_opened", terminal.ID, map[string]any{
		"backend":       terminal.Backend,
		"cols":          cols,
		"rows":          rows,
		"lease_minutes": int(SandboxWorkbenchTerminalLease.Minutes()),
	})

	go s.keepTerminalAlive(leaseCtx, sessionID, terminal.ID)
	go func() {
		<-leaseCtx.Done()
		reason := "context_cancelled"
		if errors.Is(leaseCtx.Err(), context.DeadlineExceeded) {
			reason = "lease_expired"
		}
		terminal.Close(reason, -1)
	}()
	return terminal, nil
}

// keepTerminalAlive periodically runs a wrapper exec so the docker idle sweep
// keeps seeing fresh activity while a terminal is open. The lease context
// stops the loop when the terminal's lifetime ends; the first failed resolve
// (session deleted, backend gone) stops it early.
func (s *SandboxWorkbenchService) keepTerminalAlive(ctx context.Context, sessionID, terminalID string) {
	ticker := time.NewTicker(SandboxWorkbenchTerminalKeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		executor, _, err := s.shellExecutor(ctx, sessionID)
		if err != nil {
			return
		}
		// The wrapper command touches the idle-sweep marker; failures here are
		// cosmetic and retried on the next tick.
		_, _ = executor.ExecShellCommand(
			ctx, sessionID, "true", sandbox.SessionWorkspaceRoot, 10*time.Second, nil,
		)
	}
}

// AuditTerminalInput records one interactive command line. The PTY stream
// itself is opaque, so the WebSocket pump reconstructs what the user typed
// and reports completed lines here; that keeps the per-command audit promise
// for interactive terminals too. Ctrl-C submissions arrive as interrupted
// lines and are stored the same way, marked with interrupted=true.
func (s *SandboxWorkbenchService) AuditTerminalInput(
	ctx context.Context, sessionID, terminalID, line string, interrupted bool,
) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" && !interrupted {
		return
	}
	_, session, err := s.ownedManager(ctx, sessionID)
	if err != nil || session == nil {
		return
	}
	command := line
	if interrupted {
		if command == "" {
			command = "^C"
		} else {
			command += "^C"
		}
	}
	s.auditTerminalEvent(ctx, session, "sandbox.terminal_command", terminalID, map[string]any{
		"command":     command,
		"interrupted": interrupted,
		"source":      "interactive",
	})
}

// AuditTerminalCommand records a command reported after bash executed it. In
// contrast to AuditTerminalInput this is execution-layer evidence: command is
// the committed readline/history value and exitCode is the resulting status.
func (s *SandboxWorkbenchService) AuditTerminalCommand(
	ctx context.Context, sessionID, terminalID, historyID, command string, exitCode int,
) {
	command = strings.TrimRight(command, "\r\n")
	if command == "" {
		return
	}
	_, session, err := s.ownedManager(ctx, sessionID)
	if err != nil || session == nil {
		return
	}
	s.auditTerminalEvent(ctx, session, "sandbox.terminal_command", terminalID, map[string]any{
		"command":     command,
		"exit_code":   exitCode,
		"history_id":  historyID,
		"interrupted": false,
		"source":      "shell",
	})
}

func (s *SandboxWorkbenchService) auditTerminalEvent(
	ctx context.Context, session *types.Session, action, terminalID string, details map[string]any,
) {
	if s == nil || s.audit == nil || session == nil {
		return
	}
	detailsJSON, _ := json.Marshal(details)
	actor, _ := types.UserIDFromContext(ctx)
	outcome := types.AuditOutcomeSuccess
	if action == "sandbox.terminal_closed" {
		if code, ok := details["exit_code"].(int); ok && code != 0 {
			outcome = types.AuditOutcomeFailed
		}
		if details["cleanup_error"] != nil {
			outcome = types.AuditOutcomeFailed
		}
	}
	if action == "sandbox.terminal_command" {
		if code, ok := details["exit_code"].(int); ok && code != 0 {
			outcome = types.AuditOutcomeFailed
		}
	}
	_ = s.audit.Log(ctx, &types.AuditLog{
		TenantID: session.TenantID, ActorUserID: actor,
		ActorRole: string(types.TenantRoleFromContext(ctx)),
		Action:    types.AuditAction(action),
		ScopeType: "session", ScopeID: session.ID,
		TargetType: "sandbox_terminal", TargetID: terminalID,
		Outcome: outcome, Details: types.JSON(detailsJSON),
	})
}
