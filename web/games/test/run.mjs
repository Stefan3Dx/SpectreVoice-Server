// The web games' rules against cases taken from the apps' Kotlin rules and tests (GamesTest, GameFixesTest,
// MoreGamesTest, ReviewFixesTest): node web/games/test/run.mjs (TestWebGamesRules runs it). Prints "OK n" at the end.

import assert from 'node:assert/strict';
import { seeded } from '../util.js';
import * as Grid from '../grid.js';
import { Chess, positions, sans, fromFen } from '../chess.js';
import { Barbut, Roulette, Slots } from '../casino.js';
import { Blackjack } from '../blackjack.js';
import { Backgammon } from '../backgammon.js';
import { Poker } from '../poker.js';
import * as R from '../rules.js';
import { createHub, fmtLei, isoWeek } from '../hub.js';

let n = 0;
const failures = [];
function test(name, f) {
  try { f(); n++; } catch (e) { failures.push(name + ': ' + (e.stack || e)); }
}
const clone = (x) => JSON.parse(JSON.stringify(x));
const two = [{ uid: 'a', name: 'A' }, { uid: 'b', name: 'B' }];
const ctx = (players = two, purse = 1000, host = false) => ({ players, host, purse: () => purse });
const sq = (s) => (8 - (+s[1])) * 8 + (s.charCodeAt(0) - 97);

// ====== Grid ======

test('grid: connect four and tic-tac-toe (GamesTest.barbutAndGrids)', () => {
  const g = Grid.connectFour();
  for (const m of [0, 1, 0, 1, 0, 1]) assert.equal(Grid.play(g, g.turn, m), null);
  assert.equal(Grid.play(g, 0, 0), null);
  assert.equal(g.winner, 0);
  const x = Grid.ticTacToe();
  for (const m of [0, 3, 1, 4, 2]) Grid.play(x, x.turn, m);
  assert.equal(x.winner, 0);
  // The exact state a Kotlin keeper sends: the turn stays with the winner, the line from the last cell out
  assert.deepEqual(Grid.encode(x), { cols: 3, rows: 3, need: 3, cells: [0, 0, 0, 1, 1, -1, -1, -1, -1], turn: 0, winner: 0, line: [2, 1, 0], gravity: false });
});

test('grid: moves are checked (GameFixesTest.gridMovesAreChecked)', () => {
  const s = Grid.connectFour();
  assert.equal(Grid.play(s, 0, -1), 'column');
  assert.equal(Grid.play(s, 0, 7), 'column');
  assert.equal(Grid.play(s, 0, 50), 'column');
  assert.ok(s.cells.every((c) => c === -1));
  assert.equal(Grid.play(s, 0, 3), null);
  assert.equal(s.cells[5 * 7 + 3], 0);
  assert.equal(Grid.play(s, 0, 3), 'not your turn');
  const st = Grid.encode(Grid.ticTacToe());
  assert.equal(R.move('TICTACTOE', st, null, 0, { cell: 9 }, ctx()), null);
  assert.equal(R.move('TICTACTOE', st, null, 0, { cell: -1 }, ctx()), null);
  assert.equal(R.move('TICTACTOE', st, null, 1, { cell: 4 }, ctx()), null);
  assert.notEqual(R.move('TICTACTOE', st, null, 0, { cell: 4 }, ctx()), null);
  // A full board without a line is a draw (-2), over with no winners
  let s2 = Grid.encode(Grid.ticTacToe());
  let o;
  for (const c of [0, 1, 2, 4, 3, 5, 7, 6, 8]) { o = R.move('TICTACTOE', s2, null, s2.turn, { cell: c }, ctx()); s2 = o.state; }
  assert.equal(s2.winner, -2);
  assert.deepEqual(o.over, []);
  assert.equal(o.final, true);
  // A diagonal in four in a row: the line as the Kotlin walks it
  const d = Grid.connectFour();
  for (const c of [0, 1, 1, 2, 2, 3, 2, 3, 3, 6, 3]) assert.equal(Grid.play(d, d.turn, c), null);
  assert.equal(d.winner, 0);
  assert.deepEqual(d.line, [17, 23, 29, 35]);
});

// ====== Chess ======

function perft(c, depth) {
  if (depth === 0) return 1;
  let total = 0;
  for (let from = 0; from < 64; from++) for (const to of c.legal(from)) {
    const promos = c.board[from].toLowerCase() === 'p' && (Math.floor(to / 8) === 0 || Math.floor(to / 8) === 7) ? 'qrbn' : 'q';
    for (const p of promos) { const d = c.copy(); d.apply(from, to, p); total += perft(d, depth - 1); }
  }
  return total;
}

test('chess: move counts (GamesTest.chessMovesAreRight)', () => {
  assert.equal(perft(new Chess(), 3), 8902);
  assert.equal(perft(fromFen('r3k2r/p1ppqpb1/bn2pnp1/3PN3/1p2P3/2N2Q1p/PPPBBPPP/R3K2R w KQkq -'), 3), 97862);
  assert.equal(perft(fromFen('8/2p5/3p4/KP5r/1R3p1k/8/4P1P1/8 w - -'), 3), 2812);
  assert.equal(perft(fromFen('r3k2r/Pppp1ppp/1b3nbN/nP6/BBP1P3/q4N2/Pp1P2PP/R2Q1RK1 w kq -'), 3), 9467);
  const c = new Chess();
  for (const [a, b] of [['f2', 'f3'], ['e7', 'e5'], ['g2', 'g4'], ['d8', 'h4']]) c.apply(sq(a), sq(b), 'q');
  assert.equal(c.result(), 'black');
});

test('chess: too little material (GameFixesTest.chessInsufficientMaterial)', () => {
  const board = (pieces) => new Chess(Array.from({ length: 64 }, (_, i) => pieces[i] || '.'));
  const K = 60, k = 4;
  assert.ok(board({ [K]: 'K', [k]: 'k' }).insufficient());
  assert.ok(board({ [K]: 'K', [k]: 'k', 10: 'N' }).insufficient());
  assert.ok(board({ [K]: 'K', [k]: 'k', 10: 'b' }).insufficient());
  assert.ok(board({ [K]: 'K', [k]: 'k', 2: 'B', 20: 'b' }).insufficient());
  assert.ok(!board({ [K]: 'K', [k]: 'k', 2: 'B', 21: 'b' }).insufficient());
  assert.ok(!board({ [K]: 'K', [k]: 'k', 10: 'N', 11: 'N' }).insufficient());
  assert.ok(!board({ [K]: 'K', [k]: 'k', 10: 'P' }).insufficient());
  assert.ok(!board({ [K]: 'K', [k]: 'k', 10: 'R' }).insufficient());
  assert.equal(board({ [K]: 'K', [k]: 'k', 2: 'B', 20: 'b' }).result(), 'draw:material');
});

test('chess: threefold repetition through the rules (GameFixesTest.chessThreefoldRepetition)', () => {
  let st = R.start('CHESS', ctx(), null).state;
  const dance = [[62, 45], [6, 21], [45, 62], [21, 6]];
  let ply = 0, last = null;
  for (let r = 0; r < 2; r++) for (const [from, to] of dance) {
    const o = R.move('CHESS', st, null, ply % 2, { from, to }, ctx());
    assert.ok(o, 'ply ' + ply);
    st = o.state; ply++; last = st;
    if (ply < 8) assert.equal(st.result, null, 'ended early at ply ' + ply);
  }
  assert.equal(last.result, 'draw:repetition');
});

test('chess: notation (MoreGamesTest.chessNotation)', () => {
  assert.deepEqual(sans([[52, 36, 'q'], [12, 28, 'q'], [62, 45, 'q'], [1, 18, 'q'], [61, 25, 'q'], [6, 21, 'q'], [60, 62, 'q']]), ['e4', 'e5', 'Nf3', 'Nc6', 'Bb5', 'Nf6', 'O-O']);
  const fool = [[53, 45, 'q'], [12, 28, 'q'], [54, 38, 'q'], [3, 39, 'q']];
  assert.equal(sans(fool).at(-1), 'Qh4#');
  assert.equal(positions(fool).length, 5);
  const c = new Chess();
  c.board.fill('.'); c.board[60] = 'K'; c.board[4] = 'k'; c.board[57] = 'N'; c.board[61] = 'N';
  assert.equal(c.san(57, 51), 'Nbd2');
});

test('chess: the state a Kotlin keeper sends after 1. e4, and the start', () => {
  const s0 = R.start('CHESS', ctx(), null).state;
  assert.deepEqual(s0, { board: 'rnbqkbnrpppppppp................................PPPPPPPPRNBQKBNR', white: true, castling: 'KQkq', ep: -1, quiet: 0, lf: null, lt: null, hist: [] });
  const o = R.move('CHESS', s0, null, 0, { from: 52, to: 36, promo: 'q' }, ctx());
  assert.deepEqual(o.state, { board: 'rnbqkbnrpppppppp....................P...........PPPP.PPPRNBQKBNR', white: false, castling: 'KQkq', ep: 44, quiet: 0, lf: 52, lt: 36, result: null, hist: [[52, 36, 'q']] });
  assert.equal(o.final, false);
  assert.equal(o.over, null);
  // Black's move only from seat 1, white's only from seat 0
  assert.equal(R.move('CHESS', o.state, null, 0, { from: 12, to: 28 }, ctx()), null);
  assert.ok(R.move('CHESS', o.state, null, 1, { from: 12, to: 28 }, ctx()));
  // A state as the Kotlin JSON writes it (numbers as decimals) is played on the same
  const k = JSON.parse('{"board":"rnbqkbnrpppppppp....................P...........PPPP.PPPRNBQKBNR","white":false,"castling":"KQkq","ep":44.0,"quiet":0,"lf":52,"lt":36,"result":null,"hist":[[52,36,"q"]]}');
  assert.ok(R.validState('CHESS', k, 2));
  assert.deepEqual(R.move('CHESS', k, null, 1, { from: 12, to: 28, promo: 'q' }, ctx()).state.hist, [[52, 36, 'q'], [12, 28, 'q']]);
});

test("chess: fool's mate through the rules ends the game for black", () => {
  let st = R.start('CHESS', ctx(), null).state;
  let o;
  [[53, 45], [12, 28], [54, 38], [3, 39]].forEach(([from, to], i) => { o = R.move('CHESS', st, null, i % 2, { from, to }, ctx()); st = o.state; });
  assert.equal(st.result, 'black');
  assert.deepEqual(o.over, [1]);
  assert.equal(o.final, true);
  assert.equal(R.move('CHESS', st, null, 0, { from: 52, to: 44 }, ctx()), null);
});

test('chess: promotion and its piece (the raw letter kept in the history)', () => {
  const c = fromFen('8/P6k/8/8/8/8/8/K7 w - -');
  const st = { ...c.encode(), hist: [] };
  const o = R.move('CHESS', st, null, 0, { from: 8, to: 0, promo: 'n' }, ctx());
  assert.equal(o.state.board[0], 'N');
  const o2 = R.move('CHESS', st, null, 0, { from: 8, to: 0, promo: 'x' }, ctx());
  assert.equal(o2.state.board[0], 'Q');
  assert.deepEqual(o2.state.hist, [[8, 0, 'x']]);
});

// ====== Casino ======

test('barbut: the stake only between rounds (GamesTest.barbutStakeOnlyBetweenRounds)', () => {
  const rnd = seeded(3);
  const s = Barbut.start(2, 20);
  assert.equal(s.stake, 20);
  assert.equal(Barbut.setStake(s, 100), null);
  Barbut.roll(s, s.throwers[s.turn], ['a', 'b'], rnd);
  assert.notEqual(Barbut.setStake(s, 5), null);
  let guard = 0;
  while (s.roundWinner < 0 && guard++ < 50) Barbut.roll(s, s.throwers[s.turn], ['a', 'b'], rnd);
  assert.ok(s.roundWinner >= 0);
  assert.equal(Barbut.decode(Barbut.encode(s)).stake, s.stake);
  assert.equal(Barbut.setStake(s, 0), null);
  const b = Barbut.start(3);
  for (let i = 0; i < 200; i++) if (b.winner < 0) Barbut.roll(b, b.throwers[b.turn], ['a', 'b', 'c'], rnd);
  assert.ok(b.winner >= 0 && b.wins[b.winner] === Barbut.TARGET);
  assert.ok(Barbut.rank(1, 1) > Barbut.rank(6, 5));
});

test('barbut: a round pays the stake from each loser (never more than their purse)', () => {
  const dice = [6, 6, 2, 1]; // the first throws 6-6, the second 2-1
  const rnd = { nextInt: () => dice.shift() };
  let st = R.start('BARBUT', ctx(), { stake: 30 }).state;
  assert.equal(st.stake, 30);
  const o1 = R.move('BARBUT', st, null, 0, { op: 'roll' }, ctx(two, 1000), rnd);
  assert.deepEqual(o1.pay, {});
  const purses = [1000, 10];
  const o2 = R.move('BARBUT', o1.state, null, 1, { op: 'roll' }, { players: two, host: false, purse: (i) => purses[i] }, rnd);
  assert.deepEqual(o2.pay, { 0: 10, 1: -10 });
  assert.deepEqual(o2.state, { wins: [1, 0], throwers: [0, 1], turn: 0, rolls: { 0: [6, 6], 1: [2, 1] }, round: 2, winner: -1, last: 'Runda 1: A (6-6)', stake: 30, rw: 0, pay: [10, -10] });
  // Only the keeper sets the stake
  assert.equal(R.move('BARBUT', o2.state, null, 1, { op: 'stake', v: 5 }, ctx()), null);
  assert.equal(R.move('BARBUT', o2.state, null, 0, { op: 'stake', v: 5 }, ctx(two, 1000, true)).state.stake, 5);
  // A Kotlin keeper's state (rolls keyed by strings) reads the same
  assert.ok(R.validState('BARBUT', JSON.parse(JSON.stringify(o2.state)), 2));
  assert.ok(!R.validState('BARBUT', { ...o2.state, rolls: { 0: [7, 1] } }, 2));
});

test('roulette and slots (MoreGamesTest.rouletteAndSlots)', () => {
  assert.equal(Roulette.multiplier('n', 17, 17), 35);
  assert.equal(Roulette.multiplier('red', 0, 1), 1);
  assert.equal(Roulette.multiplier('red', 0, 0), 0);
  assert.equal(Roulette.multiplier('dozen', 3, 30), 2);
  assert.equal(Roulette.multiplier('column', 1, 34), 2);
  const s = Roulette.start(2);
  assert.equal(Roulette.move(s, 0, 'bet', 'red', 0, 50, 100, false), null);
  assert.notEqual(Roulette.move(s, 0, 'bet', 'n', 5, 60, 50, false), null);
  assert.equal(Roulette.move(s, 1, 'ready', '', 0, 0, 100, false), null);
  assert.equal(Roulette.move(s, 0, 'ready', '', 0, 0, 100, false, seeded(1)), null);
  assert.equal(s.phase, 'spun');
  assert.equal(s.pay[0], Roulette.RED.has(s.result) ? 50 : -50);
  assert.equal(s.pay[1], 0);
  const rnd = seeded(9);
  const sl = Slots.start();
  let net = 0;
  for (let i = 0; i < 200000; i++) net += Slots.spin(sl, 10, 1000, rnd);
  const rtp = 1 + net / (200000 * 10);
  assert.ok(rtp >= 0.84 && rtp <= 0.97, 'RTP ' + rtp);
  assert.equal(Slots.payout([7, 7, 7], 10), 5000);
  assert.equal(Slots.payout([0, 3, 0], 10), 10);
  assert.equal(Slots.payout([0, 1, 2], 10), 0);
});

test('roulette through the rules: chips leave the purse, the spin pays back', () => {
  const rnd = { nextInt: () => 7 }; // 7: red, odd, low, first dozen, first column
  const ro = R.start('ROULETTE', ctx(), null).state;
  const b = R.move('ROULETTE', ro, null, 1, { op: 'bet', type: 'red', value: 0, v: 30 }, ctx());
  assert.deepEqual(b.pay, { 1: -30 });
  assert.deepEqual(R.committed('ROULETTE', b.state, null), { 1: 30 });
  const b2 = R.move('ROULETTE', b.state, null, 1, { op: 'bet', type: 'n', value: 7, v: 10 }, ctx());
  const r0 = R.move('ROULETTE', b2.state, null, 0, { op: 'ready' }, ctx());
  const spun = R.move('ROULETTE', r0.state, null, 1, { op: 'ready' }, ctx(), rnd);
  assert.equal(spun.state.phase, 'spun');
  assert.equal(spun.state.result, 7);
  assert.deepEqual(spun.state.pay, [0, 30 + 350]);
  assert.deepEqual(spun.pay, { 1: 380 + 40 });
  assert.deepEqual(spun.profit, { 1: 380 });
  assert.deepEqual(spun.over, [1]);
  assert.deepEqual(spun.played, [1]);
  assert.deepEqual(R.committed('ROULETTE', spun.state, null), {});
  const next = R.move('ROULETTE', spun.state, null, 0, { op: 'next' }, ctx());
  assert.deepEqual(next.pay, {});
  assert.equal(next.state.round, 2);
  // Undo gives the chip back
  const u = R.move('ROULETTE', b2.state, null, 1, { op: 'undo' }, ctx());
  assert.deepEqual(u.pay, { 1: 10 });
  // Only the keeper spins by hand
  assert.equal(R.move('ROULETTE', b.state, null, 1, { op: 'spin' }, ctx()), null);
  assert.ok(R.move('ROULETTE', b.state, null, 0, { op: 'spin' }, ctx(two, 1000, true)));
});

test('slots through the rules: one spin, the change to the purse', () => {
  const picks = [0, 0, 0]; // three cherries: stake × 6
  const rnd = { nextInt: () => picks.shift() };
  const st = R.start('SLOTS', ctx([two[0]]), null).state;
  assert.deepEqual(st, { reels: [6, 6, 6], stake: 10, last: 0, spins: 0, history: [] });
  const o = R.move('SLOTS', st, null, 0, { op: 'spin', v: 20 }, ctx([two[0]]), rnd);
  assert.deepEqual(o.state, { reels: [0, 0, 0], stake: 20, last: 100, spins: 1, history: [100] });
  assert.deepEqual(o.pay, { 0: 100 });
  assert.deepEqual(o.over, [0]);
  assert.equal(R.move('SLOTS', st, null, 0, { op: 'spin', v: 2000 }, ctx([two[0]], 1000)), null);
});

test('blackjack: hands (GamesTest.blackjackPlaysAHand)', () => {
  assert.equal(Blackjack.value([0, 12])[0], 21);
  assert.ok(Blackjack.blackjack([0, 12]));
  assert.equal(Blackjack.value([0, 0])[0], 12);
  assert.equal(Blackjack.value([9, 9, 1])[0], 22);
  const rnd = seeded(5);
  const purse = () => 1000;
  for (let k = 0; k < 200; k++) {
    const s = Blackjack.start(3, rnd);
    assert.equal(s.phase, 'bet');
    assert.notEqual(Blackjack.move(s, 0, 'bet', 5000, purse, true, rnd), null);
    assert.equal(Blackjack.move(s, 0, 'bet', 100, purse, true, rnd), null);
    assert.equal(Blackjack.move(s, 1, 'skip', 0, purse, false, rnd), null);
    assert.equal(s.phase, 'bet');
    assert.equal(Blackjack.move(s, 2, 'bet', 40, purse, false, rnd), null);
    assert.ok(s.phase === 'play' || s.phase === 'done');
    assert.equal(s.hands[1].length, 0);
    let guard = 0;
    while (s.phase === 'play' && guard++ < 30) {
      const p = s.turn;
      assert.ok(p === 0 || p === 2);
      assert.equal(Blackjack.move(s, p, Blackjack.value(s.hands[p])[0] < 15 ? 'hit' : 'stand', 0, purse, false, rnd), null);
    }
    assert.equal(s.phase, 'done');
    assert.ok(Blackjack.value(s.dealer)[0] >= 17 || s.hands.filter((h) => h.length).every((h) => Blackjack.value(h)[0] > 21 || Blackjack.blackjack(h)) || Blackjack.blackjack(s.dealer));
    assert.equal(s.result[1], 0);
    for (const p of [0, 2]) assert.ok([-s.bets[p], 0, s.bets[p], Math.trunc(s.bets[p] * 3 / 2)].includes(s.result[p]));
    assert.equal(Blackjack.move(s, 0, 'next', 0, purse, true, rnd), null);
    assert.equal(s.phase, 'bet');
    assert.ok(s.dealer.length === 0 && s.hands.every((h) => !h.length));
    assert.deepEqual(Blackjack.publicState(s).shoe, []);
  }
});

test('blackjack through the rules: the bet leaves the purse, the hole card stays hidden', () => {
  const bj = R.start('BLACKJACK', ctx(), null, seeded(2));
  assert.deepEqual(bj.state.shoe, []);
  assert.equal(bj.state.left, 312);
  assert.equal(bj.secret.shoe.length, 312);
  assert.ok(R.validState('BLACKJACK', bj.state, 2));
  const bet = R.move('BLACKJACK', bj.state, bj.secret, 0, { op: 'bet', v: 100 }, ctx());
  assert.deepEqual(bet.pay, { 0: -100 });
  assert.deepEqual(R.committed('BLACKJACK', bet.state, bet.secret), { 0: 100 });
  assert.deepEqual(R.waitingOn('BLACKJACK', bet.state, bet.secret), [1]);
  assert.equal(R.move('BLACKJACK', bet.state, null, 1, { op: 'skip' }, ctx()), null); // no secret: not the keeper
  // A known shoe: drawn from the end. Player 10+9 (cards 9, 21), dealer 10 + hole 7 (9, 19), the player stands: 19 beats 17
  const shoe = [5, 6, 19, 21, 9, 9];
  const sec = { ...bet.secret, shoe: [...Array(60).fill(0), ...shoe] };
  const d = R.move('BLACKJACK', bet.state, sec, 1, { op: 'skip' }, ctx());
  assert.deepEqual(d.secret.hands[0], [9, 21]);
  assert.deepEqual(d.secret.dealer, [9, 19]);
  assert.deepEqual(d.state.dealer, [9, -1]);
  assert.equal(d.state.phase, 'play');
  assert.equal(d.state.turn, 0);
  assert.deepEqual(d.pay, {});
  const end = R.move('BLACKJACK', d.state, d.secret, 0, { op: 'stand' }, ctx());
  assert.equal(end.state.phase, 'done');
  assert.deepEqual(end.state.dealer, [9, 19]);
  assert.deepEqual(end.state.result, [100, 0]);
  assert.deepEqual(end.state.outcome, ['Câștigat', '']);
  assert.deepEqual(end.pay, { 0: 200 });
  assert.deepEqual(end.profit, { 0: 100 });
  assert.deepEqual(end.over, [0]);
  assert.deepEqual(end.played, [0]);
  // A Kotlin keeper's public state ("left" as a number) is one the views read
  assert.ok(R.validState('BLACKJACK', clone(end.state), 2));
  assert.ok(!R.validState('BLACKJACK', { ...end.state, dealer: [99] }, 2));
});

// ====== Poker (MoreGamesTest) ======

const card = (rank, su) => su * 13 + rank - 1;

test('poker: hands rank (MoreGamesTest.pokerHandsRank)', () => {
  const royal = [card(1, 0), card(13, 0), card(12, 0), card(11, 0), card(10, 0), card(2, 1), card(3, 2)];
  const quads = [card(9, 0), card(9, 1), card(9, 2), card(9, 3), card(2, 0), card(3, 1), card(4, 2)];
  const full = [card(5, 0), card(5, 1), card(5, 2), card(8, 3), card(8, 0), card(2, 1), card(3, 2)];
  const flush = [card(2, 1), card(6, 1), card(9, 1), card(11, 1), card(13, 1), card(3, 0), card(4, 2)];
  const wheel = [card(1, 0), card(2, 1), card(3, 2), card(4, 3), card(5, 0), card(9, 1), card(12, 2)];
  const trips = [card(7, 0), card(7, 1), card(7, 2), card(2, 3), card(9, 0), card(11, 1), card(13, 2)];
  const twoPair = [card(7, 0), card(7, 1), card(9, 2), card(9, 3), card(2, 0), card(11, 1), card(13, 2)];
  const pairA = [card(1, 0), card(1, 1), card(9, 2), card(4, 3), card(2, 0), card(11, 1), card(13, 2)];
  const pairK = [card(13, 0), card(13, 1), card(9, 2), card(4, 3), card(2, 0), card(11, 1), card(12, 2)];
  const order = [royal, quads, full, flush, wheel, trips, twoPair, pairA, pairK].map(Poker.best);
  assert.deepEqual([...order].sort((a, b) => b - a), order);
  assert.equal(Poker.handName(Poker.best(royal)), 'Chintă de culoare');
  assert.equal(Poker.handName(Poker.best(wheel)), 'Chintă');
  assert.equal(Poker.handName(Poker.best(twoPair)), 'Două perechi');
});

test('poker: the chips add up, side pots (MoreGamesTest.pokerChipsAddUp, pokerSidePot)', () => {
  const rnd = seeded(5);
  for (let k = 0; k < 300; k++) {
    const np = 2 + rnd.nextInt(5);
    const purses = Array.from({ length: np }, () => 100 + rnd.nextInt(2900));
    const s = Poker.start(np, purses, -1, 10, 20, rnd);
    let guard = 0;
    while (s.phase !== 'done' && guard++ < 200) {
      const p = s.turn;
      const owe = s.current - s.bet[p];
      const r = rnd.nextInt(6);
      const op = r === 0 ? 'fold' : r === 1 ? 'allin' : r === 2 ? 'raise' : owe > 0 ? 'call' : 'check';
      if (Poker.move(s, p, op, s.current + s.minRaise + rnd.nextInt(50)) !== null) assert.equal(Poker.move(s, p, owe > 0 ? 'call' : 'check', 0), null);
    }
    assert.equal(s.phase, 'done');
    assert.equal(s.result.reduce((a, b) => a + b, 0), 0, 'Lei appear or vanish: ' + s.result);
    for (let i = 0; i < np; i++) assert.ok(s.result[i] >= -purses[i]);
  }
  const sp = Poker.start(3, [100, 500, 500], -1, 10, 20, rnd);
  while (sp.phase !== 'done') { const p = sp.turn; if (Poker.move(sp, p, 'allin', 0) !== null) Poker.move(sp, p, 'call', 0); }
  assert.equal(sp.result.reduce((a, b) => a + b, 0), 0);
  assert.ok(sp.result[0] <= 200);
});

test('poker: a short all-in does not reopen; a raise above the stack is all in; a leaver folds', () => {
  const s = Poker.start(3, [1000, 1000, 70], -1, 10, 20, seeded(1));
  const bb = s.bet.findIndex((b) => b === s.big);
  const order = [s.turn, (s.turn + 1) % 3, (s.turn + 2) % 3];
  assert.equal(Poker.move(s, order[0], 'raise', 60), null);
  assert.equal(Poker.move(s, order[1], 'call', 0), null);
  assert.equal(s.turn, bb);
  assert.equal(Poker.move(s, bb, 'allin', 0), null);
  assert.notEqual(Poker.move(s, order[0], 'raise', 300), null);
  assert.equal(Poker.move(s, order[0], 'call', 0), null);
  const r = Poker.start(3, [100, 1000, 1000], -1, 10, 20, seeded(2));
  const p = r.stack.findIndex((x, i) => x + r.bet[i] === 100);
  while (r.turn !== p) Poker.move(r, r.turn, 'call', 0);
  assert.equal(Poker.move(r, p, 'raise', 100000), null);
  assert.equal(r.bet[p], 100);
  assert.ok(r.minRaise <= 100);
  const l = Poker.start(3, [1000, 1000, 1000], -1, 10, 20, seeded(3));
  const gone = (l.turn + 1) % 3;
  assert.equal(Poker.move(l, gone, 'leave', 0), null);
  assert.ok(l.folded[gone]);
  let guard = 0;
  while (l.phase !== 'done' && guard++ < 50) assert.equal(Poker.move(l, l.turn, l.current > l.bet[l.turn] ? 'call' : 'check', 0), null);
  Poker.deal(l, [1000, 1000, 1000], seeded(4));
  assert.ok(l.out[gone]);
});

test('poker through the rules: blinds leave the purse, the cards go privately, the stack follows the purse', () => {
  const o = R.start('POKER', ctx(two, 1000), null, seeded(6));
  const blinds = R.committed('POKER', o.state, o.secret);
  assert.equal(Object.values(o.pay).reduce((a, b) => a - b, 0), Object.values(blinds).reduce((a, b) => a + b, 0));
  assert.deepEqual(Object.values(blinds).sort(), [10, 20]);
  assert.ok(o.state.hands.every((h) => h.every((c) => c === -1)));
  assert.deepEqual(o.state.deck, []);
  assert.deepEqual(o.privates[0], { hand: 1, cards: o.secret.hands[0] });
  assert.ok(R.validState('POKER', clone(o.state), 2));
  // GameFixesTest.pokerStackFollowsThePurse
  const turn = o.secret.turn;
  const m = R.move('POKER', o.state, o.secret, turn, { op: 'allin' }, ctx(two, 200));
  assert.ok(-(m.pay[turn] || 0) <= 200, JSON.stringify(m.pay));
});

// ====== Backgammon (MoreGamesTest) ======

function bg(pts, turn, dice) {
  const s = Backgammon.start(turn);
  s.points.fill(0);
  for (const [i, k] of pts) s.points[i] = k;
  s.off[0] = 15 - s.points.filter((x) => x > 0).reduce((a, b) => a + b, 0);
  s.off[1] = 15 + s.points.filter((x) => x < 0).reduce((a, b) => a + b, 0);
  s.dice = dice.slice();
  s.rolled = dice.slice(0, 2);
  return s;
}

test('backgammon: plays to the end, 15 checkers each (MoreGamesTest.backgammonPlaysToTheEnd)', () => {
  const rnd = seeded(3);
  for (let k = 0; k < 30; k++) {
    const s = Backgammon.start(rnd.nextInt(2));
    let guard = 0;
    while (s.winner < 0 && guard++ < 5000) {
      if (s.gameWinner >= 0) { assert.equal(Backgammon.move(s, 1 - s.gameWinner, 'again', 0, 0), null); continue; }
      const p = s.turn;
      if (!s.dice.length) { assert.equal(Backgammon.move(s, p, 'roll', 0, 0, rnd), null); continue; }
      const l = Backgammon.legal(s);
      const m = l[rnd.nextInt(l.length)];
      assert.equal(Backgammon.move(s, p, 'move', m[0], m[1]), null);
      for (const q of [0, 1]) assert.equal(s.points.reduce((a, v) => a + (q === 0 ? Math.max(v, 0) : Math.max(-v, 0)), 0) + s.bar[q] + s.off[q], 15);
    }
    assert.ok(s.winner >= 0 && s.score[s.winner] >= Backgammon.TARGET);
  }
});

test('backgammon: the rules (MoreGamesTest.backgammonMustUseBothDice, backgammonRules)', () => {
  const { BAR, OFF } = Backgammon;
  let s = Backgammon.start(0);
  s.points.fill(0); s.points[9] = 1; s.points[3] = -2; s.off[0] = 14; s.off[1] = 13; s.dice = [6, 5];
  let l = Backgammon.legal(s);
  assert.ok(l.length && l.every((x) => x[2] === 5), JSON.stringify(l));
  s = bg([[5, 1], [2, 1], [18, -1]], 0, [6, 1]);
  l = Backgammon.legal(s);
  assert.ok(l.some((x) => x[0] === 5 && x[1] === OFF && x[2] === 6));
  assert.ok(!l.some((x) => x[0] === 2 && x[2] === 6));
  s = bg([[6, 1], [0, 1], [18, -1]], 0, [1, 2]);
  assert.ok(!Backgammon.legal(s).some((x) => x[1] === OFF && x[0] === 0 && x[2] === 2));
  s = bg([[10, 3], [18, -2], [19, -2], [20, -2], [21, -2], [22, -2], [23, -2], [5, -1]], 0, [3, 4]);
  s.bar[0] = 1; s.off[0] = 11;
  assert.equal(Backgammon.legal(s).length, 0);
  s.dice = []; s.rolled = [];
  assert.equal(Backgammon.move(s, 0, 'roll', 0, 0, seeded(1)), null);
  assert.equal(s.turn, 1);
  assert.ok(Backgammon.blocked(s));
  s = bg([[10, 2], [20, -1], [3, -2]], 0, [4, 1]);
  s.bar[0] = 1; s.off[0] = 12;
  l = Backgammon.legal(s);
  assert.ok(l.every((x) => x[0] === BAR));
  assert.equal(Backgammon.move(s, 0, 'move', BAR, 20), null);
  assert.equal(s.bar[1], 1);
  s = bg([[10, 1], [7, -2], [4, -1]], 0, [3, 6]);
  assert.ok(!Backgammon.legal(s).some((x) => x[1] === 7));
  s = bg([[12, 1], [4, -2]], 0, [5, 3]);
  s.off[0] = 14;
  l = Backgammon.legal(s);
  assert.ok(l.length && l.every((x) => x[2] === 5));
  s = bg([[23, 2], [0, -2]], 0, [2, 2, 2, 2]);
  s.off[0] = 13; s.off[1] = 13;
  for (let k = 0; k < 4; k++) { const m = Backgammon.legal(s)[0]; assert.equal(Backgammon.move(s, 0, 'move', m[0], m[1]), null); }
  assert.equal(s.turn, 1);
  s = bg([[0, 1], [3, -15]], 0, [1, 2]);
  s.off[1] = 0;
  const m = Backgammon.legal(s).find((x) => x[1] === OFF);
  assert.equal(Backgammon.move(s, 0, 'move', m[0], m[1]), null);
  assert.equal(s.lastPoints, 3);
  for (let seed = 0; seed < 200; seed++) { const f = Backgammon.start(0); assert.equal(Backgammon.move(f, 0, 'roll', 0, 0, seeded(seed)), null); assert.notEqual(f.rolled[0], f.rolled[1]); }
});

// ====== The checks of a state from the bus (ReviewFixesTest) ======

test('malformed states are not taken (ReviewFixesTest.malformedTableStatesAreNotTaken)', () => {
  const junk = [{}, { board: 5 }, { hands: 'x' }, { cells: ['a'] }, { turn: 99 }, { hands: [[1]], turn: 7, winner: 9 }];
  for (const kind of R.PLAYABLE) for (const st of junk) {
    const nn = Math.min(Math.max(R.kindMin(kind), 2), R.kindMax(kind));
    assert.equal(R.validState(kind, st, nn), false, kind + ' took ' + JSON.stringify(st));
  }
  assert.equal(R.validState('TICTACTOE', { ...Grid.encode(Grid.ticTacToe()), turn: 5 }, 2), false);
  assert.equal(R.validState('CONNECT4', { ...Grid.encode(Grid.connectFour()), cells: Array(5).fill(-1) }, 2), false);
  const chess = { ...new Chess().encode(), hist: [] };
  assert.equal(R.validState('CHESS', { ...chess, hist: [[99, -4, 'q']] }, 2), false);
  assert.equal(R.validState('CHESS', chess, 3), false);
  assert.equal(R.validState('CHESS', chess, 2), true);
});

test('real states pass the check, played through (ReviewFixesTest.realStatesPassTheCheck)', () => {
  const rnd = seeded(7);
  for (const kind of R.PLAYABLE) for (let np = R.kindMin(kind); np <= Math.min(R.kindMax(kind), 5); np++) for (let rep = 0; rep < 3; rep++) {
    const ps = Array.from({ length: np }, (_, i) => ({ uid: 'u' + i, name: 'P' + i }));
    const c = { players: ps, host: true, purse: () => 1000 };
    let o = R.start(kind, c, null, rnd);
    let st = o.state, sec = o.secret;
    for (let step = 0; step < 400; step++) {
      assert.ok(R.validState(kind, clone(st), np), `${kind} (${np}) step ${step}: ${JSON.stringify(st).slice(0, 300)}`);
      if (np === 2 && R.turnBased(kind) && step === 5) assert.ok(R.validState(kind, R.forfeit(kind, st, 1, np, 'time').state, np));
      if (st.final === true) break;
      const waiting = R.waitingOn(kind, st, sec);
      const seats = [...new Set([...waiting, ...Array.from({ length: np }, (_, i) => i)])];
      let next = null;
      for (const p of seats) {
        const tries = [];
        if (kind === 'CHESS') { const ch = Chess.decode(st); for (let f = 0; f < 64; f++) for (const t of ch.legal(f)) tries.push({ from: f, to: t, promo: 'q' }); tries.sort(() => rnd.nextInt(3) - 1); }
        else if (kind === 'TICTACTOE' || kind === 'CONNECT4') for (let cell = 0; cell < 42; cell++) tries.push({ cell: (cell * 7 + step) % 42 });
        else if (kind === 'BACKGAMMON') { tries.push({ op: 'roll' }, { op: 'again' }); for (const [f, t] of Backgammon.legal(Backgammon.decode(st))) tries.push({ op: 'move', from: f, to: t }); }
        else if (kind === 'POKER') tries.push({ op: 'next' }, { op: 'check' }, { op: 'call' }, { op: ['raise', 'fold', 'allin'][rnd.nextInt(3)], v: 60 }, { op: 'fold' });
        else tries.push({ op: 'roll' }, { op: 'bet', v: 10 }, { op: 'hit' }, { op: 'stand' }, { op: 'next' }, { op: 'bet', type: 'red', value: 0, v: 5 }, { op: 'ready' }, { op: 'spin', v: 10 });
        for (const m of tries) { const r = R.move(kind, st, sec, p, m, c, rnd); if (r) { next = r; break; } }
        if (next) break;
      }
      if (!next) break;
      st = next.final ? { ...next.state, final: true } : next.state;
      sec = next.secret || sec;
    }
  }
});

test('forfeits and the clock (GameFixesTest.forfeitsAndTheClock)', () => {
  const st = R.start('CHESS', ctx(), null).state;
  assert.deepEqual(R.waitingOn('CHESS', st, null), [0]);
  const f = R.forfeit('CHESS', st, 0, 2, 'resign');
  assert.equal(f.final, true);
  assert.deepEqual(f.over, [1]);
  assert.equal(f.state.result, 'black');
  assert.equal(f.state.resigned, 0);
  assert.deepEqual(R.waitingOn('CHESS', f.state, null), []);
  const g = R.forfeit('CONNECT4', Grid.encode(Grid.connectFour()), 1, 2, 'time');
  assert.equal(g.state.winner, 0);
  assert.ok(R.turnBased('BARBUT'));
  assert.ok(!R.turnBased('POKER'));
});

// ====== The keeper (hub) over a bus shared by two people, as the PC's GameHub ======

function net() {
  const kept = { lei: {}, scores: {} };
  const peers = [];
  const log = [];
  function server(from, m) {
    log.push({ from, m: clone(m) });
    if (m.t === 'add' && m.kind === 'lei') {
      const cur = { ...(kept.lei[m.id] || {}) };
      if (m.once && cur.d === m.once) return;
      if (m.once) cur.d = m.once;
      let v = (typeof cur.v === 'number' ? cur.v : m.start) + m.d;
      if (v <= 0) v = m.min;
      cur.v = v; cur.n = m.set.n;
      kept.lei = { ...kept.lei, [m.id]: cur };
      return;
    }
    if (m.t === 'incr' || m.t === 'put') return;
    for (const p of peers) if (p.uid !== from) p.hub.handle({ ...clone(m), from, relay: true });
  }
  function person(uid, name) {
    const p = { uid };
    p.hub = createHub({
      send: (m) => { server(uid, m); return true; }, kept: () => kept, caps: () => new Set(['add', 'incr']), online: () => true,
      me: () => ({ uid, name }), present: () => new Set(peers.map((x) => x.uid)), text: (k, v) => k + (v ? JSON.stringify(v) : ''), rnd: seeded(uid.length * 7 + 1),
    });
    peers.push(p);
    return p.hub;
  }
  return { kept, log, person };
}

test('hub: a tic-tac-toe table kept by one, played by both, the same state on each side', () => {
  const w = net();
  const ana = w.person('ana', 'Ana'), bo = w.person('bo', 'Bo');
  const id = ana.create('TICTACTOE');
  assert.ok(bo.tables.has(id));
  bo.join(id);
  assert.deepEqual(ana.tables.get(id).players.map((p) => p.uid), ['ana', 'bo']);
  ana.startGame(id);
  const t = ana.tables.get(id);
  assert.ok(t.started);
  const first = t.players[0].uid;
  const order = first === 'ana' ? [ana, bo] : [bo, ana];
  for (const [i, cell] of [0, 3, 1, 4, 2].entries()) order[i % 2].play(id, { cell });
  assert.deepEqual(clone(bo.tables.get(id).state), clone(ana.tables.get(id).state));
  assert.equal(ana.tables.get(id).state.winner, 0);
  assert.equal(ana.tables.get(id).state.final, true);
  // The keeper scored it: a game for both, a win for the first
  const incr = w.log.filter((x) => x.m.t === 'incr' && x.m.id !== '_day').map((x) => x.m);
  assert.deepEqual(incr.map((m) => [m.id, m.inc]), [[first, { 'g.TICTACTOE': 1, 'w.TICTACTOE': 1, 'w7.TICTACTOE': 1 }], [first === 'ana' ? 'bo' : 'ana', { 'g.TICTACTOE': 1 }]]);
  // A move after the end is refused (the state stays)
  const before = JSON.stringify(ana.tables.get(id).state);
  order[1].play(id, { cell: 8 });
  assert.equal(JSON.stringify(ana.tables.get(id).state), before);
});

test('hub: a duel left mid-game is lost; onlookers bet and are paid by stake', () => {
  const w = net();
  const ana = w.person('ana', 'Ana'), bo = w.person('bo', 'Bo'), cy = w.person('cy', 'Cy'), di = w.person('di', 'Di');
  const id = ana.create('CONNECT4');
  bo.join(id);
  ana.startGame(id);
  const seats = ana.tables.get(id).players.map((p) => p.uid);
  cy.bet(id, 0, 100); di.bet(id, 1, 50); di.bet(id, 1, 25);
  assert.equal(ana.tables.get(id).wagers.length, 3);
  assert.equal(w.kept.lei.cy.v, 900);
  // A player can't bet on their own table
  bo.bet(id, 0, 10);
  assert.equal(ana.tables.get(id).wagers.length, 3);
  // Bo gets up: the other seat wins; the pool (175) goes to who bet on it, by stake
  const boSeat = seats.indexOf('bo');
  bo.leave(id);
  const t = ana.tables.get(id);
  assert.equal(t.state.resigned, boSeat);
  assert.equal(t.state.why, 'gone');
  assert.equal(t.state.winner, 1 - boSeat);
  assert.equal(t.state.final, true);
  if (boSeat === 1) { assert.equal(w.kept.lei.cy.v, 1075); assert.equal(w.kept.lei.di.v, 925); }
  else { assert.equal(w.kept.lei.cy.v, 900); assert.equal(w.kept.lei.di.v, 1100); }
  assert.equal(t.wagers.length, 0);
  assert.equal(t.note, 'gBetsPaid{"amount":"175 Lei"}');
});

test('hub: closing a casino table gives back the chips on it', () => {
  const w = net();
  const ana = w.person('ana', 'Ana'), bo = w.person('bo', 'Bo');
  const id = ana.create('ROULETTE');
  bo.join(id);
  ana.startGame(id);
  bo.play(id, { op: 'bet', type: 'red', value: 0, v: 40 });
  assert.equal(w.kept.lei.bo.v, 960);
  // More than the purse holds: refused by the keeper
  bo.play(id, { op: 'bet', type: 'n', value: 3, v: 5000 });
  assert.equal(w.kept.lei.bo.v, 960);
  ana.leave(id);
  assert.equal(w.kept.lei.bo.v, 1000);
  assert.ok(!bo.tables.has(id));
});

test('hub: a table from someone other than its host, or malformed, is not taken', () => {
  const w = net();
  const ana = w.person('ana', 'Ana');
  const chess = { ...new Chess().encode(), hist: [] };
  const table = (from, host, st) => ({ t: 'g', op: 'table', from, id: 'x1', kind: 'CHESS', host, players: two.map((p) => ({ ...p })), started: true, state: st, note: '', at: 0, wagers: [], tour: '' });
  ana.handle(table('mallory', 'alice', chess));
  assert.ok(!ana.tables.has('x1'));
  ana.handle(table('alice', 'alice', { board: 5 }));
  assert.ok(!ana.tables.has('x1'));
  ana.handle(table('alice', 'alice', chess));
  assert.ok(ana.tables.has('x1'));
  ana.handle(table('mallory', 'mallory', chess));
  assert.equal(ana.tables.get('x1').host, 'alice');
});

test('hub: the clock warns, then a two-player game is lost for time', () => {
  let clock = 1_000_000;
  const msgs = [];
  const hub = createHub({ send: (m) => { msgs.push(m); return true; }, kept: () => ({ lei: {}, scores: {} }), caps: () => new Set(['add', 'incr']), online: () => true, me: () => ({ uid: 'ana', name: 'Ana' }), present: () => new Set(['ana', 'bo']), now: () => clock, text: (k) => k, rnd: seeded(1) });
  const id = hub.create('CHESS');
  hub.handle({ t: 'g', op: 'join', from: 'bo', id, name: 'Bo' });
  hub.startGame(id);
  const t = hub.tables.get(id);
  const toMove = t.players[0].uid;
  hub.watchTurns(clock);
  clock += 125000;
  hub.watchTurns(clock);
  assert.deepEqual(hub.tables.get(id).state.afk, { s: [0], dl: 1_000_000 + 180000, f: true });
  clock += 60000;
  hub.watchTurns(clock);
  const st = hub.tables.get(id).state;
  assert.equal(st.resigned, 0);
  assert.equal(st.why, 'time');
  assert.equal(st.result, 'black');
  assert.equal(st.afk, undefined);
  assert.ok(toMove);
});

test('Lei and dates as the apps write them', () => {
  assert.equal(fmtLei(1000), '1.000 Lei');
  assert.equal(fmtLei(1234567), '1.234.567 Lei');
  assert.equal(fmtLei(-1500), '-1.500 Lei');
  assert.equal(isoWeek(new Date(2026, 9, 29)), '2026-W44');
  assert.equal(isoWeek(new Date(2021, 0, 3)), '2020-W53');
  assert.equal(isoWeek(new Date(2024, 11, 30)), '2025-W01');
});

if (failures.length) {
  for (const f of failures) console.error('FAIL ' + f);
  process.exit(1);
}
console.log('OK ' + n);
