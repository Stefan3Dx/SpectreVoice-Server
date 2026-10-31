// End-to-end check of the web client's streams, TV, files and settings in headless Chromium, driven by
// TestWebE2EMedia (webapp_media_test.go):
//
//	node web/e2e/media.js <server url> <second server url (no SFU rooms, password "pw")> <TV playlist url>
//
// Prints "UID <who> <uid>" for each page before it connects and waits for "GO" on stdin (the test makes Ana an
// owner meanwhile). Then: Ana shares her screen (a fake one), Bob watches it through the server; the TV: Ana sets
// the playlist, the list shows with logos, she puts a channel on, both watch it; channel files: Ana uploads, Bob
// downloads the same bytes; a picture in the chat shows in the line at Bob's; unread counters; settings; the
// password prompt and a screen share peer to peer on the second server (it takes no stream through the SFU, so the
// share goes P2P), watched by Dan. "STEP <name>" after each, "OK" at the end; anything else: stderr, exit 1.
// SV_WEB_SHOTS=<dir>: screenshots of each view there.

const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');
const readline = require('readline');

const [base, base2, tvList] = process.argv.slice(2);
if (!base || !base2 || !tvList) {
  console.error('usage: media.js <url> <url2> <tv list url>');
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
  await page.screenshot({ path: path.join(shots, 'feat-c-' + name + '.png') });
}

// A 64×64 PNG (a violet square) for the picture in the chat
function png() {
  const zlib = require('zlib');
  const w = 64, h = 64;
  const raw = Buffer.alloc((w * 3 + 1) * h);
  for (let y = 0; y < h; y++) {
    raw[y * (w * 3 + 1)] = 0;
    for (let x = 0; x < w; x++) { const o = y * (w * 3 + 1) + 1 + x * 3; raw[o] = 124; raw[o + 1] = 92; raw[o + 2] = 255; }
  }
  const crc = (buf) => { let c, crcs = []; for (let n = 0; n < 256; n++) { c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; crcs[n] = c >>> 0; } let r = 0xffffffff; for (const b of buf) r = crcs[(r ^ b) & 0xff] ^ (r >>> 8); return (r ^ 0xffffffff) >>> 0; };
  const chunk = (type, data) => { const l = Buffer.alloc(4); l.writeUInt32BE(data.length); const td = Buffer.concat([Buffer.from(type), data]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([l, td, c]); };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  return Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}

async function open(browser, url, name, opts = {}) {
  const ctx = await browser.newContext({ permissions: ['microphone', 'notifications'], locale: 'en-US', viewport: opts.viewport || { width: 1280, height: 860 }, acceptDownloads: true });
  // Every WebSocket the page opens, so a test can drop them all (a network change)
  if (opts.trackWs) await ctx.addInitScript(() => { const O = window.WebSocket; window.__ws = []; window.WebSocket = class extends O { constructor(...a) { super(...a); window.__ws.push(this); } }; });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.error(name + ' page error:', e.message));
  page.on('console', (m) => { if (m.type() === 'error') console.error(name + ' console:', m.text()); });
  await page.goto(url + '/app/', { waitUntil: 'load' });
  await page.waitForSelector('#uidNote:not(:empty)', { timeout: 15000 });
  const uid = (await page.textContent('#uidNote')).replace('ID: ', '').trim();
  await page.fill('#name', name);
  return { page, uid, ctx };
}
async function connect(p) {
  await p.page.click('#go');
  await p.page.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
}
const playing = (page, sel) => page.waitForFunction(async (s) => {
  const v = document.querySelector(s);
  if (!v || !v.videoWidth) return false;
  const t0 = v.currentTime;
  await new Promise((r) => setTimeout(r, 400));
  return v.currentTime > t0;
}, sel, { timeout: 30000, polling: 500 });

(async () => {
  const browser = await chromium.launch({
    headless: true,
    args: ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream', '--autoplay-policy=no-user-gesture-required', '--auto-select-desktop-capture-source=Entire screen'],
  });
  let failedPage = null;
  try {
    const ana = await open(browser, base, 'Ana');
    const bob = await open(browser, base, 'Bob', { trackWs: true });
    console.log('UID Ana ' + ana.uid);
    await waitStdin('GO');
    await connect(ana);
    await connect(bob);
    failedPage = bob.page;
    step('connected');

    // ====== A screen share through the server ======
    await ana.page.waitForSelector('#shareBtn:not(.hidden)', { timeout: 10000 });
    await ana.page.click('#shareBtn');
    await ana.page.waitForSelector('.tile.self video', { timeout: 10000 });
    await ana.page.waitForFunction(() => /via server/.test(document.querySelector('#shareInfo')?.textContent || ''), null, { timeout: 20000 });
    const item = `.liveitem[data-room="${ana.uid}"] button`;
    await bob.page.waitForSelector(item, { timeout: 15000 });
    await shot(bob.page, 'live-list');
    await bob.page.click(item);
    const tile = `.tile[data-room="${ana.uid}"]`;
    await playing(bob.page, tile + ' video');
    const via = await bob.page.getAttribute(tile + ' .via', 'data-via');
    if (via !== 'server') throw new Error('via ' + via);
    await ana.page.waitForFunction(() => /1 watching/.test(document.querySelector('#shareInfo')?.textContent || ''), null, { timeout: 15000 });
    // Simulcast: the quality picker has the layers; the smallest asked for, the picture gets smaller
    await bob.page.waitForSelector(tile + ' select.q:not(.hidden) option[value="h"]', { state: 'attached', timeout: 15000 });
    await bob.page.waitForSelector(tile + ' select.q:not(.hidden) option[value="l"]', { state: 'attached', timeout: 15000 });
    // The full size first (auto may already be on a small layer while the link warms up)
    await bob.page.selectOption(tile + ' select.q', 'h');
    await bob.page.waitForFunction((s) => { const v = document.querySelector(s + ' video'); return v.videoWidth > 0 && document.querySelector(s).dataset.layer === 'h'; }, tile, { timeout: 15000 });
    await bob.page.waitForTimeout(500);
    const full = await bob.page.$eval(tile + ' video', (v) => v.videoWidth);
    await bob.page.selectOption(tile + ' select.q', 'l');
    await bob.page.waitForFunction(([s, w]) => { const v = document.querySelector(s + ' video'); return v.videoWidth > 0 && v.videoWidth <= w / 3 && document.querySelector(s).dataset.layer === 'l'; }, [tile, full], { timeout: 15000 });
    step('quality picker: ' + full + ' → ' + (await bob.page.$eval(tile + ' video', (v) => v.videoWidth)));
    await bob.page.selectOption(tile + ' select.q', 'auto');
    await bob.page.hover(tile);
    await shot(bob.page, 'watch-sfu');
    await shot(ana.page, 'share-screen');
    step('share watched through the server');
    // Bob's network drops (all his sockets close): his watch ends with it, and once he's back it comes back by itself
    await bob.page.evaluate(() => { for (const s of window.__ws) try { s.close(); } catch { /* gone */ } });
    await bob.page.waitForSelector(tile + '.ended', { timeout: 15000 });
    await bob.page.waitForSelector(tile + ':not(.ended)', { timeout: 20000 });
    await playing(bob.page, tile + ' video');
    step('watched again after a drop');
    // Ana stops: Bob's tile says so
    await ana.page.click('#shareBtn');
    await bob.page.waitForSelector(tile + '.ended', { timeout: 15000 });
    await bob.page.click(tile + ' .tclose');
    step('share ended');
    // A double click on Share: one share, one preview (the second capture stayed, and its publish ended the first)
    await ana.page.evaluate(() => { const b = document.querySelector('#shareBtn'); b.click(); b.click(); });
    await ana.page.waitForFunction(() => /via server/.test(document.querySelector('#shareInfo')?.textContent || ''), null, { timeout: 20000 });
    await ana.page.waitForTimeout(1500);
    const selfTiles = await ana.page.$$eval('.tile.self', (els) => els.length);
    if (selfTiles !== 1) throw new Error('double click: ' + selfTiles + ' previews');
    if (!(await ana.page.$eval('#shareBtn', (b) => b.classList.contains('live')))) throw new Error('double click: the share stopped');
    await ana.page.click('#shareBtn');
    await ana.page.waitForFunction(() => !document.querySelector('.tile.self'), null, { timeout: 10000 });
    step('double click shares once');

    // ====== The TV ======
    await ana.page.waitForSelector('#tvCard:not(.hidden) #tvOpen', { timeout: 10000 });
    await ana.page.click('#tvOpen');
    await ana.page.fill('#tvUrl', tvList);
    await ana.page.click('.tvadmin form button[type=submit]');
    await ana.page.waitForSelector('.tvch[data-i="0"]', { timeout: 30000 });
    const chans = await ana.page.$$eval('.tvch .tvname', (els) => els.map((e) => e.textContent));
    if (!chans.includes('Test Channel')) throw new Error('TV list: ' + JSON.stringify(chans));
    await ana.page.waitForFunction(() => { const i = document.querySelector('.tvch[data-i="0"] img.tvlogo'); return i && i.complete && i.naturalWidth > 0; }, null, { timeout: 15000 });
    step('tv list ' + JSON.stringify(chans));
    await ana.page.click('.tvch[data-i="0"]');
    await ana.page.waitForSelector('.tvch.on[data-i="0"]', { timeout: 15000 });
    await shot(ana.page, 'tv-list');
    // Bob sees what's on, may not change it (not open to everyone)
    await bob.page.waitForFunction(() => /Test Channel/.test(document.querySelector('#tvCard')?.textContent || ''), null, { timeout: 15000 });
    await bob.page.click('#tvOpen');
    await bob.page.waitForSelector('.tvch[data-i="1"]', { timeout: 15000 });
    if (!(await bob.page.$eval('.tvch[data-i="1"]', (b) => b.disabled))) throw new Error('Bob may change the TV channel');
    await bob.page.click('.tvbar button.primary');
    await playing(bob.page, '.tile[data-room="tv:main"] video');
    await bob.page.hover('.tile[data-room="tv:main"]');
    await bob.page.waitForTimeout(1500);
    await shot(bob.page, 'tv-watch');
    step('tv watched');
    await bob.page.click('.tile[data-room="tv:main"] .tclose');
    await ana.page.keyboard.press('Escape');
    await ana.page.waitForSelector('#dlg:not([open])', { state: 'attached', timeout: 5000 });

    // ====== Channel files ======
    await ana.page.click('#tabFiles');
    await ana.page.waitForSelector('#cfUpload', { timeout: 10000 });
    const content = 'hello from the web ' + Date.now();
    await ana.page.setInputFiles('#cfFile', { name: 'hello.txt', mimeType: 'text/plain', buffer: Buffer.from(content) });
    await ana.page.waitForFunction(() => [...document.querySelectorAll('#cfList .fname')].some((e) => e.textContent === 'hello.txt'), null, { timeout: 15000 });
    await ana.page.click('#cfMkdir');
    await ana.page.fill('#dlg input', 'Pictures');
    await ana.page.click('#dlg .dlg-foot .primary');
    await ana.page.waitForFunction(() => [...document.querySelectorAll('#cfList .fname')].some((e) => e.textContent === 'Pictures'), null, { timeout: 15000 });
    await shot(ana.page, 'files');
    await bob.page.click('#tabFiles');
    await bob.page.waitForFunction(() => [...document.querySelectorAll('#cfList a.fname')].some((e) => e.textContent === 'hello.txt'), null, { timeout: 15000 });
    const [dl] = await Promise.all([bob.page.waitForEvent('download'), bob.page.click('#cfList a.fname:text-is("hello.txt")')]);
    const got = fs.readFileSync(await dl.path(), 'utf8');
    if (got !== content) throw new Error('downloaded ' + JSON.stringify(got));
    // Bob may not delete Ana's file
    if (await bob.page.$('#cfList .file:has(a.fname:text-is("hello.txt")) .facts button')) throw new Error("Bob may delete Ana's file");
    step('files uploaded and downloaded');

    // ====== A picture in the chat, unread counters ======
    await ana.page.click('#tabChannel');
    await bob.page.click('#tabChannel');
    await ana.page.setInputFiles('#attachFile', { name: 'violet.png', mimeType: 'image/png', buffer: png() });
    await bob.page.waitForFunction(() => { const i = document.querySelector('.msg img.pic'); return i && i.complete && i.naturalWidth === 64; }, null, { timeout: 15000 });
    const src = await bob.page.getAttribute('.msg img.pic', 'src');
    if (!src.startsWith('/f/')) throw new Error('picture from ' + src);
    await shot(bob.page, 'chat-picture');
    step('picture in the chat');
    await ana.page.click('#tabComm');
    await ana.page.fill('#say', 'to everyone');
    await ana.page.press('#say', 'Enter');
    await bob.page.waitForSelector('#tabComm.unread[data-count="1"]', { timeout: 10000 });
    await bob.page.click('#tabComm');
    await bob.page.waitForSelector('#tabComm:not(.unread)', { timeout: 5000 });
    step('unread counter');

    // ====== Settings ======
    await bob.page.click('#settings');
    await bob.page.waitForSelector('#setIn', { timeout: 5000 });
    await bob.page.click('#setPtt');
    await bob.page.keyboard.press('KeyT');
    if ((await bob.page.textContent('#setPtt')) !== 'T') throw new Error('ptt key ' + (await bob.page.textContent('#setPtt')));
    await shot(bob.page, 'settings');
    await bob.page.selectOption('#setLang', 'de');
    await bob.page.waitForFunction(() => document.documentElement.lang === 'de' && document.querySelector('#dlg h2')?.textContent === 'Einstellungen', null, { timeout: 5000 });
    await bob.page.click('#dlg .dlg-foot .primary');
    if ((await bob.page.textContent('#pttKey')) !== 'T') throw new Error('bar key');
    await bob.page.keyboard.down('KeyT');
    if (!(await bob.page.$eval('#ptt', (e) => e.classList.contains('live')))) throw new Error('T does not talk');
    await bob.page.keyboard.up('KeyT');
    step('settings');

    // ====== Phone layout ======
    const phone = await open(browser, base, 'Cleo', { viewport: { width: 390, height: 844 } });
    await connect(phone);
    await shot(phone.page, 'mobile-channels');
    await phone.page.click('.mobile-tabs [data-pane="chat"]');
    await shot(phone.page, 'mobile-chat');
    const wide = await phone.page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1);
    if (wide) throw new Error('the phone layout scrolls sideways');
    step('phone layout');

    // ====== Second server: the password prompt; a share peer to peer ======
    const dan = await open(browser, base2, 'Dan');
    const eve = await open(browser, base2, 'Eve');
    failedPage = dan.page;
    await dan.page.click('#go');
    await dan.page.waitForSelector('#dlg[open] #pwInput', { timeout: 10000 });
    await shot(dan.page, 'password');
    await dan.page.fill('#pwInput', 'nope');
    await dan.page.click('#pwGo');
    await dan.page.waitForSelector('#dlg[open] .error', { timeout: 10000 });
    await dan.page.fill('#pwInput', 'pw');
    await dan.page.click('#pwGo');
    await dan.page.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
    await eve.page.click('#go');
    await eve.page.waitForSelector('#dlg[open] #pwInput', { timeout: 10000 });
    await eve.page.fill('#pwInput', 'pw');
    await eve.page.press('#pwInput', 'Enter');
    await eve.page.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
    step('password prompt');
    await eve.page.click('#shareBtn');
    await eve.page.waitForFunction(() => /P2P/.test(document.querySelector('#shareInfo')?.textContent || ''), null, { timeout: 20000 });
    await dan.page.waitForSelector(`.liveitem[data-room="${eve.uid}"] button`, { timeout: 15000 });
    await dan.page.click(`.liveitem[data-room="${eve.uid}"] button`);
    const p2pTile = `.tile[data-room="${eve.uid}"]`;
    await playing(dan.page, p2pTile + ' video');
    await dan.page.waitForSelector(p2pTile + ' .via[data-via="p2p"], ' + p2pTile + ' .via[data-via="relay"]', { timeout: 15000 });
    await eve.page.waitForFunction(() => /1 watching/.test(document.querySelector('#shareInfo')?.textContent || ''), null, { timeout: 15000 });
    await dan.page.hover(p2pTile);
    await shot(dan.page, 'watch-p2p');
    step('share watched peer to peer');
    await eve.page.click('#shareBtn');
    await dan.page.waitForSelector(p2pTile + '.ended', { timeout: 15000 });
    step('p2p share ended');
    console.log('OK');
  } catch (e) {
    console.error('e2e failed:', e.message);
    if (failedPage && shots) await failedPage.screenshot({ path: path.join(shots, 'feat-c-failed.png') }).catch(() => {});
    process.exitCode = 1;
  } finally {
    stdin.close();
    await browser.close();
  }
})();
