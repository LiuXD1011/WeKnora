import { chromium } from '@playwright/test'

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: 920, height: 950 } })
page.on('console', msg => {
  if (msg.type() === 'error') console.log('[console.error]', msg.text().slice(0, 300))
})
page.on('pageerror', err => console.log('[pageerror]', String(err).slice(0, 400)))

await page.route('**/api/v1/sessions/*/sandbox/workbench**', route =>
  route.fulfill({
    status: 200, contentType: 'application/json',
    body: JSON.stringify({ success: true, data: { backend: 'docker', artifact_root: '/workspace/output', terminal: true, files: true, interactive: true } }),
  }))

await page.routeWebSocket(/\/sandbox\/terminal\/ws/, server => {
  server.onMessage(message => {
    if (typeof message !== 'string') {
      server.send(message)
    }
  })
  server.send(JSON.stringify({ type: 'ready', terminal_id: 'term-probe', backend: 'docker' }))
  server.send(Buffer.from('user@workspace:/workspace/output$ ', 'utf-8'))
})

await page.goto('http://localhost:8123/e2e/workbench?session=e2e-interactive', { waitUntil: 'networkidle' })
await page.waitForTimeout(2500)

const info = await page.evaluate(() => {
  const xterm = document.querySelector('.xterm')
  const rows = document.querySelector('.xterm-rows')
  const textarea = document.querySelector('.xterm-helper-textarea')
  return {
    xterm: !!xterm,
    rowsText: rows ? rows.textContent?.slice(0, 200) : null,
    rowsChildren: rows ? rows.children.length : -1,
    textareaW: textarea ? getComputedStyle(textarea).width : null,
    cssLoaded: [...document.styleSheets].some(sheet => {
      try { return [...sheet.cssRules].some(r => r.cssText?.includes('.xterm-rows')) } catch { return false }
    }),
  }
})
console.log(JSON.stringify(info, null, 2))
await page.screenshot({ path: 'e2e-artifacts/probe.png' })
await browser.close()
