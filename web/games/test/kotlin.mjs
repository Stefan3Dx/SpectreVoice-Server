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
import { Trivia, Hangman } from '../party.js';
import { HANGMAN } from '../partydata.js';

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
  if (o && kind === 'BACKGAMMON' && step.m.op === 'roll') queue.push(...o.state.rolled);
  // A new hangman word: its place among the words not used yet
  if (o && kind === 'HANGMAN' && o.secret.used.length > (step.in.secret.used || []).length) queue.push(hangmanPick(step.in.secret.used || [], o.secret.used.at(-1)));
  return { nextInt: () => { if (!queue.length) throw new Error('the web rules drew more than the Kotlin did'); return queue.shift(); } };
}

function hangmanPick(used, word) {
  let pool = HANGMAN.filter((x) => !used.includes(x[0]));
  if (!pool.length) pool = HANGMAN;
  return pool.findIndex((x) => x[0] === word);
}

// Blackjack shuffles a new shoe when it runs low, poker a new deck each hand
const DECK = { BLACKJACK: 'shoe', POKER: 'deck' };
function shuffledHere(kind, step) {
  if (!DECK[kind] || !step.out?.secret || !step.in.secret) return false;
  const a = step.in.secret[DECK[kind]], b = step.out.secret[DECK[kind]];
  return b.length > a.length || b.some((c, i) => c !== a[i]);
}

// The answers that make the web's shuffle (Fisher–Yates from the end, util.shuffled) give the deck `target`
function shuffleAnswers(target) {
  const a = [...Array(target.length).keys()];
  const out = [];
  for (let i = a.length - 1; i > 0; i--) {
    const j = a.indexOf(target[i]);
    out.push(j);
    [a[i], a[j]] = [a[j], a[i]];
  }
  return out;
}

// A poker deal: the whole deck as the Kotlin shuffled it, from what's left and the cards dealt off its end (two
// rounds from the button's left, then any board run out)
function pokerDeck(sec) {
  const n = sec.hands.length;
  const popped = [];
  for (let r = 0; r < 2; r++) for (let k = 1; k <= n; k++) { const i = (sec.button + k) % n; if (!sec.out[i] && sec.hands[i][r] !== undefined) popped.push(sec.hands[i][r]); }
  popped.push(...sec.board);
  return [...sec.deck, ...popped.reverse()];
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
    const deck = DECK[g.kind];
    if (deck && o.secret && o.secret[deck] && !Array.isArray(o.secret[deck])) o.secret[deck] = prev.secret[deck].slice(0, o.secret[deck].keep);
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
  // (backgammon: who starts as the Kotlin drew it; the cards: only how many, they're shuffled; trivia's questions in
  // the Kotlin's order, hangman's first word and player as it drew them)
  let s;
  if (g.kind === 'TRIVIA') {
    const st0 = g.start.state;
    const r = Trivia.round({ rd: 0, of: g.start.secret.order.length, sc: Array(g.n).fill(0) }, { order: g.start.secret.order.slice() }, g.n, st0.start);
    s = { state: r.pub, secret: r.sec };
  } else if (g.kind === 'HANGMAN') {
    const feed = [g.start.state.turn, hangmanPick([], g.start.secret.used[0])];
    s = R.start(g.kind, ctx(true), null, { nextInt: () => feed.shift() }, g.start.state.dl - Hangman.TURN_MS);
  } else s = R.start(g.kind, ctx(true), null, { nextInt: () => g.start.state.turn });
  if (R.isParty(g.kind)) {
    if (!same(s.state, g.start.state) || !same(s.secret, g.start.secret)) bad.push(`${g.kind} start: ${JSON.stringify(s)} vs ${JSON.stringify(g.start)}`);
  } else if (DECK[g.kind]) {
    if (!same(s.state, g.start.state) || s.secret[DECK[g.kind]].length !== g.start.secret[DECK[g.kind]].length) bad.push(`${g.kind} start: ${JSON.stringify(s.state)} vs ${JSON.stringify(g.start.state)}`);
    for (const f of ['pay', 'profit', 'over', 'played', 'final']) if (!same(s[f] ?? null, g.start[f] ?? null)) bad.push(`${g.kind} start ${f}: ${JSON.stringify(s[f])} vs ${JSON.stringify(g.start[f])}`);
  } else if (!same(s.state, g.start.state)) bad.push(`${g.kind} start: ${JSON.stringify(s.state)} vs ${JSON.stringify(g.start.state)}`);
  if (!R.validState(g.kind, g.start.state, g.n)) bad.push(`${g.kind} start state refused`);
  for (const [k, step] of g.steps.entries()) {
    moves++;
    let rnd = rndFor(g.kind, step);
    if (shuffledHere(g.kind, step)) {
      // Poker deals a new hand from a deck the Kotlin shuffled: the web shuffle is fed the same order. A blackjack
      // shoe (six decks, cards alike) can't be told apart that way: skipped, the next steps go on from the Kotlin's
      const sec = step.out.secret;
      if (g.kind !== 'POKER' || sec.deck.length + sec.board.length + sec.hands.reduce((a, h) => a + h.length, 0) !== 52) { skipped++; continue; }
      const answers = shuffleAnswers(pokerDeck(sec));
      rnd = { nextInt: () => { if (!answers.length) throw new Error('shuffled twice'); return answers.shift(); } };
    }
    let o;
    try { o = R.move(g.kind, step.in.state, step.in.secret, step.p, step.m, ctx(step.host), rnd, step.now); } catch (e) { o = 'threw ' + e.message; }
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
console.log(`OK ${games} games, ${moves} moves (${taken} taken, the rest refused by both; ${skipped} blackjack reshuffles skipped)`);
