// Screenshots of every back-office page, for docs/screenshots (docs/plan-v1.0.md §12 DoD).
//
// Needs a running admin role and an administrator whose TOTP has been
// enrolled (exchange admin totp enroll). The code is computed here from the
// secret, so nothing has to be typed:
//
//   ADMIN_URL=http://localhost:8082 ADMIN_EMAIL=admin@example.com \
//   ADMIN_PASSWORD=... ADMIN_TOTP_SECRET=<base32> node scripts/screenshots.mjs
//
// Uses the globally installed playwright (NODE_PATH=$(npm root -g)); the
// browser comes from PLAYWRIGHT_BROWSERS_PATH or CHROMIUM.
import { createHmac } from 'node:crypto';
import { mkdirSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';

// ES modules do not consult NODE_PATH; CommonJS require does, so the global
// playwright is loaded through it (make screenshots sets NODE_PATH).
const require = createRequire(import.meta.url);
function loadPlaywright() {
  const candidates = ['playwright', ...(process.env.NODE_PATH ?? '').split(':').filter(Boolean).map((p) => join(p, 'playwright'))];
  for (const c of candidates) {
    try {
      return require(c);
    } catch {
      // next
    }
  }
  throw new Error('playwright not found: npm install -g playwright, then NODE_PATH=$(npm root -g)');
}
const { chromium } = loadPlaywright();

const base = (process.env.ADMIN_URL ?? 'http://localhost:8082').replace(/\/$/, '');
const email = process.env.ADMIN_EMAIL;
const password = process.env.ADMIN_PASSWORD;
const secret = process.env.ADMIN_TOTP_SECRET;
const outDir = process.env.SCREENSHOT_DIR ?? 'docs/screenshots';
if (!email || !password || !secret) {
  console.error('ADMIN_EMAIL, ADMIN_PASSWORD and ADMIN_TOTP_SECRET are required');
  process.exit(2);
}
mkdirSync(outDir, { recursive: true });

// RFC 6238 with the defaults the server uses: SHA-1, six digits, 30 seconds.
function totp(base32, at = Date.now()) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const c of base32.toUpperCase().replace(/=+$/, '')) {
    const v = alphabet.indexOf(c);
    if (v < 0) throw new Error(`not base32: ${c}`);
    bits += v.toString(2).padStart(5, '0');
  }
  const key = Buffer.from(bits.match(/.{8}/g).map((b) => parseInt(b, 2)));
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(at / 1000 / 30)));
  const mac = createHmac('sha1', key).update(counter).digest();
  const offset = mac[mac.length - 1] & 0x0f;
  const code = ((mac[offset] & 0x7f) << 24 | mac[offset + 1] << 16 | mac[offset + 2] << 8 | mac[offset + 3]) % 1_000_000;
  return code.toString().padStart(6, '0');
}

const browser = await chromium.launch({ executablePath: process.env.CHROMIUM || undefined });
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
let n = 0;
const shot = async (name) => {
  n += 1;
  const file = join(outDir, `${String(n).padStart(2, '0')}-${name}.png`);
  await page.screenshot({ path: file, fullPage: true });
  console.log(file);
};

await page.goto(`${base}/admin/login`);
await shot('login');
await page.fill('input[name=email]', email);
await page.fill('input[name=password]', password);
await page.click('button[type=submit]');
await page.waitForURL('**/admin/totp');
await shot('totp');
await page.fill('input[name=code]', totp(secret));
await page.click('form[action="/admin/totp"] button[type=submit]');
await page.waitForURL('**/admin/');
await shot('dashboard');

for (const name of ['assets', 'markets', 'fee-schedules', 'withdrawal-limits', 'users']) {
  await page.goto(`${base}/admin/${name}`);
  await shot(name);
}
// one user's page: the first link in the users table
const user = await page.locator('table a[href^="/admin/users/"]').first();
if (await user.count()) {
  await user.click();
  await page.waitForURL('**/admin/users/*');
  await shot('user');
}
for (const name of ['ledger', 'withdrawals', 'chain', 'reconciliation', 'audit', 'webhooks']) {
  await page.goto(`${base}/admin/${name}`);
  await shot(name);
}
await browser.close();
