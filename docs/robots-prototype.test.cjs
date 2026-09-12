// Run with playwright-core on NODE_PATH and CHROME_PATH pointing to Chrome.
const assert = require('node:assert/strict');
const { pathToFileURL } = require('node:url');
const { join } = require('node:path');
const { chromium } = require('playwright-core');

(async () => {
  const browser = await chromium.launch({ executablePath: process.env.CHROME_PATH, headless: true });
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(pathToFileURL(join(__dirname, 'robots-prototype.html')).href);
    for (const width of [1440, 760]) {
      await page.setViewportSize({ width, height: 900 });
      const row = await page.locator('.bot-title').boundingBox();
      const select = await page.locator('#robot-select').boundingBox();
      const settings = await page.locator('#settings').boundingBox();
      assert.equal(Math.round(settings.x + settings.width), Math.round(row.x + row.width));
      assert.equal(Math.round(select.width), Math.round(row.width - 48));
    }
    assert.equal(await page.locator('.delete-btn').count(), 3);
    await page.locator('[data-delete-session="12"]').click();
    assert.equal(await page.locator('#count').innerText(), '2');
    assert(await page.locator('[data-session="11"]').getAttribute('aria-pressed') === 'true');
    await page.locator('[data-session="13"]').click();
    assert((await page.locator('.workspace h2').innerText()).includes('接口联调'));
    await page.locator('[data-delete-session="13"]').click();
    assert.equal(await page.locator('#workspace').innerHTML(), '');
    await page.locator('#robot-select').selectOption('2');
    assert.equal(await page.locator('.delete-btn').count(), 2);
    await page.locator('#robot-select').selectOption('1');
    await page.locator('[data-delete-session="11"]').click();
    assert.equal(await page.locator('#count').innerText(), '0');
    assert.equal(await page.locator('.delete-btn').count(), 0);
    assert.deepEqual(errors, []);
    console.log('PASS: session deletion, selection, empty state and bot isolation');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
