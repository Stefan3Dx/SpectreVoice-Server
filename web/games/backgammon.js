// Table (backgammon), as Backgammon.kt: 15 checkers each, two dice, doubles played four times, hitting a lone checker
// to the bar, coming in from the bar first, bearing off once all are home; as many dice as can be played (the larger
// when only one fits). A win where the other bore off none is a marți (2 points), with checkers still in the
// winner's home or on the bar a marți dublu (3); 3 points win the match.
// Points 0..23: white (seat 0) moves down and bears off below 0, black (seat 1) up past 23; +n white, −n black.

import { mustInt, intOr, ints, mustList, defaultRnd } from './util.js';

export const BAR = -1, OFF = -2, TARGET = 3;

export function fresh() {
  const p = Array(24).fill(0);
  p[23] = 2; p[12] = 5; p[7] = 3; p[5] = 5;
  p[0] = -2; p[11] = -5; p[16] = -3; p[18] = -5;
  return p;
}

export const start = (first) => ({ points: fresh(), bar: [0, 0], off: [0, 0], turn: first, dice: [], rolled: [], winner: -1, score: [0, 0], gameWinner: -1, lastPoints: 0, last: [] });

export const encode = (s) => ({
  points: s.points.slice(), bar: s.bar.slice(), off: s.off.slice(), turn: s.turn, dice: s.dice.slice(), rolled: s.rolled.slice(),
  winner: s.winner, score: s.score.slice(), gw: s.gameWinner, lp: s.lastPoints, last: s.last.map((x) => [x[0], x[1]]),
});

export function decode(m) {
  return {
    points: ints(m.points), bar: ints(m.bar), off: ints(m.off), turn: mustInt(m.turn), dice: ints(m.dice), rolled: ints(m.rolled),
    winner: mustInt(m.winner), score: ints(m.score), gameWinner: intOr(m.gw, -1), lastPoints: intOr(m.lp, 0),
    last: (Array.isArray(m.last) ? m.last : []).map((x) => { const l = mustList(x); return [mustInt(l[0]), mustInt(l[1])]; }),
  };
}

const sign = (p) => (p === 0 ? 1 : -1);
const mine = (s, p, i) => s.points[i] * sign(p) > 0;

function home(s, p) {
  if (s.bar[p] > 0) return false;
  for (let i = p === 0 ? 6 : 0; i <= (p === 0 ? 23 : 17); i++) if (mine(s, p, i)) return false;
  return true;
}

// Where a checker of p from `from` lands with die: a point, OFF, or null
export function target(s, p, from, die) {
  if (s.bar[p] > 0 && from !== BAR) return null;
  const to = from === BAR ? (p === 0 ? 24 - die : die - 1) : p === 0 ? from - die : from + die;
  if (to >= 0 && to <= 23) return s.points[to] * sign(p) >= -1 ? to : null;
  if (from === BAR || !home(s, p)) return null;
  const exact = p === 0 ? from - die === -1 : from + die === 24;
  if (exact) return OFF;
  let farther = false;
  if (p === 0) { for (let i = from + 1; i <= 5; i++) if (mine(s, p, i)) farther = true; } else { for (let i = 18; i < from; i++) if (mine(s, p, i)) farther = true; }
  return farther ? null : OFF;
}

function apply(s, p, from, to) {
  if (from === BAR) s.bar[p]--; else s.points[from] -= sign(p);
  if (to === OFF) { s.off[p]++; return; }
  if (s.points[to] === -sign(p)) { s.points[to] = 0; s.bar[1 - p]++; }
  s.points[to] += sign(p);
}

const copy = (s) => ({ ...s, points: s.points.slice(), bar: s.bar.slice(), off: s.off.slice(), dice: s.dice.slice(), score: s.score.slice() });
const without = (dice, d) => { const l = dice.slice(); const i = l.indexOf(d); if (i >= 0) l.splice(i, 1); return l; };

function steps(s, p, dice) {
  const froms = s.bar[p] > 0 ? [BAR] : [...Array(24).keys()].filter((i) => mine(s, p, i));
  const out = [];
  for (const d of [...new Set(dice)]) for (const f of froms) { const t = target(s, p, f, d); if (t !== null) out.push([f, t, d]); }
  return out;
}

function depth(s, p, dice) {
  if (!dice.length) return 0;
  let best = 0;
  for (const [f, t, d] of steps(s, p, dice)) {
    const c = copy(s); apply(c, p, f, t);
    best = Math.max(best, 1 + depth(c, p, without(dice, d)));
    if (best === dice.length) break;
  }
  return best;
}

// The moves allowed now: [from, to, die]
export function legal(s) {
  if (s.winner >= 0 || !s.dice.length) return [];
  const p = s.turn;
  const all = steps(s, p, s.dice);
  if (!all.length) return [];
  const max = depth(s, p, s.dice);
  let ok = all.filter(([f, t, d]) => { const c = copy(s); apply(c, p, f, t); return 1 + depth(c, p, without(s.dice, d)) === max; });
  if (max === 1 && s.dice.length === 2 && s.dice[0] !== s.dice[1]) {
    const big = Math.max(...ok.map((x) => x[2]));
    ok = ok.filter((x) => x[2] === big);
  }
  return ok;
}

const same = (a, b) => a.length === b.length && a.every((x, i) => x === b[i]);

// Player p: "roll", "move" from → to, "again" (the next game of the match). null when done, else why not
export function move(s, p, op, from, to, rnd = defaultRnd) {
  if (s.winner >= 0) return 'over';
  if (op === 'again') {
    if (s.gameWinner < 0) return 'not over';
    s.points = fresh(); s.bar = [0, 0]; s.off = [0, 0];
    s.dice = []; s.rolled = []; s.turn = s.gameWinner; s.gameWinner = -1; s.last = [];
    return null;
  }
  if (s.gameWinner >= 0) return 'over';
  if (s.turn !== p) return 'not your turn';
  if (op === 'roll') {
    if (s.dice.length) return 'rolled';
    const opening = s.off[0] + s.off[1] === 0 && s.bar[0] + s.bar[1] === 0 && same(s.points, fresh());
    const a = rnd.nextInt(1, 7);
    let b = rnd.nextInt(1, 7);
    while (opening && b === a) b = rnd.nextInt(1, 7);
    s.rolled = [a, b];
    s.dice = a === b ? [a, a, a, a] : [a, b];
    s.last = [];
    if (!legal(s).length) pass(s);
  } else if (op === 'move') {
    const m = legal(s).find((x) => x[0] === from && x[1] === to);
    if (!m) return 'not allowed';
    apply(s, p, from, to);
    s.dice = without(s.dice, m[2]);
    s.last = [...s.last, [from, to]];
    if (s.off[p] === 15) { win(s, p); return null; }
    if (!s.dice.length || !legal(s).length) pass(s);
  } else return 'what?';
  return null;
}

export const blocked = (s) => s.rolled.length > 0 && !s.dice.length && !s.last.length && s.gameWinner < 0;

function pass(s) { s.dice = []; s.turn = 1 - s.turn; }

function win(s, p) {
  const o = 1 - p;
  let pts;
  if (s.off[o] > 0) pts = 1;
  else {
    let inHome = false;
    for (let i = o === 0 ? 18 : 0; i <= (o === 0 ? 23 : 5); i++) if (s.points[i] * sign(o) > 0) inHome = true;
    pts = s.bar[o] > 0 || inHome ? 3 : 2;
  }
  s.score[p] += pts;
  s.lastPoints = pts;
  s.gameWinner = p;
  s.dice = [];
  if (s.score[p] >= TARGET) s.winner = p;
}

export function pips(s, p) {
  let n = s.bar[p] * 25;
  for (let i = 0; i < 24; i++) n += Math.max(0, s.points[i] * sign(p)) * (p === 0 ? i + 1 : 24 - i);
  return n;
}

export const Backgammon = { BAR, OFF, TARGET, fresh, start, encode, decode, target, legal, move, blocked, pips };
