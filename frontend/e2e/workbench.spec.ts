import { expect, test, type Page, type Route } from '@playwright/test'

/**
 * Sandbox workbench E2E.
 *
 * Scenario switch: the harness page (/e2e/workbench?session=…) picks a fake
 * session id; the intercepted /workbench endpoint answers with the capability
 * set that scenario represents:
 *   e2e-interactive — docker backend with a live PTY (interactive terminal)
 *   e2e-exec        — cube backend without PTY (degraded command mode)
 *   e2e-none        — no live sandbox at all (empty state)
 */

const SHOT_DIR = 'e2e-artifacts'

const FILE_ENTRIES_ROOT = [
  { name: 'reports', path: 'reports', type: 'dir', size: 0, mod_time: '2026-09-02T10:00:00Z' },
  {
    name: 'presentation.html',
    path: 'presentation.html',
    type: 'file',
    size: 2048,
    mod_time: '2026-09-02T10:05:00Z',
  },
  { name: 'deck.pptx', path: 'deck.pptx', type: 'file', size: 482_013, mod_time: '2026-09-02T10:06:00Z' },
  { name: 'sales.csv', path: 'sales.csv', type: 'file', size: 128, mod_time: '2026-09-02T10:07:00Z' },
]

const FILE_ENTRIES_REPORTS = [
  { name: 'a.txt', path: 'reports/a.txt', type: 'file', size: 24, mod_time: '2026-09-02T10:08:00Z' },
]

const HTML_ARTIFACT = `<!doctype html><html><body>
<h1>sandbox artifact</h1>
<script>try { document.cookie = 'probe=1' } catch (e) {}</script>
</body></html>`

function workbenchCapabilities(sessionId: string) {
  if (sessionId === 'e2e-interactive') {
    return { backend: 'docker', artifact_root: '/workspace/output', terminal: true, files: true, interactive: true }
  }
  if (sessionId === 'e2e-exec') {
    return { backend: 'cube', artifact_root: '/workspace/output', terminal: true, files: true, interactive: false }
  }
  return null
}

async function fulfillJson(route: Route, status: number, body: unknown) {
  await route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
}

/** Installs HTTP intercepts for every endpoint the drawer touches. */
async function mockWorkbenchApi(page: Page) {
  const listRequests: string[] = []
  const contentRequests: string[] = []
  const execRequests: string[] = []

  await page.route('**/api/v1/sessions/*/sandbox/workbench**', async route => {
    const sessionId = decodeURIComponent(route.request().url().match(/sessions\/([^/]+)\/sandbox/)![1])
    const capabilities = workbenchCapabilities(sessionId)
    if (!capabilities) {
      await fulfillJson(route, 409, { success: false, error: 'session has no live sandbox' })
      return
    }
    await fulfillJson(route, 200, { success: true, data: capabilities })
  })

  await page.route('**/api/v1/sessions/*/sandbox/files?**', async route => {
    const url = new URL(route.request().url())
    const path = url.searchParams.get('path') || ''
    listRequests.push(path)
    const entries = path === 'reports' ? FILE_ENTRIES_REPORTS : FILE_ENTRIES_ROOT
    await fulfillJson(route, 200, { success: true, data: entries })
  })

  await page.route('**/api/v1/sessions/*/sandbox/files/content?**', async route => {
    const url = new URL(route.request().url())
    const path = url.searchParams.get('path') || ''
    contentRequests.push(path)
    if (path.endsWith('.html')) {
      await route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: HTML_ARTIFACT })
      return
    }
    if (path.endsWith('.csv')) {
      await route.fulfill({ status: 200, contentType: 'text/csv', body: 'region,amount\nnorth,42\nsouth,17' })
      return
    }
    await route.fulfill({ status: 200, contentType: 'application/octet-stream', body: 'binary' })
  })

  await page.route('**/api/v1/sessions/*/sandbox/terminal/exec', async route => {
    const body = route.request().postDataJSON() as { command?: string }
    execRequests.push(body.command || '')
    await fulfillJson(route, 200, {
      success: true,
      data: {
        stdout: `ran: ${body.command}\nreport.csv\n`,
        stderr: '',
        exit_code: 0,
        duration_ms: 12,
        killed: false,
      },
    })
  })

  return { listRequests, contentRequests, execRequests }
}

/** Opens the harness page for a scenario and waits for the drawer. */
async function openWorkbench(page: Page, scenario: string) {
  await page.goto(`/e2e/workbench?session=${scenario}`)
  await expect(page.getByText('可视化沙箱工作台')).toBeVisible()
}

test.describe('sandbox workbench drawer', () => {
  test('interactive PTY terminal: ready, keystroke stream, exit reason', async ({ page }) => {
    const received: (string | Buffer)[] = []
    let resizeFrames = 0

    await page.routeWebSocket(/\/sandbox\/terminal\/ws/, server => {
      server.onMessage(message => {
        received.push(message)
        if (typeof message === 'string') {
          const event = JSON.parse(message)
          if (event.type === 'resize') resizeFrames += 1
        }
      })
      // The server-side lifecycle: ready → (browser types) → exit.
      server.send(JSON.stringify({ type: 'ready', terminal_id: 'term-e2e-1', backend: 'docker' }))
    })

    await mockWorkbenchApi(page)
    await openWorkbench(page, 'e2e-interactive')

    // Terminal connects and announces itself.
    await expect(page.getByText(/已连接/)).toBeVisible({ timeout: 15_000 })
    await expect(page.getByText('term-e2e-1')).toBeVisible()

    // Focus the terminal and type a command; xterm ships it as binary frames.
    const host = page.locator('[data-interactive-terminal]')
    await host.click()
    await page.keyboard.type('echo hello-workbench')
    await page.keyboard.press('Enter')

    await expect
      .poll(() => received.filter(item => typeof item !== 'string').length, { timeout: 10_000 })
      .toBeGreaterThan(0)
    const typed = received
      .filter(item => typeof item !== 'string')
      .map(item => (item as Buffer).toString())
      .join('')
    expect(typed).toContain('echo hello-workbench')

    // The FitAddon fires an initial resize once the socket is open.
    expect(resizeFrames).toBeGreaterThanOrEqual(1)

    await page.screenshot({ path: `${SHOT_DIR}/terminal-interactive.png` })

    // Sanity: capability payload reached the drawer (backend badge).
    await expect(page.getByText('docker', { exact: true })).toBeVisible()
  })

  test('interactive terminal exit event is surfaced with its reason', async ({ page }) => {
    test.info().annotations.push({ description: 'Verifies the exit protocol event drives the status bar.' })
    await page.routeWebSocket(/\/sandbox\/terminal\/ws/, server => {
      server.onMessage(() => {})
      server.send(JSON.stringify({ type: 'ready', terminal_id: 'term-e2e-2', backend: 'docker' }))
      setTimeout(() => {
        server.send(JSON.stringify({ type: 'exit', code: 0, reason: 'lease_expired' }))
      }, 500)
    })
    await mockWorkbenchApi(page)
    await openWorkbench(page, 'e2e-interactive')

    const statusBadge = page.locator('.status-badge.status-ended')
    await expect(statusBadge).toBeVisible({ timeout: 15_000 })
    await expect(statusBadge).toContainText('达到时长上限')
    await expect(statusBadge).toContainText('退出码 0')
    await page.screenshot({ path: `${SHOT_DIR}/terminal-exit.png` })
  })

  test('degrades to command mode when the backend has no PTY', async ({ page }) => {
    const api = await mockWorkbenchApi(page)
    await openWorkbench(page, 'e2e-exec')

    // Downgrade hint instead of an interactive shell.
    await expect(page.getByText(/不支持交互式终端/)).toBeVisible()
    await expect(page.getByText('cube', { exact: true })).toBeVisible()

    // Command mode still executes and renders aggregated output.
    await page.locator('.terminal-command input').fill('ls -la /workspace/output')
    await page.getByRole('button', { name: '运行' }).click()
    await expect(page.getByText('ran: ls -la /workspace/output')).toBeVisible()
    await expect(page.getByText('[退出码 0 · 12 ms]')).toBeVisible()
    expect(api.execRequests).toContain('ls -la /workspace/output')

    await page.screenshot({ path: `${SHOT_DIR}/terminal-degraded.png` })
  })

  test('file manager lists, navigates directories and returns via breadcrumb', async ({ page }) => {
    const api = await mockWorkbenchApi(page)
    await openWorkbench(page, 'e2e-interactive')

    await page.locator('.t-tabs__nav-item').filter({ hasText: '文件' }).click()
    await expect(page.getByText('产物目录 · 4 项')).toBeVisible()
    await expect(page.getByText('presentation.html')).toBeVisible()
    await expect(page.getByText('deck.pptx')).toBeVisible()

    // Enter the reports subdirectory: the listing request carries the path.
    await page.getByText('reports/').click()
    await expect(page.getByText('reports/a.txt')).toBeVisible()
    expect(api.listRequests).toContain('reports')

    // Breadcrumbs: the root crumb is labelled output.
    await expect(page.getByText('产物目录 · 1 项')).toBeVisible()
    await page.getByRole('button', { name: 'output', exact: true }).click()
    await expect(page.getByText('产物目录 · 4 项')).toBeVisible()
    expect(api.listRequests.filter(path => path === '').length).toBeGreaterThanOrEqual(2)

    await page.screenshot({ path: `${SHOT_DIR}/files-navigation.png` })
  })

  test('html artifact previews inside a sandboxed iframe without same-origin', async ({ page }) => {
    const api = await mockWorkbenchApi(page)
    await openWorkbench(page, 'e2e-interactive')

    await page.locator('.t-tabs__nav-item').filter({ hasText: '文件' }).click()
    await page.getByText('presentation.html').click()

    // The preview tab opens and the artifact content is fetched once. The
    // mock answers instantly, so assert the finished state, not the loader.
    await expect(page.locator('iframe.preview-frame')).toHaveCount(1)
    expect(api.contentRequests).toContain('presentation.html')

    // Isolation contract: no allow-same-origin on the preview iframe.
    const sandboxAttr = await page.locator('iframe.preview-frame').getAttribute('sandbox')
    expect(sandboxAttr).toContain('allow-scripts')
    expect(sandboxAttr).not.toContain('allow-same-origin')

    await page.screenshot({ path: `${SHOT_DIR}/preview-html.png` })
  })

  test('csv artifact renders as a read-only sheet view', async ({ page }) => {
    const api = await mockWorkbenchApi(page)
    await openWorkbench(page, 'e2e-interactive')

    await page.locator('.t-tabs__nav-item').filter({ hasText: '文件' }).click()
    await page.getByText('sales.csv').click()

    await expect(page.locator('.sheet-table')).toBeVisible()
    await expect(page.locator('.sheet-table').getByText('north')).toBeVisible()
    await expect(page.locator('.sheet-table').getByText('42')).toBeVisible()
    expect(api.contentRequests).toContain('sales.csv')

    await page.screenshot({ path: `${SHOT_DIR}/preview-csv.png` })
  })

  test('shows the empty state when the session has no live sandbox', async ({ page }) => {
    await mockWorkbenchApi(page)
    await openWorkbench(page, 'e2e-none')

    await expect(page.getByText('当前会话还没有可用沙箱')).toBeVisible()
    await page.screenshot({ path: `${SHOT_DIR}/empty-state.png` })
  })
})
