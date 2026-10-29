// Replays a recording of the apps' own Kotlin rules (kotlin-games.json.gz, made by KotlinOracle.java: the apps'
// compiled Rules.kt played with seeded random moves, written with the apps' Json.write, packed by pack.py) through
// the web rules: every move must be taken or refused the same, with the same state, secret, Lei, winners and end. The dice, the wheel and the reels
// are read from the Kotlin outcome (Kotlin drew them), so the web rules are fed the same numbers.
//
//	node web/games/test/kotlin.mjs [recording.json[.gz]]      prints "OK <games> games, <moves> moves"

import { readFileSync } from 'node:fs';
import { gunzipSync } from 'node:zlib';
import { fileURLToPath } from 'node:url';
import * as R from '../rules.js';

const file = process.argv[2] || fileURLToPath(new URL('./kotlin-games.json.gz', import.meta.url));
const raw = readFileSync(file);
const rec = JSON.parse((file.endsWith('.gz') ? gunzipSync(raw) : raw).toString('utf8'));

// The same value whatever the order of an object's keys
const canon = (x) => (Array.isArray(x) ? x.map(canon) : x && typeof x === 'object' ? Object.fromEntries(Object.keys(x).sort().map((k) => [k, canon(x[k])])) : x);
const same = (a, b) => JSON.stringify(canon(a)) === JSON.stringify(canon(b));
const WEIGHTS = [300, 250, 200, 120, 70, 40, 15, 5];

function rndFor(kind, step) {
  const o = step.out;
  const queue = [];
  if (o && kind === 'BARBUT' && o.state.rolls?.[String(step.p)]) queue.push(...o.state.rolls[String(step.p)]);
  if (o && kind === 'ROULETTE') queue.push(o.state.result);
  if (o && kind === 'SLOTS') for (const r of o.state.reels) queue.push(WEIGHTS.slice(0, r).reduce((a, b) => a + b, 0));
  return { nextInt: () => { if (!queue.length) throw new Error('the web rules drew more than the Kotlin did'); return queue.shift(); } };
}

// Blackjack shuffles a new shoe when it runs low: the Kotlin's own draw can't be replayed then (skipped)
function reshuffled(kind, step) {
  if (kind !== 'BLACKJACK' || !step.out?.secret || !step.in.secret) return false;
  const a = step.in.secret.shoe, b = step.out.secret.shoe;
  return b.length > a.length || b.some((c, i) => c !== a[i]);
}

// The recording is packed (pack.py): each step's "in" is the last state taken, a chess history only its new move,
// a blackjack shoe only how many cards are left of the one before. Unpacked here as it's replayed.
function unpack(g) {
  let prev = { state: g.start.state, secret: g.start.secret };
  for (const s of g.steps) {
    s.in = prev;
    const o = s.out;
    if (!o) continue;
    if (o.state.hist && !Array.isArray(o.state.hist)) o.state.hist = [...prev.state.hist, o.state.hist.push];
    if (o.secret && o.secret.shoe && !Array.isArray(o.secret.shoe)) o.secret.shoe = prev.secret.shoe.slice(0, o.secret.shoe.keep);
    prev = { state: o.final ? { ...o.state, final: true } : o.state, secret: o.secret ?? prev.secret };
  }
}

let games = 0, moves = 0, skipped = 0, taken = 0;
const bad = [];
for (const g of rec.games) {
  games++;
  unpack(g);
  const players = Array.from({ length: g.n }, (_, i) => ({ uid: 'u' + i, name: 'P' + i }));
  const ctx = (host) => ({ players, host, purse: (i) => g.purses[i] });
  // The start: the same (the shuffled shoe aside)
  const s = R.start(g.kind, ctx(true), null);
  if (g.kind === 'BLACKJACK') { if (!same({ ...s.state }, { ...g.start.state }) || s.secret.shoe.length !== g.start.secret.shoe.length) bad.push(`${g.kind} start`); }
  else if (!same(s.state, g.start.state)) bad.push(`${g.kind} start: ${JSON.stringify(s.state)} vs ${JSON.stringify(g.start.state)}`);
  if (!R.validState(g.kind, g.start.state, g.n)) bad.push(`${g.kind} start state refused`);
  for (const [k, step] of g.steps.entries()) {
    moves++;
    if (reshuffled(g.kind, step)) { skipped++; continue; }
    let o;
    try { o = R.move(g.kind, step.in.state, step.in.secret, step.p, step.m, ctx(step.host), rndFor(g.kind, step)); } catch (e) { o = 'threw ' + e.message; }
    const where = `${g.kind} game ${games} step ${k} seat ${step.p} ${JSON.stringify(step.m)}`;
    if (typeof o === 'string') { bad.push(where + ': ' + o); continue; }
    if (step.out === null) { if (o !== null) bad.push(where + ': taken here, refused by the Kotlin'); continue; }
    if (o === null) { bad.push(where + ': refused here, taken by the Kotlin'); continue; }
    taken++;
    for (const f of ['state', 'secret', 'pay', 'profit', 'over', 'played', 'final']) {
      if (!same(o[f] ?? null, step.out[f] ?? null)) bad.push(`${where}: ${f} ${JSON.stringify(o[f]).slice(0, 400)} vs Kotlin ${JSON.stringify(step.out[f]).slice(0, 400)}`);
    }
    const st = step.out.final ? { ...step.out.state, final: true } : step.out.state;
    if (!R.validState(g.kind, st, g.n)) bad.push(where + ': the Kotlin state is refused by the web check');
    for (const seat of R.waitingOn(g.kind, st, step.out.secret ?? step.in.secret)) if (seat < 0 || seat >= g.n) bad.push(where + ': waiting on ' + seat);
  }
}
for (const f of rec.forfeits) {
  const o = R.forfeit(f.kind, f.in, f.seat, 2, 'resign');
  for (const k of ['state', 'over', 'final']) if (!same(o[k], f.out[k])) bad.push(`forfeit ${f.kind} ${f.seat}: ${k} ${JSON.stringify(o[k])} vs ${JSON.stringify(f.out[k])}`);
}
if (bad.length) {
  for (const b of bad.slice(0, 40)) console.error('DIFF ' + b);
  console.error(`${bad.length} differences`);
  process.exit(1);
}
console.log(`OK ${games} games, ${moves} moves (${taken} taken, the rest refused by both; ${skipped} reshuffles skipped)`);
