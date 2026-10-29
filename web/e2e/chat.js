// End-to-end check of the web client's chat as the PC app has it, in headless Chromium, driven by TestWebChatE2E
// (webapp_chat_test.go), with a Go client standing in for the PC app (its hub and bus connections):
//
//	node web/e2e/chat.js <server url>
//
// Ana and Bob open the page and connect. Ana says "hello bob"; Bob reacts 👍 from the line's toolbar (Ana and the
// "PC" see it, with the PC's key), replies (the quote shows at Ana's), Ana edits her line (Bob sees the new text
// and "(edited)") and deletes it (gone at Bob's); Bob @mentions Ana with the completion (lit at Ana's) while Ana
// sees him typing; the emoji picker writes into the line. Prints "STEP <name>" after each; after "STEP reply" it
// waits for "GO" on stdin (the Go client reacts ❤️ to Bob's reply meanwhile, which Ana's page must show). "OK" at
// the end; anything else: a message on stderr, exit 1. SV_WEB_SHOTS=<dir>: screenshots there.

const { chromium } = require('playwright');
const path = require('path');
const readline = require('readline');

const [base] = process.argv.slice(2);
if (!base) {
  console.error('usage: chat.js <url>');
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
  await page.screenshot({ path: path.join(shots, 'chat-' + name + '.png') });
}

async function open(browser, name) {
  const ctx = await browser.newContext({ permissions: ['microphone'], locale: 'en-US', viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.error(name + ' page error:', e.message));
  page.on('console', (m) => { if (m.type() === 'error') console.error(name + ' console:', m.text()); });
  await page.goto(base + '/app/', { waitUntil: 'load' });
  await page.waitForSelector('#uidNote:not(:empty)', { timeout: 15000 });
  await page.fill('#name', name);
  await page.click('#go');
  await page.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
  // The bus is up (reactions and typing go through it)
  await page.waitForFunction(() => import('./bus.js').then((m) => m.bus.online), null, { timeout: 15000, polling: 200 });
  return page;
}
// The line whose text is exactly this
const lineWith = (text) => `#msgList .msg:has(.text:text-is(${JSON.stringify(text)}))`;

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream'] });
  try {
    const ana = await open(browser, 'Ana');
    const bob = await open(browser, 'Bob');
    step('connected');

    // ====== A line, a reaction from its toolbar ======
    await ana.fill('#say', 'hello bob');
    await ana.press('#say', 'Enter');
    const hello = lineWith('hello bob');
    await bob.waitForSelector(hello + ':not(.pending)', { timeout: 10000 });
    await bob.hover(hello);
    await bob.click(hello + ' .mtools .tq:text-is("👍")');
    await ana.waitForSelector(hello + ' .react:has(.remo:text-is("👍")):has(.rn:text-is("1"))', { timeout: 10000 });
    await bob.waitForSelector(hello + ' .react.mine:has(.remo:text-is("👍"))', { timeout: 5000 });
    step('react');

    // ====== A reply: the quote at the others' ======
    await bob.hover(hello);
    await bob.click(hello + ' .mtools .treply');
    await bob.waitForSelector('#replyBar:not(.hidden) .rwhat:text-is("hello bob")', { timeout: 5000 });
    await bob.fill('#say', 'hi ana');
    await bob.press('#say', 'Enter');
    const reply = lineWith('hi ana');
    await ana.waitForSelector(reply + ' .quote:has(.qwho:text-is("↪ Ana")):has(.qwhat:text-is("hello bob"))', { timeout: 10000 });
    if (!(await bob.$eval('#replyBar', (e) => e.classList.contains('hidden')))) throw new Error('the reply bar stayed');
    step('reply');
    await waitStdin('GO');
    // The "PC" reacted ❤️ to Bob's reply (its key: "Bob|" + the hash of the whole text, quote included)
    await ana.waitForSelector(reply + ' .react:has(.remo:text-is("❤️"))', { timeout: 10000 });
    await ana.hover(reply);
    await shot(ana, 'react-reply');
    step('pc reaction');

    // ====== Editing in place, then deleting ======
    await ana.hover(hello);
    await ana.click(hello + ' .mtools .tedit');
    await ana.fill('.editbox', 'hello bob, again');
    await ana.press('.editbox', 'Enter');
    const edited = lineWith('hello bob, again');
    await bob.waitForSelector(edited + ' .edit', { timeout: 10000 });
    if (await bob.$(hello)) throw new Error('the old text stayed');
    // Bob may not edit or delete Ana's line
    await bob.hover(edited);
    if (await bob.$(edited + ' .mtools .tedit') || await bob.$(edited + ' .mtools .tdel')) throw new Error("Bob has tools on Ana's line");
    step('edit');
    await ana.hover(edited);
    await ana.click(edited + ' .mtools .tdel');
    await bob.waitForSelector(edited, { state: 'detached', timeout: 10000 });
    await ana.waitForSelector(edited, { state: 'detached', timeout: 10000 });
    step('delete');

    // ====== @mentions, typing ======
    await bob.click('#say');
    await bob.keyboard.type('@An');
    await bob.waitForSelector('#mentions:not(.hidden) .mchip:text-is("@Ana")', { timeout: 5000 });
    await ana.waitForSelector('#typing:has-text("Bob is typing")', { timeout: 10000 });
    await shot(bob, 'mention');
    await bob.keyboard.press('Tab');
    if ((await bob.inputValue('#say')) !== '@Ana ') throw new Error('completion: ' + (await bob.inputValue('#say')));
    await bob.keyboard.type('look');
    await bob.keyboard.press('Enter');
    await ana.waitForSelector(lineWith('@Ana look') + '.mention', { timeout: 10000 });
    await ana.waitForSelector('#typing:not(:has(span))', { timeout: 10000 });
    step('mention');

    // ====== The emoji picker ======
    await ana.click('#emojiBtn');
    await ana.waitForSelector('#emojiPop:not(.hidden)', { timeout: 5000 });
    await shot(ana, 'emoji');
    await ana.click('#emojiPop .emo:text-is("🔥")');
    if ((await ana.inputValue('#say')) !== '🔥') throw new Error('emoji: ' + (await ana.inputValue('#say')));
    if (!(await ana.$eval('#emojiPop', (e) => e.classList.contains('hidden')))) throw new Error('the picker stayed open');
    step('emoji');
    console.log('OK');
  } catch (e) {
    console.error('e2e failed:', e.message);
    process.exitCode = 1;
  } finally {
    await browser.close();
    stdin.close();
  }
})();
