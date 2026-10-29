// End-to-end check of the web client's soundboard and music bot in headless Chromium, driven by TestWebE2ESound
// (webapp_sound_test.go):
//
//	node web/e2e/sound.js <server url>
//
// Prints "UID Ana <uid>" before Ana connects and waits for "GO" (the test makes her an owner and connects an app of
// its own meanwhile). Ana uploads a sound (a WAV made here) from the soundboard's dialog and plays it: Bob's page
// decodes and plays it (WebAudio), "PLAYED <hash>" lets the test check its app got the same, then waits for
// "PCPLAYED" (the app played it) and checks Bob's page played that one too. Then Bob drives the music bot from its
// dialog: a song, a second one queued, pause, resume, skip, stop. "STEP <name>" after each, "OK" at the end;
// anything else: stderr, exit 1. SV_WEB_SHOTS=<dir>: screenshots there.

const { chromium } = require('playwright');
const fs = require('fs');
const os = require('os');
const path = require('path');
const readline = require('readline');

const [base] = process.argv.slice(2);
if (!base) {
  console.error('usage: sound.js <url>');
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
  fs.mkdirSync(shots, { recursive: true });
  await page.waitForTimeout(300);
  await page.screenshot({ path: path.join(shots, 'web-' + name + '.png') });
}

// 1.5 s of a 660 Hz tone, 16-bit mono 48 kHz WAV
function wav() {
  const rate = 48000, n = rate * 1.5;
  const b = Buffer.alloc(44 + n * 2);
  b.write('RIFF', 0); b.writeUInt32LE(36 + n * 2, 4); b.write('WAVE', 8); b.write('fmt ', 12);
  b.writeUInt32LE(16, 16); b.writeUInt16LE(1, 20); b.writeUInt16LE(1, 22); b.writeUInt32LE(rate, 24); b.writeUInt32LE(rate * 2, 28);
  b.writeUInt16LE(2, 32); b.writeUInt16LE(16, 34); b.write('data', 36); b.writeUInt32LE(n * 2, 40);
  for (let i = 0; i < n; i++) b.writeInt16LE(Math.round(Math.sin(2 * Math.PI * 660 * i / rate) * 16000), 44 + i * 2);
  return b;
}

async function open(browser, name) {
  const ctx = await browser.newContext({ permissions: ['microphone'], locale: 'en-US', viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.error(name + ' page error:', e.message));
  page.on('console', (m) => { if (m.type() === 'error') console.error(name + ' console:', m.text()); });
  await page.goto(base + '/app/', { waitUntil: 'load' });
  await page.waitForSelector('#uidNote:not(:empty)', { timeout: 15000 });
  const uid = (await page.textContent('#uidNote')).replace('ID: ', '').trim();
  await page.fill('#name', name);
  return { page, uid, ctx };
}
async function connect(p) {
  await p.page.click('#go');
  await p.page.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
}
const played = (page, n) => page.waitForFunction((k) => +(document.body.dataset.sbPlayed || 0) >= k, n, { timeout: 15000 });

(async () => {
  const browser = await chromium.launch({
    headless: true,
    args: ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
  });
  let failedPage = null;
  try {
    const ana = await open(browser, 'Ana');
    const bob = await open(browser, 'Bob');
    console.log('UID Ana ' + ana.uid);
    await waitStdin('GO');
    await connect(ana);
    await connect(bob);
    step('connected');

    // ---- Soundboard: Ana (soundboard_manage) adds a sound from the page
    failedPage = ana.page;
    await ana.page.waitForSelector('#moduleTools #sbOpen:not(.hidden)', { timeout: 10000 });
    await ana.page.click('#sbOpen');
    await ana.page.waitForSelector('#sbView .sbempty');
    await ana.page.click('#sbAdd');
    const file = path.join(os.tmpdir(), 'sv-e2e-tone-' + process.pid + '.wav');
    fs.writeFileSync(file, wav());
    await ana.page.setInputFiles('#sbFile', file);
    await ana.page.fill('#sbName', 'Tada');
    await ana.page.click('.sbpick:has-text("🎉")');
    await ana.page.click('#sbSave');
    await ana.page.waitForSelector('#sbGrid .sbtile:has-text("Tada")', { timeout: 60000 }); // ffmpeg converts it (slow on a busy machine)
    fs.unlinkSync(file); // the page read it
    // Bob's list has it too (the server tells everyone)
    await bob.page.waitForFunction(() => !document.querySelector('#sbOpen').classList.contains('hidden'));
    step('upload');

    // Bob's soundboard open, Ana plays: Bob's page decodes and plays it
    failedPage = bob.page;
    await bob.page.click('#sbOpen');
    await bob.page.waitForSelector('#sbGrid .sbtile:has-text("Tada")');
    await ana.page.click('#sbGrid .sbtile:has-text("Tada")');
    await played(bob.page, 1);
    const got = await bob.page.evaluate(() => ({ ...document.body.dataset }));
    if (!(+got.sbSecs > 1.3 && +got.sbSecs < 1.7) || !(+got.sbPeak > 0.1)) throw new Error('Bob played ' + JSON.stringify(got));
    await bob.page.waitForSelector('#sbPlaying:has-text("Ana")');
    await played(ana.page, 1); // the one who played it hears it too
    await ana.page.waitForSelector('.sbbadge'); // and waits for the next
    await shot(bob.page, 'soundboard');
    console.log('PLAYED ' + got.sbLast);
    step('played');

    // The app's sound reaches the page
    await waitStdin('PCPLAYED');
    await played(bob.page, 2);
    await bob.page.waitForSelector('#sbPlaying:has-text("Pc")');
    step('from the app');

    // Muted here: the next one isn't played
    await bob.page.click('#sbMute');
    await ana.page.waitForFunction(() => !document.querySelector('.sbbadge'), null, { timeout: 8000 });
    await ana.page.waitForTimeout(800);
    await ana.page.click('#sbGrid .sbtile:has-text("Tada")');
    await played(ana.page, 3);
    await bob.page.waitForTimeout(800);
    if (+(await bob.page.evaluate(() => document.body.dataset.sbPlayed)) !== 2) throw new Error('played while muted');
    await bob.page.click('#sbMute');
    step('mute');

    // Ana renames it: everyone's list changes
    failedPage = ana.page;
    await ana.page.click('#sbGrid .sbtile:has-text("Tada") .sbedit');
    await ana.page.fill('#sbName', 'Fanfare');
    await ana.page.click('#sbSave');
    await bob.page.waitForSelector('#sbGrid .sbtile:has-text("Fanfare")');
    await shot(ana.page, 'soundboard-manage');
    step('rename');

    // ---- The music bot, from Bob's page
    failedPage = bob.page;
    await bob.page.keyboard.press('Escape');
    await bob.page.waitForSelector('#muOpen:not(.hidden)');
    await bob.page.click('#muOpen');
    await bob.page.waitForSelector('#muView #muQ');
    await bob.page.fill('#muQ', 'one');
    await bob.page.click('#muAdd');
    await bob.page.waitForSelector('#muNow .mutitle:has-text("Tone one")', { timeout: 15000 });
    // The bot is in the channel, as a member
    await bob.page.waitForSelector('.tree .mem:has-text("Music")');
    await bob.page.fill('#muQ', 'two');
    await bob.page.press('#muQ', 'Enter');
    await bob.page.waitForSelector('#muQueue li:has-text("Tone two")', { timeout: 15000 });
    await shot(bob.page, 'music');
    step('queue');
    await bob.page.click('#muPause');
    await bob.page.waitForSelector('#muPause:has-text("Resume")');
    await bob.page.waitForSelector('#muNow:has-text("paused")');
    await bob.page.click('#muPause');
    await bob.page.waitForSelector('#muPause:has-text("Pause")');
    step('pause');
    await bob.page.click('#muSkip');
    await bob.page.waitForSelector('#muNow .mutitle:has-text("Tone two")', { timeout: 15000 });
    if (await bob.page.$('#muQueue')) throw new Error('the queue should be empty');
    step('skip');
    // Ana sees the same, from her own dialog
    failedPage = ana.page;
    await ana.page.keyboard.press('Escape');
    await ana.page.click('#muOpen');
    await ana.page.waitForSelector('#muNow .mutitle:has-text("Tone two")');
    await ana.page.fill('#muVol', '40');
    await ana.page.dispatchEvent('#muVol', 'change');
    await bob.page.waitForFunction(() => document.querySelector('#muVol')?.value === '40', null, { timeout: 8000 });
    step('volume');
    failedPage = bob.page;
    await bob.page.click('#muStop');
    await bob.page.waitForSelector('#muNow:has-text("Nothing playing")', { timeout: 15000 });
    await bob.page.waitForFunction(() => ![...document.querySelectorAll('.tree .mem')].some((e) => e.textContent.includes('Music')), null, { timeout: 15000 });
    step('stop');
    console.log('OK');
  } catch (e) {
    console.error('FAIL', e?.stack || e);
    if (failedPage && shots) await failedPage.screenshot({ path: path.join(shots, 'web-sound-fail.png') }).catch(() => {});
    await browser.close();
    process.exit(1);
  }
  await browser.close();
  process.exit(0);
})();
