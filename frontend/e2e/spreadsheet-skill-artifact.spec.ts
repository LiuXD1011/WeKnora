import { readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { expect, test, type Route } from '@playwright/test'

const evidenceDir = process.env.SPREADSHEET_EVIDENCE_DIR || ''

async function json(route: Route, body: unknown) {
  await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
}

test('real spreadsheet Skill artifact previews and downloads in the workbench', async ({ page }) => {
  test.skip(!evidenceDir, 'set SPREADSHEET_EVIDENCE_DIR to run the real artifact acceptance test')
  expect(evidenceDir, 'SPREADSHEET_EVIDENCE_DIR is required').not.toBe('')
  const xlsxPath = join(evidenceDir, 'weknora-sandbox-acceptance.xlsx')
  const xlsx = readFileSync(xlsxPath)

  await page.routeWebSocket(/\/sandbox\/terminal\/ws/, server => {
    server.onMessage(() => {})
    server.send(JSON.stringify({ type: 'ready', terminal_id: 'spreadsheet-evidence', backend: 'docker' }))
  })
  await page.route('**/api/v1/sessions/*/sandbox/workbench**', route => json(route, {
    success: true,
    data: { backend: 'docker', artifact_root: '/workspace/output', terminal: true, files: true, interactive: true },
  }))
  await page.route('**/api/v1/sessions/*/sandbox/files?**', route => json(route, {
    success: true,
    data: [{
      name: 'weknora-sandbox-acceptance.xlsx', path: 'weknora-sandbox-acceptance.xlsx', type: 'file',
      size: statSync(xlsxPath).size, mod_time: '2026-09-08T08:00:00Z',
      artifact_type: 'spreadsheet', preview_format: 'spreadsheet',
      media_type: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet', classification_source: 'producer',
    }],
  }))
  await page.route('**/api/v1/sessions/*/sandbox/files/content?**', route => route.fulfill({
    status: 200,
    contentType: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
    body: xlsx,
  }))

  await page.goto('/e2e/workbench?session=spreadsheet-evidence')
  await expect(page.getByText('可视化沙箱工作台')).toBeVisible()
  await page.locator('.t-tabs__nav-item').filter({ hasText: '文件' }).click()
  const row = page.locator('.workbench-file-row').filter({ hasText: 'weknora-sandbox-acceptance.xlsx' })
  await expect(row.locator('.artifact-type-tag')).toHaveText('表格')
  await row.locator('.file-main').click()
  await expect(page.locator('.sheet-table')).toBeVisible()
  await expect(page.locator('.sheet-table')).toContainText('WeKnora 沙盒验收表')
  await expect(page.locator('.sheet-table')).toContainText('电子表格预览')
  await expect(page.locator('.sheet-table')).toContainText('PPTX 与 HTML 可预览下载')
  await page.locator('.sandbox-workbench-drawer .t-drawer__content-wrapper').screenshot({
    path: 'e2e-artifacts/spreadsheet-skill-real.png',
  })

  const downloadPromise = page.waitForEvent('download')
  await page.locator('.preview-toolbar').getByRole('button', { name: '下载' }).click()
  const download = await downloadPromise
  expect(download.suggestedFilename()).toBe('weknora-sandbox-acceptance.xlsx')
})
