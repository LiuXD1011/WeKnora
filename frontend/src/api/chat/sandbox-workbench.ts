import { del, get, getDown, post, postUpload } from '@/utils/request'
import { getApiBaseUrl } from '@/utils/api-base'

export type SandboxWorkbenchInfo = {
  backend: string
  artifact_root: string
  terminal: boolean
  files: boolean
  /** True when the backend offers a real PTY over the WebSocket terminal. */
  interactive: boolean
}

export type SandboxWorkbenchFile = {
  name: string
  path: string
  type: 'file' | 'dir' | 'other'
  size: number
  mod_time: string
}

export type SandboxCommandResult = {
  stdout: string
  stderr: string
  exit_code: number
  duration_ms: number
  killed: boolean
  error?: string
}

const root = (sessionId: string) => `/api/v1/sessions/${encodeURIComponent(sessionId)}/sandbox`

export function getSandboxWorkbenchInfo(sessionId: string) {
  return get<{ success: boolean; data: SandboxWorkbenchInfo }>(`${root(sessionId)}/workbench`)
}

export function listSandboxWorkbenchFiles(sessionId: string, path = '') {
  return get<{ success: boolean; data: SandboxWorkbenchFile[] }>(
    `${root(sessionId)}/files?path=${encodeURIComponent(path)}`,
  )
}

export function downloadSandboxWorkbenchFile(sessionId: string, path: string): Promise<Blob> {
  return getDown(`${root(sessionId)}/files/content?path=${encodeURIComponent(path)}`)
}

export function uploadSandboxWorkbenchFile(sessionId: string, path: string, file: File) {
  const form = new FormData()
  form.append('path', path)
  form.append('file', file)
  return postUpload(`${root(sessionId)}/files`, form)
}

export function renameSandboxWorkbenchFile(sessionId: string, oldPath: string, newPath: string) {
  return post(`${root(sessionId)}/files/rename`, { old_path: oldPath, new_path: newPath })
}

export function deleteSandboxWorkbenchFile(sessionId: string, path: string) {
  return del(`${root(sessionId)}/files?path=${encodeURIComponent(path)}`)
}

export function executeSandboxWorkbenchCommand(
  sessionId: string,
  command: string,
  workDir = '',
  timeoutSeconds = 60,
  signal?: AbortSignal,
) {
  return post<{ success: boolean; data: SandboxCommandResult }>(
    `${root(sessionId)}/terminal/exec`,
    { command, work_dir: workDir, timeout_seconds: timeoutSeconds },
    { signal, timeout: 0 },
  )
}

/**
 * Opens the interactive PTY terminal for a session.
 *
 * Browsers cannot set custom headers on a WebSocket handshake, so the JWT
 * travels as a "bearer.<token>" WebSocket sub-protocol and the active
 * workspace as a tenant_id query parameter; the auth middleware promotes both
 * back into the regular header set before authentication runs. The token
 * never appears in the URL, keeping it out of access logs and history.
 */
export function openSandboxWorkbenchTerminal(
  sessionId: string,
  cols: number,
  rows: number,
): { socket: WebSocket; terminalUrl: string } {
  const token = localStorage.getItem('weknora_token') || ''
  const tenantId = localStorage.getItem('weknora_selected_tenant_id') || ''
  const base = getApiBaseUrl().replace(/^http/, 'ws').replace(/\/$/, '')
  const query = new URLSearchParams({
    cols: String(cols),
    rows: String(rows),
  })
  if (tenantId) query.set('tenant_id', tenantId)
  const terminalUrl = `${base}${root(sessionId)}/terminal/ws?${query.toString()}`
  const protocols = token ? [`bearer.${token}`] : []
  return { socket: new WebSocket(terminalUrl, protocols), terminalUrl }
}
