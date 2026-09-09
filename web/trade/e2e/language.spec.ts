import { expect, test } from '@playwright/test'

// The language switch (ADR-0010): English for a browser that has not
// chosen (Playwright's Chromium reports en-US), Traditional Chinese after
// one click, the choice kept in localStorage and honoured after a reload,
// and back again. Only the public markets page is used, so this runs
// without the admin faucet.

test('the interface switches between English and Traditional Chinese and remembers the choice', async ({ page }) => {
  await page.goto('/markets')
  await expect(page.locator('html')).toHaveAttribute('lang', 'en')
  await expect(page.getByTestId('nav-markets')).toHaveText('Markets')

  await page.getByTestId('lang-zh-TW').click()
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh-TW')
  await expect(page.getByTestId('nav-markets')).toHaveText('市場')
  expect(await page.evaluate(() => localStorage.getItem('exchange.lang'))).toBe('zh-TW')

  await page.reload()
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh-TW')
  await expect(page.getByTestId('nav-markets')).toHaveText('市場')

  await page.getByTestId('lang-en').click()
  await expect(page.locator('html')).toHaveAttribute('lang', 'en')
  await expect(page.getByTestId('nav-markets')).toHaveText('Markets')
  expect(await page.evaluate(() => localStorage.getItem('exchange.lang'))).toBe('en')
})
