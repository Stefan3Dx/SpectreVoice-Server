// Texas Hold'em, no limit, for Lei, as Poker.kt: blinds, two cards each, flop / turn / river, betting rounds, side
// pots when someone is all in, the best five of seven at the showdown. Each hand is settled on its own; a player
// plays with what's in their purse at the start of the hand. The deck and the hands stay in the keeper's secret;
// each player gets their own cards privately.

import { mustInt, intOr, ints, bools, mustList, defaultRnd, shuffled } from './util.js';

export const SMALL = 10, BIG = 20;

export const rank = (card) => (card % 13) + 1;
export const suit = (card) => Math.floor(card / 13);
const value = (card) => (rank(card) === 1 ? 14 : rank(card));

export function encode(s) {
  return {
    deck: s.deck.slice(), hands: s.hands.map((h) => h.slice()), board: s.board.slice(), stack: s.stack.slice(), bet: s.bet.slice(), total: s.total.slice(),
    folded: s.folded.slice(), acted: s.acted.slice(), out: s.out.slice(), button: s.button, turn: s.turn, phase: s.phase,
    current: s.current, minRaise: s.minRaise, hand: s.hand, result: s.result.slice(), shown: s.shown.slice(), best: s.best.slice(),
    note: s.note, small: s.small, big: s.big, left: s.left.slice(),
  };
}

export function decode(m) {
  const hands = mustList(m.hands).map(ints);
  if (typeof m.phase !== 'string') throw new TypeError('phase');
  return {
    deck: ints(m.deck), hands, board: ints(m.board), stack: ints(m.stack), bet: ints(m.bet), total: ints(m.total),
    folded: bools(m.folded), acted: bools(m.acted), out: bools(m.out), button: mustInt(m.button), turn: mustInt(m.turn), phase: m.phase,
    current: mustInt(m.current), minRaise: mustInt(m.minRaise), hand: mustInt(m.hand), result: ints(m.result), shown: bools(m.shown),
    best: mustList(m.best).map((x) => (typeof x === 'string' ? x : '')), note: typeof m.note === 'string' ? m.note : '',
    small: intOr(m.small, SMALL), big: intOr(m.big, BIG), left: Array.isArray(m.left) ? bools(m.left) : Array(hands.length).fill(false),
  };
}

// What everyone may see: no deck, the cards in hand only once shown (yours: me)
export function publicState(s, me = -1) {
  return { ...encode(s), deck: [], hands: s.hands.map((h, i) => (i === me || s.shown[i] ? h.slice() : h.map(() => -1))) };
}

export function start(players, purses, button = -1, small = SMALL, big = BIG, rnd = defaultRnd) {
  const n = players;
  const s = {
    deck: [], hands: Array.from({ length: n }, () => []), board: [], stack: purses.slice(), bet: Array(n).fill(0), total: Array(n).fill(0),
    folded: Array(n).fill(false), acted: Array(n).fill(false), out: Array(n).fill(false), button, turn: -1, phase: 'done', current: 0, minRaise: big, hand: 0,
    result: Array(n).fill(0), shown: Array(n).fill(false), best: Array(n).fill(''), note: '', small, big, left: Array(n).fill(false),
  };
  deal(s, purses, rnd);
  return s;
}

function next(s, from, ok) {
  const n = s.hands.length;
  for (let k = 1; k <= n; k++) { const i = (((from + k) % n) + n) % n; if (ok(i)) return i; }
  return -1;
}
const inHand = (s, i) => !s.out[i] && !s.folded[i];
const canAct = (s, i) => inHand(s, i) && s.stack[i] > 0;
const idx = (s) => [...s.hands.keys()];

// A new hand: the button moves on, blinds go in, two cards each
export function deal(s, purses, rnd = defaultRnd) {
  const n = s.hands.length;
  s.deck = shuffled([...Array(52).keys()], rnd);
  s.board = [];
  for (let i = 0; i < n; i++) {
    s.hands[i] = []; s.stack[i] = purses[i]; s.bet[i] = 0; s.total[i] = 0; s.folded[i] = false; s.acted[i] = false;
    s.out[i] = s.left[i] || purses[i] < s.big; s.result[i] = 0; s.shown[i] = false; s.best[i] = '';
  }
  s.note = '';
  s.hand++;
  if (s.out.filter((x) => !x).length < 2) { s.phase = 'done'; s.note = 'Prea puțini jucători cu bani'; return; }
  s.button = next(s, s.button, (i) => !s.out[i]);
  const heads = s.out.filter((x) => !x).length === 2;
  const sb = heads ? s.button : next(s, s.button, (i) => !s.out[i]);
  const bb = next(s, sb, (i) => !s.out[i]);
  put(s, sb, Math.min(s.small, s.stack[sb]));
  put(s, bb, Math.min(s.big, s.stack[bb]));
  s.current = s.big;
  s.minRaise = s.big;
  for (let r = 0; r < 2; r++) for (let k = 1; k <= n; k++) { const i = (s.button + k) % n; if (!s.out[i]) s.hands[i].push(s.deck.pop()); }
  s.phase = 'preflop';
  s.turn = next(s, bb, (i) => canAct(s, i));
  if (s.turn < 0 || everyoneAllIn(s)) runOut(s);
}

function put(s, i, amount) {
  const a = Math.min(amount, s.stack[i]);
  s.stack[i] -= a; s.bet[i] += a; s.total[i] += a;
}

function everyoneAllIn(s) {
  const can = idx(s).filter((i) => canAct(s, i));
  return can.length <= 1 && can.every((i) => s.bet[i] >= s.current);
}

// Player p: "fold", "check", "call", "raise" (to `to` in this round), "allin", "leave". null when done, else why not
export function move(s, p, op, to) {
  if (op === 'leave') {
    s.left[p] = true;
    if (s.phase !== 'done' && inHand(s, p)) {
      s.folded[p] = true;
      s.acted[p] = true;
      if (idx(s).filter((i) => inHand(s, i)).length <= 1) finish(s, false);
      else if (s.turn === p) advance(s, p);
    }
    return null;
  }
  if (s.phase === 'done' || s.turn !== p) return 'not your turn';
  const owe = s.current - s.bet[p];
  switch (op) {
    case 'fold': s.folded[p] = true; break;
    case 'check': if (owe > 0) return "can't check"; break;
    case 'call': if (owe <= 0) return 'nothing to call'; put(s, p, owe); break;
    case 'raise': case 'allin': {
      const most = s.bet[p] + s.stack[p];
      const target = op === 'allin' ? most : Math.min(to, most);
      if (target <= s.current) {
        if (op !== 'allin') return 'raise more';
        put(s, p, target - s.bet[p]);
        s.acted[p] = true;
        advance(s, p);
        return null;
      }
      if (s.acted[p]) return "can't raise";
      const raise = target - s.current;
      const all = target >= most;
      if (raise < s.minRaise && !all) return 'min raise ' + (s.current + s.minRaise);
      put(s, p, target - s.bet[p]);
      s.current = Math.max(s.current, s.bet[p]);
      if (raise >= s.minRaise) {
        s.minRaise = raise;
        for (let i = 0; i < s.hands.length; i++) if (i !== p) s.acted[i] = false;
      }
      break;
    }
    default: return 'what?';
  }
  s.acted[p] = true;
  advance(s, p);
  return null;
}

function advance(s, from) {
  const live = idx(s).filter((i) => inHand(s, i));
  if (live.length === 1) { finish(s, false); return; }
  const waiting = idx(s).filter((i) => canAct(s, i) && (!s.acted[i] || s.bet[i] < s.current));
  if (waiting.length) { s.turn = next(s, from, (i) => waiting.includes(i)); return; }
  if (idx(s).filter((i) => canAct(s, i)).length <= 1) { runOut(s); return; }
  street(s);
}

function street(s) {
  for (let i = 0; i < s.hands.length; i++) { s.bet[i] = 0; s.acted[i] = false; }
  s.current = 0;
  s.minRaise = s.big;
  switch (s.phase) {
    case 'preflop': s.phase = 'flop'; for (let k = 0; k < 3; k++) s.board.push(s.deck.pop()); break;
    case 'flop': s.phase = 'turn'; s.board.push(s.deck.pop()); break;
    case 'turn': s.phase = 'river'; s.board.push(s.deck.pop()); break;
    default: finish(s, true); return;
  }
  s.turn = next(s, s.button, (i) => canAct(s, i));
  if (s.turn < 0) runOut(s);
}

function runOut(s) {
  while (s.board.length < 5) s.board.push(s.deck.pop());
  for (let i = 0; i < s.hands.length; i++) s.bet[i] = 0;
  finish(s, true);
}

function finish(s, showdown) {
  const n = s.hands.length;
  const won = Array(n).fill(0);
  const live = [...Array(n).keys()].filter((i) => inHand(s, i));
  if (!showdown || live.length === 1) {
    won[live[0]] = s.total.reduce((a, b) => a + b, 0);
    s.note = 'Ceilalți au renunțat';
  } else {
    for (const i of live) { s.shown[i] = true; s.best[i] = handName(best([...s.hands[i], ...s.board])); }
    const scores = Array.from({ length: n }, (_, i) => (live.includes(i) ? best([...s.hands[i], ...s.board]) : -1));
    const levels = [...new Set(s.total.filter((x) => x > 0))].sort((a, b) => a - b);
    let prev = 0;
    for (const lv of levels) {
      let pot = 0;
      for (let i = 0; i < n; i++) pot += Math.min(s.total[i], lv) - Math.min(s.total[i], prev);
      const eligible = live.filter((i) => s.total[i] >= lv);
      if (pot > 0 && eligible.length) {
        const top = Math.max(...eligible.map((i) => scores[i]));
        const winners = eligible.filter((i) => scores[i] === top).sort((a, b) => ((a - s.button - 1 + n) % n + n) % n - ((b - s.button - 1 + n) % n + n) % n);
        const share = Math.floor(pot / winners.length);
        for (const w of winners) won[w] += share;
        won[winners[0]] += pot - share * winners.length;
      } else if (pot > 0) {
        for (let i = 0; i < n; i++) won[i] += Math.min(s.total[i], lv) - Math.min(s.total[i], prev);
      }
      prev = lv;
    }
    const w = [...Array(n).keys()].filter((i) => won[i] > s.total[i]);
    s.note = w.length ? w.map((i) => s.best[i]).join(', ') : 'Egalitate';
  }
  for (let i = 0; i < n; i++) s.result[i] = won[i] - s.total[i];
  s.phase = 'done';
  s.turn = -1;
}

// ====== Hands ======

export const NAMES = ['Carte mare', 'Pereche', 'Două perechi', 'Trei de un fel', 'Chintă', 'Culoare', 'Full house', 'Careu', 'Chintă de culoare'];
export const handName = (score) => NAMES[Math.floor(score / 15 ** 5)];

// The best five of cards: higher is better
export function best(cards) {
  let top = -1;
  const n = cards.length;
  for (let a = 0; a < n; a++) for (let b = a + 1; b < n; b++) {
    const f = n <= 5 ? cards : cards.filter((_, i) => i !== a && i !== b);
    top = Math.max(top, five(f));
    if (n <= 5) return top;
  }
  return top;
}

function five(c) {
  const v = c.map(value).sort((x, y) => y - x);
  const flush = new Set(c.map(suit)).size === 1;
  const distinct = [...new Set(v)];
  const wheel = distinct.join(',') === '14,5,4,3,2';
  const straight = distinct.length === 5 && (v[0] - v[4] === 4 || wheel);
  const high = wheel ? 5 : v[0];
  const counts = new Map();
  for (const x of v) counts.set(x, (counts.get(x) || 0) + 1);
  const groups = [...counts.entries()].sort((a, b) => b[1] - a[1] || b[0] - a[0]);
  const order = groups.flatMap(([k, c2]) => Array(c2).fill(k));
  const score = (cat, ks) => { const t = ks.slice(0, 5); let x = t.reduce((acc, k) => acc * 15 + k, cat); for (let i = t.length; i < 5; i++) x *= 15; return x; };
  if (straight && flush) return score(8, [high]);
  if (groups[0][1] === 4) return score(7, order);
  if (groups[0][1] === 3 && groups[1][1] === 2) return score(6, order);
  if (flush) return score(5, v);
  if (straight) return score(4, [high]);
  if (groups[0][1] === 3) return score(3, order);
  if (groups[0][1] === 2 && groups[1][1] === 2) return score(2, order);
  if (groups[0][1] === 2) return score(1, order);
  return score(0, v);
}

export const Poker = { SMALL, BIG, rank, suit, encode, decode, publicState, start, deal, move, best, handName, NAMES };
