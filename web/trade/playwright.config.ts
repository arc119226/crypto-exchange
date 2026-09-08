import { defineConfig, devices } from '@playwright/test'

// The smoke runs against a stack that is already up (compose app profile in
// CI, a local `exchange serve` on a laptop): API_URL / WS_URL / ADMIN_URL
// name it and the Vite preview server proxies the browser to it, so the
// page is same-origin with the API the way a deployment is. The admin key
// is used from Node only (the faucet); it never reaches the browser.
const port = 5173

export default defineConfig({
  testDir: './e2e',
  timeout: 90_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: `http://localhost:${port}`,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `npm run preview -- --port ${port} --strictPort`,
    url: `http://localhost:${port}/`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
})
