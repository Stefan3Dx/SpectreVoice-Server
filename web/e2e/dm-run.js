// End-to-end encrypted private messages between two browsers (two identities, two IndexedDBs), in headless
// Chromium through a real server; driven by TestWebDmE2E (webapp_test.go):
//
//	node web/e2e/dm-run.js <server url> <secret text> [screenshot dir]
//
// Ana opens a private chat with Bob from his card, sees the lock, sends <secret text> and a picture; Bob gets both
// decrypted (the picture opened from the encrypted upload), with the lock; Bob answers; both show the same safety
// number. Prints SENT once Ana's line is delivered (the Go test then looks at the database), OK at the end.
// Screenshots (when a folder is given): e2e-web-dm.png (Ana's private chat), e2e-web-safety.png (the safety number).

const { chromium } = require('playwright');
const path = require('path');

const [base, secret, shots] = process.argv.slice(2);
if (!base || !secret) {
  console.error('usage: dm-run.js <url> <secret> [dir]');
  process.exit(2);
}
// A 2×2 PNG
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAFklEQVR4AWP4z8DwnwEJMDEwMDAAAEcPAgFpuRzGAAAAAElFTkSuQmCC', 'base64');

async function join(browser, name) {
  const ctx = await browser.newContext({ locale: 'en-US', viewport: { width: 1180, height: 760 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.error(name + ' page error:', e.message));
  page.on('console', (m) => { if (m.type() === 'error') console.error(name + ' console:', m.text()); });
  await page.goto(base + '/app/', { waitUntil: 'load' });
  await page.waitForSelector('#uidNote:not(:empty)', { timeout: 15000 });
  await page.fill('#name', name);
  await page.click('#go');
  await page.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
  return page;
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream'] });
  try {
    const ana = await join(browser, 'Ana');
    const bob = await join(browser, 'Bob');
    // Ana: Bob's card › Message
    await ana.waitForFunction(() => [...document.querySelectorAll('.tree .mem .mname')].some((e) => e.textContent === 'Bob'), null, { timeout: 15000 });
    await ana.click('.tree .mem:has(.mname:text-is("Bob"))');
    await ana.click('#dmOpen');
    await ana.waitForFunction(() => document.querySelector('.lockbadge')?.textContent.includes('End-to-end encrypted'), null, { timeout: 15000 });
    await ana.fill('#dmSay', secret);
    await ana.press('#dmSay', 'Enter');
    await ana.waitForFunction((s) => [...document.querySelectorAll('#dmList .msg.lock-sealed:not(.pending) .text')].some((e) => e.textContent === s), secret, { timeout: 15000 });
    console.log('SENT');
    await ana.setInputFiles('#dmFile', { name: 'dot.png', mimeType: 'image/png', buffer: PNG });
    await ana.waitForFunction(() => [...document.querySelectorAll('#dmList .msg:not(.pending) .filecard .fname')].some((e) => e.textContent === 'dot.png'), null, { timeout: 15000 });

    // Bob: the Private tab counts it; the chat shows it decrypted, with the lock, and the picture
    await bob.waitForFunction(() => document.querySelector('#tabDm')?.dataset.count === '2', null, { timeout: 15000 });
    await bob.click('#tabDm');
    await bob.click('.dmhead button:text-is("@Ana")');
    await bob.waitForFunction((s) => [...document.querySelectorAll('#dmList .msg.lock-sealed .text')].some((e) => e.textContent === s), secret, { timeout: 15000 });
    await bob.waitForSelector('#dmList .msg.lock-sealed img.pic[src^="blob:"]', { timeout: 15000 });
    const lock = await bob.textContent('#dmList .msg.lock-sealed .lock');
    if (!lock.includes('End-to-end encrypted')) throw new Error('no lock on the line: ' + lock);
    await bob.fill('#dmSay', 'got it, thanks');
    await bob.press('#dmSay', 'Enter');
    await ana.waitForFunction(() => [...document.querySelectorAll('#dmList .msg.lock-sealed .text')].some((e) => e.textContent === 'got it, thanks'), null, { timeout: 15000 });
    // Client-side search over the decrypted lines
    await ana.fill('#dmFilter', 'thanks');
    await ana.waitForFunction(() => document.querySelectorAll('#dmList .msg').length === 1, null, { timeout: 5000 });
    await ana.fill('#dmFilter', '');
    await ana.waitForFunction(() => document.querySelectorAll('#dmList .msg').length >= 3, null, { timeout: 5000 });
    if (shots) await ana.screenshot({ path: path.join(shots, 'e2e-web-dm.png') });

    // The same safety number on both sides
    await ana.click('#dmVerifyBtn');
    await ana.waitForSelector('#safetyNumber span');
    await bob.click('#dmVerifyBtn');
    await bob.waitForSelector('#safetyNumber span');
    const na = await ana.textContent('#safetyNumber');
    const nb = await bob.textContent('#safetyNumber');
    if (!/^\d{60}$/.test(na) || na !== nb) throw new Error('safety numbers: ' + na + ' / ' + nb);
    if (shots) await ana.screenshot({ path: path.join(shots, 'e2e-web-safety.png') });
    await ana.click('.dlg-foot button.primary');
    await ana.waitForSelector('.verified', { timeout: 5000 });
    console.log('OK');
  } catch (e) {
    console.error('dm e2e failed:', e.message);
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();
