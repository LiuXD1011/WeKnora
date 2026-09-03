import { chromium } from '@playwright/test'

const browser = await chromium.launch()
const page = await browser.newPage()
page.on('console', msg => {
  if (msg.type() === 'error' || msg.type() === 'warning') console.log('[console]', msg.type(), msg.text().slice(0, 300))
})
page.on('pageerror', err => console.log('[pageerror]', String(err).slice(0, 500)))
await page.goto('http://localhost:8123/e2e/workbench?session=e2e-interactive', { waitUntil: 'networkidle' })
console.log('final URL:', page.url())
console.log('title:', await page.title())
const body = await page.locator('body').innerText()
console.log('body head:', body.slice(0, 400).replace(/\n/g, ' | '))
await browser.close()
