import { defineConfig } from '@playwright/test'

/**
 * E2E tests for the sandbox workbench drawer.
 *
 * The suite drives the real production component (SandboxWorkbenchDrawer,
 * SandboxTerminal, xterm.js) against Playwright browser-level network
 * interception: HTTP calls are fulfilled by page.route handlers and the PTY
 * WebSocket is simulated with page.routeWebSocket. No backend, database or
 * Docker daemon is required, so it runs anywhere Chromium runs.
 *
 * The suite needs the vite dev server (the /e2e/workbench harness route is
 * registered only in DEV builds).
 */
export default defineConfig({
  testDir: './e2e',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  retries: 0,
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'e2e-report' }]],
  use: {
    // Port 5175 on purpose: 5173 is commonly taken by other local dev
    // servers, and a strict-port instance we own keeps the harness isolated.
    baseURL: 'http://localhost:8123',
    // Narrow viewport on purpose: the drawer docks right at 820px, so a
    // ~920px frame keeps evidence screenshots focused on the workbench.
    viewport: { width: 920, height: 950 },
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  webServer: {
    command: 'npm run dev -- --port 8123 --strictPort',
    url: 'http://localhost:8123',
    reuseExistingServer: false,
    timeout: 180_000,
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
})
