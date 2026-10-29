// End-to-end check of moving people from the web client, in headless Chromium, driven by TestWebMoveE2E
// (webapp_move_test.go):
//
//	node web/e2e/move.js <server url>
//
// "Mod" opens the page and prints "UID <uid>"; the Go side makes that uid a moderator and prints "GO". The tree
// shows a microphone on voice channels and "#" on text ones (STEP icons). Mod opens Bob's card, "Move to…", picks
// AFK (STEP picked; the Go side checks Bob landed there and prints "GO"), then "Move to my channel" (STEP here).
// "OK" at the end; anything else: a message on stderr, exit 1. SV_WEB_SHOTS=<dir>: screenshots there.

const { chromium } = require('playwright');
const path = require('path');
const readline = require('readline');

const [base] = process.argv.slice(2);
if (!base) {
  console.error('usage: move.js <url>');
  process.exit(2);
}
const shots = process.env.SV_WEB_SHOTS || '';
const stdin = readline.createInterface({ input: process.stdin });
const lines = [];
let waiter = null;
stdin.on('line', (l) => { lines.push(l); if (waiter) waiter(); });
const waitStdin = (want) => new Promise((ok) => {
  const check = () => { const i = lines.indexOf(want); if (i >= 0) { lines.splice(i, 1); waiter = null; ok(); } };
  waiter = check;
  check();
});
const step = (s) => console.log('STEP ' + s);
async function shot(page, name) {
  if (!shots) return;
  await page.waitForTimeout(300);
  await page.screenshot({ path: path.join(shots, 'move-web-' + name + '.png') });
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream'] });
  try {
    const ctx = await browser.newContext({ permissions: ['microphone'], locale: 'en-US', viewport: { width: 1280, height: 800 } });
    const page = await ctx.newPage();
    page.on('pageerror', (e) => console.error('page error:', e.message));
    page.on('console', (m) => { if (m.type() === 'error') console.error('console:', m.text()); });
    await page.goto(base + '/app/', { waitUntil: 'load' });
    await page.waitForSelector('#uidNote:not(:empty)', { timeout: 15000 });
    await page.fill('#name', 'Mod');
    await page.click('#go');
    await page.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
    const uid = (await page.textContent('#uidNote')).replace(/^ID:\s*/, '').trim();
    console.log('UID ' + uid);
    await waitStdin('GO');
    await page.waitForFunction(() => import('./core.js').then((m) => m.may('move')), null, { timeout: 15000, polling: 200 });

    // Voice channels: a mic, no "#"; text channels: "#", no mic
    await page.waitForSelector('.tree .ch.text', { timeout: 10000 });
    const icons = await page.$$eval('.tree .ch', (els) => els.map((e) => ({
      name: e.querySelector('.chname').textContent, voice: e.classList.contains('voice'), mic: !!e.querySelector('.chic svg.ic-mic'),
      before: getComputedStyle(e, '::before').content,
    })));
    for (const c of icons) {
      if (c.voice && (!c.mic || c.before.includes('#'))) throw new Error('voice channel without its mic: ' + JSON.stringify(c));
      if (!c.voice && (c.mic || !c.before.includes('#'))) throw new Error('text channel without #: ' + JSON.stringify(c));
    }
    step('icons');
    await shot(page, 'tree');

    // Bob's card → Move to… → AFK
    const bob = '.tree .mem:has(.mname:text-is("Bob"))';
    await page.waitForSelector(bob, { timeout: 10000 });
    await page.click(bob);
    await page.waitForSelector('#volPop:not(.hidden) #moveTo', { timeout: 5000 });
    await page.click('#moveTo');
    await page.waitForSelector('#volPop .movelist .movech', { timeout: 5000 });
    await shot(page, 'picker');
    await page.click('#volPop .movech:has(.chname:text-is("AFK"))');
    step('picked');
    await waitStdin('GO');
    // Bob shows under AFK now; his card offers "Move to my channel"
    await page.waitForFunction(() => {
      const afk = [...document.querySelectorAll('.tree .ch')].find((e) => e.querySelector('.chname')?.textContent === 'AFK');
      return afk && afk.nextElementSibling?.classList.contains('mems') && afk.nextElementSibling.textContent.includes('Bob');
    }, null, { timeout: 10000 });
    await page.click(bob);
    await page.waitForSelector('#volPop:not(.hidden) #moveHere', { timeout: 5000 });
    await page.click('#moveHere');
    step('here');
    console.log('OK');
  } catch (e) {
    console.error('e2e failed:', e.message);
    process.exitCode = 1;
  } finally {
    await browser.close();
    process.exit(process.exitCode || 0);
  }
})();
