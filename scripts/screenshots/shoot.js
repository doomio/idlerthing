// Screenshot driver: logs into a locally running idlerthing, configures a
// synthetic PulsedMedia provider key, syncs the feed, and captures the pages
// used in README.md.
//
// Driven by scripts/screenshots/main.go, which sets SHOTS_BASE and SHOTS_OUT.
// Run it with `go run ./scripts/screenshots`, not directly.
const { chromium } = require('playwright');

const BASE = process.env.SHOTS_BASE;
const OUT = process.env.SHOTS_OUT || 'docs/screenshots';
const PASSWORD = 'screenshot-pass';
const EMAIL = 'admin@localhost';

// The key the harness's mock feed accepts. Synthetic; see fixture.go.
const PMAPI_KEY = 'synthetic-screenshot-key';

const PAGES = [
  { file: 'dashboard.png', path: '/', full: true },
  { file: 'servers.png', path: '/servers' },
  { file: 'seedboxes.png', path: '/seedboxes' },
  { file: 'fleet.png', path: '/seedboxes/fleet' },
  { file: 'fleet-cancelled.png', path: '/seedboxes/fleet?cancelled=1' },
  { file: 'seedbox-detail.png', path: '/seedboxes/1' },
  { file: 'providers.png', path: '/catalogs/providers' },
];

// 'networkidle' is deliberately not used: htmx keeps requests in flight, so it
// never settles and the run hangs instead of failing.
const WAIT = 'domcontentloaded';

async function main() {
  if (!BASE) {
    throw new Error('SHOTS_BASE is not set; run this through scripts/screenshots/main.go');
  }
  const browser = await chromium.launch();
  const page = await browser.newPage({
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 1,
    colorScheme: 'dark',
  });
  // Fail fast: a hung selector should abort the run, not stall it.
  page.setDefaultTimeout(15000);
  page.setDefaultNavigationTimeout(15000);

  await login(page);
  await setProviderKey(page);
  await sync(page);

  for (const p of PAGES) {
    await page.goto(BASE + p.path, { waitUntil: WAIT });
    await page.waitForTimeout(300); // let fonts and layout settle
    await page.screenshot({ path: `${OUT}/${p.file}`, fullPage: Boolean(p.full) });
    console.log('captured', p.file, p.path);
  }

  await browser.close();
}

async function login(page) {
  await page.goto(BASE + '/login', { waitUntil: WAIT });
  await page.fill('input[name=email]', EMAIL);
  await page.fill('input[name=password]', PASSWORD);
  await Promise.all([
    page.waitForURL((u) => !u.pathname.startsWith('/login')),
    page.click('button[type=submit]'),
  ]);
  console.log('logged in');
}

// setProviderKey fills the write-only key field on the PulsedMedia provider row.
async function setProviderKey(page) {
  await page.goto(BASE + '/catalogs/providers', { waitUntil: WAIT });
  const row = page.locator('tr', { hasText: 'PulsedMedia' }).first();
  await row.locator('input[name=pm_api_key]').fill(PMAPI_KEY);
  await row.locator('button[title="Save key"]').click();
  await page.waitForLoadState(WAIT);

  // The key is write-only: the page must say one is set, never show it.
  const body = await page.content();
  if (body.includes(PMAPI_KEY)) {
    throw new Error('the provider page echoed the API key');
  }
  console.log('provider key set');
}

async function sync(page) {
  // The sync is a POST; drive it through the form on the fleet page.
  await page.goto(BASE + '/seedboxes/fleet', { waitUntil: WAIT });
  await page.click('button:has-text("Sync now")');
  await page.waitForLoadState(WAIT);
  await page.goto(BASE + '/seedboxes/fleet', { waitUntil: WAIT });

  const body = await page.content();
  if (!body.includes('Dragon-R')) {
    throw new Error('sync produced no fleet rows; the feed or the match failed');
  }
  console.log('feed synced and matched');
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
