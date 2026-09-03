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
  { name: 'summary.md', path: 'summary.md', type: 'file', size: 1_024, mod_time: '2026-09-02T10:09:00Z' },
  { name: 'chart.png', path: 'chart.png', type: 'file', size: 86_400, mod_time: '2026-09-02T10:10:00Z' },
]

const FILE_ENTRIES_REPORTS = [
  { name: 'a.txt', path: 'reports/a.txt', type: 'file', size: 24, mod_time: '2026-09-02T10:08:00Z' },
]

const HTML_ARTIFACT = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>课题汇报</title>
<style>body{margin:0;background:#0f2440;color:#fff;font-family:"Microsoft YaHei",sans-serif}
.slide{box-sizing:border-box;min-height:100vh;padding:9vh 8vw;display:none;background:linear-gradient(135deg,#0f2440,#183a5a)}
.slide.active{display:block}h1{font-size:40px;border-left:8px solid #07c05f;padding-left:20px}
li{font-size:24px;line-height:1.8}.num{position:fixed;right:3vw;bottom:3vh;color:#a9bdca}</style></head><body>
<div class="slide active"><h1>可视化沙箱工作台</h1><ul><li>交互式终端 · 实时可观测</li><li>产物在线预览 · 一键下载</li></ul><span class="num">1/3</span></div>
<div class="slide"><h1>架构要点</h1><ul><li>会话级沙箱能力协商</li><li>双防线路径安全策略</li><li>全链路命令审计</li></ul><span class="num">2/3</span></div>
<div class="slide"><h1>演示链路</h1><ul><li>Agent 生成 PPTX</li><li>工作台在线预览与下载</li></ul><span class="num">3/3</span></div>
<script>const s=[...document.querySelectorAll('.slide')];let c=0;function show(n){c=(n+s.length)%s.length;s.forEach((x,i)=>x.classList.toggle('active',i===c))}addEventListener('keydown',e=>{if(e.key==='ArrowRight')show(c+1);if(e.key==='ArrowLeft')show(c-1)});</script>
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

/**
 * Silently satisfies the interactive terminal handshake for scenarios whose
 * screenshots focus on other tabs, so no connection-error toast appears.
 */
async function mockTerminalReady(page: Page) {
  await page.routeWebSocket(/\/sandbox\/terminal\/ws/, server => {
    server.onMessage(() => {})
    server.send(JSON.stringify({ type: 'ready', terminal_id: 'term-e2e-bg', backend: 'docker' }))
  })
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

    const PROMPT = 'user@workspace:/workspace/output$ '
    // CRLF built without escapes: the PTY contract is CRLF line endings.
    const CRLF = String.fromCharCode(13, 10)
    const banner = [
      'WeKnora sandbox terminal (docker)',
      'Workspace: /workspace  Account: user',
    ].join(CRLF) + CRLF + PROMPT
    const listing = [
      'total 12',
      'drwxr-xr-x 2 user user   4096 Sep  2 10:08 reports',
      '-rw-r--r-- 1 user user 482013 Sep  2 10:06 deck.pptx',
      '-rw-r--r-- 1 user user    128 Sep  2 10:07 sales.csv',
    ].join(CRLF) + CRLF + PROMPT
    await page.routeWebSocket(/\/sandbox\/terminal\/ws/, server => {
      server.onMessage(message => {
        received.push(message)
        if (typeof message === 'string') {
          const event = JSON.parse(message)
          if (event.type === 'resize') resizeFrames += 1
          return
        }
        // Behave like a real PTY: echo the keystrokes, and answer a completed
        // line with listing output plus a fresh prompt.
        const text = message.toString()
        server.send(Buffer.from(text, 'utf-8'))
        if (text.endsWith(String.fromCharCode(13))) {
          server.send(Buffer.from(listing, 'utf-8'))
        }
      })
      // The server-side lifecycle: banner + prompt, then the browser types.
      server.send(JSON.stringify({ type: 'ready', terminal_id: 'term-e2e-1', backend: 'docker' }))
      server.send(Buffer.from(banner, 'utf-8'))
    })

    await mockWorkbenchApi(page)
    await openWorkbench(page, 'e2e-interactive')

    // Terminal connects and announces itself.
    await expect(page.getByText(/已连接/)).toBeVisible({ timeout: 15_000 })
    await expect(page.getByText('term-e2e-1')).toBeVisible()

    // Focus the terminal and type a command; xterm ships it as binary frames
    // and the fake PTY echoes prompt + listing back.
    const host = page.locator('[data-interactive-terminal]')
    await host.click()
    await page.keyboard.type('ls -la')
    await page.keyboard.press('Enter')

    await expect
      .poll(() => received.filter(item => typeof item !== 'string').length, { timeout: 10_000 })
      .toBeGreaterThan(0)
    const typed = received
      .filter(item => typeof item !== 'string')
      .map(item => (item as Buffer).toString())
      .join('')
    expect(typed).toContain('ls -la')

    // The FitAddon fires an initial resize once the socket is open.
    expect(resizeFrames).toBeGreaterThanOrEqual(1)

    await page.screenshot({ path: `${SHOT_DIR}/terminal-interactive.png` })

    // Sanity: capability payload reached the drawer (backend badge).
    await expect(page.getByText('docker', { exact: true })).toBeVisible()
  })

  test('interactive terminal exit event is surfaced with its reason', async ({ page }) => {
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
    await mockTerminalReady(page)
    await openWorkbench(page, 'e2e-interactive')

    await page.locator('.t-tabs__nav-item').filter({ hasText: '文件' }).click()
    await expect(page.getByText('产物目录 · 6 项')).toBeVisible()
    await expect(page.getByText('presentation.html')).toBeVisible()
    await expect(page.getByText('deck.pptx')).toBeVisible()

    // Enter the reports subdirectory: the listing request carries the path.
    await page.getByText('reports/').click()
    await expect(page.getByText('reports/a.txt')).toBeVisible()
    expect(api.listRequests).toContain('reports')

    // Breadcrumbs: the root crumb is labelled output.
    await expect(page.getByText('产物目录 · 1 项')).toBeVisible()
    await page.getByRole('button', { name: 'output', exact: true }).click()
    await expect(page.getByText('产物目录 · 6 项')).toBeVisible()
    expect(api.listRequests.filter(path => path === '').length).toBeGreaterThanOrEqual(2)

    await page.screenshot({ path: `${SHOT_DIR}/files-navigation.png` })
  })

  test('html artifact previews inside a sandboxed iframe without same-origin', async ({ page }) => {
    const api = await mockWorkbenchApi(page)
    await mockTerminalReady(page)
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
    await mockTerminalReady(page)
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
