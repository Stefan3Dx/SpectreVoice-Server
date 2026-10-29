// Games between two browsers through a real server's bus, in headless Chromium; driven by TestWebE2EGames
// (webapp_games_test.go):
//
//	node web/e2e/games.js <server url> [screenshot dir]
//
// Ana opens a tic-tac-toe table, Bob sits down, Ana starts; they play it out by clicking the cells (whoever the
// keeper's shuffle made first wins on the top row) and both pages show the same state and the winner. Then a chess
// table: the Ruy Lopez to White castling (1. e4 e5 2. Nf3 Nc6 3. Bb5 a6 4. Ba4 Nf6 5. O-O), clicked square to square,
// the same board on both sides. Last, Ana spins the slot machine once and her purse on the server moves by what the
// spin says; and Ana, keeping a roulette with Bob's chip on it, leaves the server: the table closes and the chip
// comes back. Prints a line per step and OK at the end. Screenshots (with a folder): games-list.png, games-ttt.png,
// games-chess-white.png, games-chess-black.png, games-slots.png.

const { chromium } = require('playwright');
const path = require('path');

const [base, shots] = process.argv.slice(2);
if (!base) {
  console.error('usage: games.js <url> [dir]');
  process.exit(2);
}

async function join(browser, name) {
  const ctx = await browser.newContext({ locale: 'en-US', viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.error(name + ' page error:', e.message));
  page.on('console', (m) => { if (m.type() === 'error') console.error(name + ' console:', m.text()); });
  await page.goto(base + '/app/', { waitUntil: 'load' });
  await page.waitForSelector('#uidNote:not(:empty)', { timeout: 15000 });
  await page.fill('#name', name);
  await page.click('#go');
  await page.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
  await page.waitForFunction(() => window.svGames && window.svGames.bus.online() && 'lei' in window.svGames.bus.kept(), null, { timeout: 15000 });
  return page;
}

// The same value whatever the order of the keys (the server's relay writes them sorted)
const canon = (x) => (Array.isArray(x) ? x.map(canon) : x && typeof x === 'object' ? Object.fromEntries(Object.keys(x).sort().map((k) => [k, canon(x[k])])) : x);
const same = (a, b) => JSON.stringify(canon(a)) === JSON.stringify(canon(b));
const shot = async (page, name) => { if (shots) await page.screenshot({ path: path.join(shots, name) }); };
const uid = (page) => page.evaluate(() => window.svGames.hub.me().uid);
const table = (page, id) => page.evaluate((i) => { const t = window.svGames.hub.tables.get(i); return t ? JSON.parse(JSON.stringify(t)) : null; }, id);

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream', '--autoplay-policy=no-user-gesture-required'] });
  let ana, bob;
  try {
    ana = await join(browser, 'Ana');
    bob = await join(browser, 'Bob');
    const who = { [await uid(ana)]: ana, [await uid(bob)]: bob };
    console.log('CONNECTED');

    // ====== Tic-tac-toe ======
    await ana.click('#gamesBtn');
    await ana.waitForSelector('.dlg.games .gm-kind[data-kind="TICTACTOE"]');
    await ana.click('.gm-kind[data-kind="TICTACTOE"]');
    await ana.waitForSelector('.gm-tableview .gm-waiting');
    const ttt = await ana.evaluate(() => window.svGames.hub.open);
    await bob.click('#gamesBtn');
    await bob.waitForSelector(`.gm-table-row[data-table="${ttt}"] button`, { timeout: 20000 });
    await shot(bob, 'games-list.png');
    await bob.click(`.gm-table-row[data-table="${ttt}"] button`);
    await ana.waitForFunction((i) => window.svGames.hub.tables.get(i)?.players.length === 2, ttt, { timeout: 10000 });
    await ana.click('#gmStart');
    await bob.waitForFunction((i) => window.svGames.hub.tables.get(i)?.started, ttt, { timeout: 10000 });
    console.log('TTT STARTED');
    const order = (await table(ana, ttt)).players.map((p) => who[p.uid]);
    for (const [k, cell] of [0, 3, 1, 4, 2].entries()) {
      const page = order[k % 2];
      await page.click(`.gm-cell[data-cell="${cell}"]:not([disabled])`, { timeout: 10000 });
      for (const p of [ana, bob]) await p.waitForFunction(([i, c]) => window.svGames.hub.tables.get(i)?.state.cells[c] !== -1, [ttt, cell], { timeout: 10000 });
    }
    const a1 = await table(ana, ttt), b1 = await table(bob, ttt);
    if (!same(a1.state, b1.state)) throw new Error('tic-tac-toe states differ: ' + JSON.stringify(a1.state) + ' / ' + JSON.stringify(b1.state));
    if (a1.state.winner !== 0 || a1.state.final !== true || JSON.stringify(a1.state.line) !== '[2,1,0]') throw new Error('tic-tac-toe end: ' + JSON.stringify(a1.state));
    for (const p of [ana, bob]) await p.waitForFunction((n) => document.querySelector('.gm-banner')?.textContent === n + ' wins!', a1.players[0].name, { timeout: 5000 });
    await shot(order[0], 'games-ttt.png');
    console.log('TTT WON by ' + a1.players[0].name);

    // ====== Chess ======
    await ana.click('#gmBack');
    await ana.click('.gm-kind[data-kind="CHESS"]');
    await ana.waitForSelector('.gm-tableview .gm-waiting');
    const chess = await ana.evaluate(() => window.svGames.hub.open);
    await bob.click('#gmBack');
    await bob.waitForSelector(`.gm-table-row[data-table="${chess}"] button`, { timeout: 20000 });
    await bob.click(`.gm-table-row[data-table="${chess}"] button`);
    await ana.waitForFunction((i) => window.svGames.hub.tables.get(i)?.players.length === 2, chess, { timeout: 10000 });
    await ana.click('#gmStart');
    await bob.waitForFunction((i) => window.svGames.hub.tables.get(i)?.started, chess, { timeout: 10000 });
    const seats = (await table(ana, chess)).players.map((p) => who[p.uid]);
    console.log('CHESS STARTED');
    const opening = [[52, 36], [12, 28], [62, 45], [1, 18], [61, 25], [8, 16], [25, 32], [6, 21], [60, 62]];
    for (const [k, [from, to]] of opening.entries()) {
      const page = seats[k % 2];
      await page.click(`.gm-sq[data-sq="${from}"]:not([disabled])`, { timeout: 10000 });
      await page.click(`.gm-sq[data-sq="${to}"].to, .gm-sq[data-sq="${to}"].cap`, { timeout: 5000 });
      for (const p of [ana, bob]) await p.waitForFunction(([i, n]) => (window.svGames.hub.tables.get(i)?.state.hist || []).length === n, [chess, k + 1], { timeout: 10000 });
    }
    const a2 = await table(ana, chess), b2 = await table(bob, chess);
    if (!same(a2.state, b2.state) || !same(a2.players, b2.players)) throw new Error('chess states differ');
    if (a2.state.board[62] !== 'K' || a2.state.board[61] !== 'R' || a2.state.castling !== 'kq' || a2.state.white !== false) throw new Error('chess after castling: ' + JSON.stringify(a2.state));
    for (const p of [ana, bob]) await p.waitForFunction(() => [...document.querySelectorAll('.gm-moves span')].map((e) => e.textContent).join(' ') === 'e4 e5 Nf3 Nc6 Bb5 a6 Ba4 Nf6 O-O', null, { timeout: 5000 });
    await shot(seats[0], 'games-chess-white.png');
    await shot(seats[1], 'games-chess-black.png');
    console.log('CHESS CASTLED');

    // ====== Slots: the purse on the server moves by the spin ======
    await ana.click('#gmBack');
    // The day's bonus first (given 5 s in), so it doesn't land in the middle
    await ana.waitForFunction(() => { const h = window.svGames.hub; return !!window.svGames.bus.kept().lei?.[h.me().uid]?.d; }, null, { timeout: 20000 });
    const before = await ana.evaluate(() => window.svGames.hub.Lei.of(window.svGames.hub.me().uid));
    await ana.click('.gm-kind[data-kind="SLOTS"]');
    await ana.waitForSelector('#gmSpin');
    await ana.fill('.gm-stake', '25');
    await ana.click('#gmSpin');
    await ana.waitForFunction(() => { const t = window.svGames.hub.tables.get(window.svGames.hub.open); return t?.state.spins === 1; }, null, { timeout: 10000 });
    const last = await ana.evaluate(() => window.svGames.hub.tables.get(window.svGames.hub.open).state.last);
    const me = await uid(ana);
    // The server's own record (a put back from the bus), on Bob's page too
    for (const p of [ana, bob]) await p.waitForFunction(([u, v]) => window.svGames.bus.kept().lei?.[u]?.v === v, [me, before + last], { timeout: 10000 });
    await ana.waitForTimeout(1000);
    await shot(ana, 'games-slots.png');
    console.log(`SLOTS ${before} ${last >= 0 ? '+' : ''}${last}`);
    // Closing a table goes to everyone
    await ana.click('#gmLeave');
    await bob.waitForFunction(() => [...window.svGames.hub.tables.values()].every((t) => t.kind !== 'SLOTS'), null, { timeout: 10000 });
    // A keeper who leaves the server closes its tables: Bob's chips on Ana's roulette come back
    const bobUid = await uid(bob);
    await ana.click('.gm-kind[data-kind="ROULETTE"]');
    await ana.waitForSelector('.gm-tableview .gm-waiting');
    const ro = await ana.evaluate(() => window.svGames.hub.open);
    await bob.click('#gmBack');
    await bob.waitForSelector(`.gm-table-row[data-table="${ro}"] button`, { timeout: 20000 });
    await bob.click(`.gm-table-row[data-table="${ro}"] button`);
    await ana.waitForFunction((i) => window.svGames.hub.tables.get(i)?.players.length === 2, ro, { timeout: 10000 });
    await ana.click('#gmStart');
    const bobBefore = await bob.evaluate((u) => window.svGames.hub.Lei.of(u), bobUid);
    await bob.click('.gm-rc[data-bet="red:0"]:not([disabled])', { timeout: 10000 });
    await bob.waitForFunction(([u, v]) => window.svGames.bus.kept().lei?.[u]?.v === v, [bobUid, bobBefore - 10], { timeout: 10000 });
    await ana.keyboard.press('Escape');
    await ana.click('#leave');
    await bob.waitForFunction((i) => !window.svGames.hub.tables.has(i), ro, { timeout: 10000 });
    await bob.waitForFunction(([u, v]) => window.svGames.bus.kept().lei?.[u]?.v === v, [bobUid, bobBefore], { timeout: 10000 });
    console.log('REFUNDED on leaving');
    console.log('OK');
  } catch (e) {
    console.error('e2e failed:', e.message);
    if (shots) { try { await ana?.screenshot({ path: path.join(shots, 'games-fail-ana.png') }); await bob?.screenshot({ path: path.join(shots, 'games-fail-bob.png') }); } catch { /* closing */ } }
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();
