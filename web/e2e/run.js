// End-to-end check of the web client in headless Chromium (Playwright), driven by TestWebE2E (webapp_test.go):
//
//	node web/e2e/run.js <server url> <text the Go client will say>
//
// It opens /app/, connects as "Web" (a key made in the page), prints CONNECTED once the channel list is rendered,
// waits for the voice connection (rtc connected) and for the Go client's message in the chat, says "hi from web",
// holds Space for a moment (push-to-talk), prints OK and exits 0. Anything else: a message on stderr, exit 1.
// Needs: node, playwright (NODE_PATH to its global install if it isn't local) and its Chromium.

const { chromium } = require('playwright');

const [base, text] = process.argv.slice(2);
if (!base || !text) {
  console.error('usage: run.js <url> <text>');
  process.exit(2);
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    args: ['--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
  });
  const ctx = await browser.newContext({ permissions: ['microphone'], locale: 'en-US' });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.error('page error:', e.message));
  page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') console.error('console:', m.text()); });
  try {
    await page.goto(base + '/app/', { waitUntil: 'load' });
    await page.waitForSelector('#uidNote:not(:empty)', { timeout: 15000 });
    await page.fill('#name', 'Web');
    await page.click('#go');
    await page.waitForSelector('#app:not(.hidden) .tree .ch', { timeout: 15000 });
    const channels = await page.$$eval('.tree .ch', (els) => els.map((e) => e.firstChild.textContent));
    if (!channels.some((c) => c.includes('Lobby'))) throw new Error('no Lobby in ' + JSON.stringify(channels));
    const status = await page.textContent('#status');
    console.log('CONNECTED ' + JSON.stringify({ channels, status }));

    await page.waitForSelector('body[data-rtc="connected"]', { timeout: 20000 });
    console.log('RTC connected');

    await page.waitForFunction((want) => [...document.querySelectorAll('.msg .text')].some((e) => e.textContent === want), text, { timeout: 15000 });
    console.log('SEEN ' + text);

    await page.fill('#say', 'hi from web');
    await page.press('#say', 'Enter');
    await page.waitForFunction(() => [...document.querySelectorAll('.msg:not(.pending) .text')].some((e) => e.textContent === 'hi from web'), null, { timeout: 10000 });
    console.log('SAID');

    // Max volume: the voices' limiter keeps a full-scale tone at the ceiling (-9 dB ≈ 0.355) and leaves a quiet one as it was
    const lim = await page.evaluate(async () => {
      const m = await import(new URL('voice.js', document.baseURI).href);
      const run = async (amp) => {
        const ctx = new OfflineAudioContext(1, 48000, 48000);
        const o = ctx.createOscillator();
        o.frequency.value = 440;
        const g = ctx.createGain();
        g.gain.value = amp;
        const l = m.limiterChain(ctx, -9);
        o.connect(g); g.connect(l.input); l.output.connect(ctx.destination); o.start();
        const d = (await ctx.startRendering()).getChannelData(0);
        let peak = 0;
        for (let i = 24000; i < d.length; i++) peak = Math.max(peak, Math.abs(d[i]));
        return peak;
      };
      return { loud: await run(1), quiet: await run(0.1) };
    });
    if (!(lim.loud <= 0.37 && lim.loud > 0.3)) throw new Error('limiter: a full-scale tone came out at ' + lim.loud);
    if (Math.abs(lim.quiet - 0.1) > 0.01) throw new Error('limiter: a quiet tone changed to ' + lim.quiet);
    console.log('LIMITER ' + JSON.stringify(lim));

    // A voice through the limiter is heard also while the context isn't running (Safari before a click): its element
    // plays then, and the limiter again once the context runs
    const route = await page.evaluate(async () => {
      const m = await import(new URL('voice.js', document.baseURI).href);
      const gen = new AudioContext();
      const o = gen.createOscillator();
      const d = gen.createMediaStreamDestination();
      o.connect(d); o.start();
      const e = m._testVoice(d.stream);
      const ctx = m.A.ctx;
      const look = () => ({ state: ctx.state, graph: !!e.graph, elMuted: e.el.muted, gain: e.graph ? e.graph.gain.gain.value : -1 });
      await ctx.resume();
      const running = look();
      await ctx.suspend();
      await new Promise((r) => setTimeout(r, 100));
      const suspended = look();
      await ctx.resume();
      await new Promise((r) => setTimeout(r, 100));
      const back = look();
      e.done(); o.stop(); gen.close();
      return { running, suspended, back };
    });
    if (!route.running.graph || !route.running.elMuted || route.running.gain !== 1) throw new Error('limiter route: ' + JSON.stringify(route));
    if (route.suspended.elMuted || route.suspended.gain !== 0) throw new Error('suspended, the voice is not on its element: ' + JSON.stringify(route));
    if (!route.back.elMuted || route.back.gain !== 1) throw new Error('running again, the voice is not on the limiter: ' + JSON.stringify(route));
    console.log('ROUTE ' + JSON.stringify(route));

    await page.click('body');
    await page.keyboard.down('Space');
    const live = await page.$eval('#ptt', (e) => e.classList.contains('live'));
    if (!live) throw new Error('push-to-talk did not go live');
    await page.waitForTimeout(2500);
    await page.keyboard.up('Space');
    console.log('OK');
  } catch (e) {
    console.error('e2e failed:', e.message);
    console.error('rtc state:', await page.evaluate(() => document.body.dataset.rtc).catch(() => '?'));
    process.exitCode = 1;
  } finally {
    await browser.close();
  }
})();
