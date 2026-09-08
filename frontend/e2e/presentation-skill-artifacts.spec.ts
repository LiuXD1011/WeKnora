import { readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Route } from '@playwright/test'

const evidenceDir = process.env.PRESENTATION_EVIDENCE_DIR || ''

async function json(route: Route, body: unknown) {
  await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
}

test('real presentation Skill artifacts preview and download in the workbench', async ({ page }) => {
  test.skip(!evidenceDir, 'set PRESENTATION_EVIDENCE_DIR to run the real artifact acceptance test')
  expect(evidenceDir, 'PRESENTATION_EVIDENCE_DIR is required').not.toBe('')
  const pptxPath = join(evidenceDir, 'weknora-sandbox-demo.pptx')
  const htmlPath = join(evidenceDir, 'weknora-sandbox-demo.html')
  const pptx = readFileSync(pptxPath)
  const html = readFileSync(htmlPath)

  await page.routeWebSocket(/\/sandbox\/terminal\/ws/, server => {
    server.onMessage(() => {})
    server.send(JSON.stringify({ type: 'ready', terminal_id: 'presentation-evidence', backend: 'docker' }))
  })
  await page.route('**/api/v1/sessions/*/sandbox/workbench**', route => json(route, {
    success: true,
    data: { backend: 'docker', artifact_root: '/workspace/output', terminal: true, files: true, interactive: true },
  }))
  await page.route('**/api/v1/sessions/*/sandbox/files?**', route => json(route, {
    success: true,
    data: [
      { name: 'weknora-sandbox-demo.pptx', path: 'weknora-sandbox-demo.pptx', type: 'file', size: statSync(pptxPath).size, mod_time: '2026-09-08T07:00:00Z', artifact_type: 'presentation', preview_format: 'pptx', media_type: 'application/vnd.openxmlformats-officedocument.presentationml.presentation', classification_source: 'producer' },
      { name: 'weknora-sandbox-demo.html', path: 'weknora-sandbox-demo.html', type: 'file', size: statSync(htmlPath).size, mod_time: '2026-09-08T07:00:00Z', artifact_type: 'webpage', preview_format: 'html', media_type: 'text/html', classification_source: 'producer' },
    ],
  }))
  await page.route('**/api/v1/sessions/*/sandbox/files/content?**', async route => {
    const path = new URL(route.request().url()).searchParams.get('path') || ''
    if (path.endsWith('.pptx')) {
      await route.fulfill({ status: 200, contentType: 'application/vnd.openxmlformats-officedocument.presentationml.presentation', body: pptx })
    } else {
      await route.fulfill({ status: 200, contentType: 'text/html; charset=utf-8', body: html })
    }
  })

  await page.goto('/e2e/workbench?session=presentation-evidence')
  await expect(page.getByText('可视化沙箱工作台')).toBeVisible()
  await page.locator('.t-tabs__nav-item').filter({ hasText: '文件' }).click()

  const htmlRow = page.locator('.workbench-file-row').filter({ hasText: 'weknora-sandbox-demo.html' })
  await expect(htmlRow.locator('.artifact-type-tag')).toHaveText('网页')
  await htmlRow.locator('.file-main').click()
  const frame = page.frameLocator('iframe.preview-frame')
  await expect(frame.getByRole('heading', { name: 'WeKnora 沙盒工作台实战' })).toBeVisible()
  await frame.locator('body').evaluate(body => {
    body.ownerDocument.defaultView?.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight' }))
  })
  await expect(frame.getByText('统一工作台')).toBeVisible()

  await page.locator('.t-tabs__nav-item').filter({ hasText: '文件' }).click()
  const pptxRow = page.locator('.workbench-file-row').filter({ hasText: 'weknora-sandbox-demo.pptx' })
  await expect(pptxRow.locator('.artifact-type-tag')).toHaveText('演示文稿')
  await pptxRow.locator('.file-main').click()
  await expect(page.locator('.office-preview')).toBeVisible()
  await expect(page.locator('.office-preview')).toContainText('WeKnora 沙盒工作台实战', { timeout: 30_000 })
  await page.locator('.sandbox-workbench-drawer .t-drawer__content-wrapper').screenshot({
    path: 'e2e-artifacts/presentation-skill-real.png',
  })

  const downloadPromise = page.waitForEvent('download')
  await page.locator('.preview-toolbar').getByRole('button', { name: '下载' }).click()
  const download = await downloadPromise
  expect(download.suggestedFilename()).toBe('weknora-sandbox-demo.pptx')
})
