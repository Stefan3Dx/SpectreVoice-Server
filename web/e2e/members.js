// The member list and a private message to someone offline, in headless Chromium through a real server; driven
// by TestWebMembersE2E (people_test.go):
//
//	node web/e2e/members.js <server url> <secret text> [screenshot dir]
//
// Bob comes in (his browser publishes its device key) and leaves. Ana opens the right column's Members tab: Bob is
// under "Offline — 1" (folding the section hides him, search finds him); she writes to him from his card and
// blocks Carl (gone from her list). Bob comes back in the same browser: the Private tab counts 1, the list shows Ana with an
// unread badge and the decrypted preview; opening the chat reads it and the count goes, the server's read marker
// with it (a reload doesn't bring it back). Prints SENT once Ana's line is delivered, OK at the end.
// Screenshots: dm-web-members.png (the member list), dm-web-list.png (Bob's private chats).

const { chromium } = require('playwright');
const path = require('path');

const [base, secret, shots] = process.argv.slice(2);
if (!base || !secret) {
  console.error('usage: members.js <url> <secret> [dir]');
  process.exit(2);
}

async function join(ctx, name) {
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
  const ctx = (n) => browser.newContext({ locale: 'en-US', viewport: { width: 1360, height: 820 } });
  try {
    const bobCtx = await ctx();
    let bob = await join(bobCtx, 'Bob');
    const carl = await join(await ctx(), 'Carl');
    // Bob's key is published once he's in; then he goes
    await bob.waitForTimeout(1200);
    await bob.close();

    const ana = await join(await ctx(), 'Ana');
    await ana.click('#rpMembers');
    await ana.waitForFunction(() => document.querySelector('#mem-off')?.textContent.includes('Offline — 1'), null, { timeout: 15000 });
    await ana.waitForFunction(() => document.querySelector('#mem-on')?.textContent.includes('Online — 2'), null, { timeout: 15000 });
    await ana.waitForSelector('#memList .drow.offline:has(.mname:text-is("Bob"))', { timeout: 15000 });
    if (shots) await ana.screenshot({ path: path.join(shots, 'dm-web-members.png') });
    // Folding, searching
    await ana.click('#mem-off');
    await ana.waitForFunction(() => !document.querySelector('#memList .drow.offline'), null, { timeout: 5000 });
    await ana.click('#mem-off');
    await ana.fill('#memSearch', 'car');
    await ana.waitForFunction(() => document.querySelectorAll('#memList .drow').length === 1 && document.querySelector('#memList .drow .mname').textContent === 'Carl', null, { timeout: 5000 });
    // Block Carl: he is gone from the list
    await ana.click('#memList .drow:has(.mname:text-is("Carl"))');
    await ana.click('#memBlock');
    await ana.waitForFunction(() => !document.querySelector('#memList .drow'), null, { timeout: 5000 });
    await ana.fill('#memSearch', '');
    await ana.evaluate(() => { const e = document.querySelector('#memSearch'); e.dispatchEvent(new Event('input')); });
    await ana.waitForSelector('#memList .drow.offline:has(.mname:text-is("Bob"))', { timeout: 5000 });
    await ana.click('#memList .drow.offline:has(.mname:text-is("Bob"))');
    await ana.waitForSelector('#volPop:not(.hidden) #memMessage');
    await ana.click('#memMessage');
    await ana.waitForFunction(() => document.querySelector('.lockbadge')?.textContent.includes('End-to-end encrypted'), null, { timeout: 15000 });
    await ana.waitForSelector('#dmOfflineNote', { timeout: 5000 });
    await ana.fill('#dmSay', secret);
    await ana.press('#dmSay', 'Enter');
    await ana.waitForFunction((s) => [...document.querySelectorAll('#dmList .msg.lock-sealed:not(.pending) .text')].some((e) => e.textContent === s), secret, { timeout: 15000 });
    console.log('SENT');
    await carl.waitForTimeout(100);

    // Bob is back: unread, decrypted preview, read when opened
    bob = await join(bobCtx, 'Bob');
    await bob.waitForFunction(() => document.querySelector('#tabDm')?.dataset.count === '1', null, { timeout: 15000 });
    await bob.click('#tabDm');
    await bob.waitForFunction((s) => {
      const r = document.querySelector('.dmrow.unread');
      return r && r.querySelector('.dmname').textContent === 'Ana' && r.querySelector('.dmprev').textContent === s && r.querySelector('.dmcount').textContent === '1';
    }, secret, { timeout: 15000 });
    if (shots) await bob.screenshot({ path: path.join(shots, 'dm-web-list.png') });
    await bob.click('.dmrow.unread');
    await bob.waitForFunction((s) => [...document.querySelectorAll('#dmList .msg.lock-sealed .text')].some((e) => e.textContent === s), secret, { timeout: 15000 });
    await bob.waitForFunction(() => !document.querySelector('#tabDm').dataset.count, null, { timeout: 5000 });
    await bob.waitForTimeout(500);
    await bob.reload({ waitUntil: 'load' });
    await bob.waitForSelector('#uidNote:not(:empty)', { timeout: 15000 });
    await bob.fill('#name', 'Bob');
    await bob.click('#go');
    await bob.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
    await bob.waitForTimeout(1500);
    const left = await bob.evaluate(() => document.querySelector('#tabDm').dataset.count);
    if (left) throw new Error('unread again after a reload: ' + left);
    console.log('OK');
  } catch (e) {
    console.error('members e2e failed:', e.message);
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();
