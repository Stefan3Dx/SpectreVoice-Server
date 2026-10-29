// Each game's rules behind one door, as Rules.kt: start, a player's move (null: not allowed), the check of a state
// from the bus, whose turn it is, what's played for someone out of time, giving up, the chips on the table.
// Pure functions: no DOM, no bus. An outcome: {state, secret, pay: {seat: Lei}, profit, over, played, final, privates}.

import { toInt, mustInt, defaultRnd } from './util.js';
import * as Grid from './grid.js';
import { Chess, histMoves, positions } from './chess.js';
import { Barbut, Roulette, Slots } from './casino.js';
import { Blackjack } from './blackjack.js';

// GameHub.Kind: every game the apps know (min, max players), in the apps' order
export const KINDS = {
  CHESS: [2, 2, '♟️'], RUMMY: [2, 4, '🀄'], BARBUT: [2, 6, '🎲'], BLACKJACK: [1, 6, '🃏'], POKER: [2, 8, '♠️'], ROULETTE: [1, 8, '🎡'], SLOTS: [1, 1, '🎰'],
  BACKGAMMON: [2, 2, '🎯'], RENTZ: [3, 6, '🂡'], TICTACTOE: [2, 2, '❌'], CONNECT4: [2, 2, '🔴'],
  PHONE: [3, 10, '📞'], SKETCH: [2, 10, '🎨'], BATTLE: [2, 10, '🖌️'], SPY: [3, 10, '🕵️'], QUIP: [3, 10, '😂'], LIKELY: [3, 12, '👉'], TRIVIA: [1, 12, '🧠'], SONGS: [1, 10, '🎵'],
  HANGMAN: [1, 8, '🪢'], FAZAN: [2, 8, '🐦'], WHIST: [3, 6, '♦️'],
};
export const kindMin = (k) => KINDS[k][0];
export const kindMax = (k) => KINDS[k][1];
export const emoji = (k) => KINDS[k]?.[2] || '🎮';
export const PARTY = new Set(['PHONE', 'SKETCH', 'BATTLE', 'SPY', 'QUIP', 'LIKELY', 'TRIVIA', 'SONGS', 'HANGMAN', 'FAZAN']);
export const isParty = (k) => PARTY.has(k);
export const isChance = (k) => ['BARBUT', 'BLACKJACK', 'POKER', 'ROULETTE', 'SLOTS'].includes(k);
export const isDuel = (k) => ['CHESS', 'BACKGAMMON', 'TICTACTOE', 'CONNECT4'].includes(k);
// What this web client can play (and keep)
export const PLAYABLE = ['TICTACTOE', 'CONNECT4', 'CHESS', 'BARBUT', 'ROULETTE', 'SLOTS', 'BLACKJACK'];
export const playable = (k) => PLAYABLE.includes(k);

export function turnBased(k) {
  return ['CHESS', 'BACKGAMMON', 'TICTACTOE', 'CONNECT4', 'RUMMY', 'WHIST', 'RENTZ', 'BARBUT'].includes(k);
}

const out = (state, o = {}) => ({ state, secret: null, pay: {}, profit: null, over: null, played: null, final: false, privates: {}, ...o });

// ctx: {players: [{uid, name}], host: bool, purse: (seat) => Lei}; prev: the table's last state (its stake)
export function start(kind, ctx, prev, rnd = defaultRnd) {
  const n = ctx.players.length;
  switch (kind) {
    case 'CHESS': return out({ ...new Chess().encode(), hist: [] });
    case 'BARBUT': return out(Barbut.encode(Barbut.start(n, toInt(prev?.stake) ?? Barbut.STAKE)));
    case 'BLACKJACK': { const s = Blackjack.start(n, rnd); return out(Blackjack.publicState(s), { secret: Blackjack.encode(s) }); }
    case 'TICTACTOE': return out(Grid.encode(Grid.ticTacToe()));
    case 'CONNECT4': return out(Grid.encode(Grid.connectFour()));
    case 'ROULETTE': return out(Roulette.encode(Roulette.start(n)));
    case 'SLOTS': return out(Slots.encode(Slots.start()));
    default: throw new Error('no rules for ' + kind);
  }
}

const isInt = (x) => typeof x === 'number';

// The state everyone sees is one the views of kind can show for n players (Rules.validState)
export function validState(kind, st, n) {
  if (st === null || st === undefined) return true;
  if (typeof st !== 'object' || Array.isArray(st)) return false;
  if (n < 1) return false;
  const seat = (i, none = -1) => i >= none && i < n;
  const seats = (l) => l.every((i) => i >= 0 && i < n);
  const card = (c, top = 51) => c >= -1 && c <= top;
  const cards = (l, top = 51) => l.length <= 120 && l.every((c) => card(c, top));
  const sized = (...ls) => ls.every((l) => l.length === n);
  try {
    switch (kind) {
      case 'CHESS': {
        const c = Chess.decode(st);
        const hist = histMoves(st.hist);
        return n === 2 && c.board.length === 64 && (c.last === null || (c.last[0] >= 0 && c.last[0] <= 63 && c.last[1] >= 0 && c.last[1] <= 63)) &&
          hist.length <= 2000 && hist.every(([f, t]) => f >= 0 && f <= 63 && t >= 0 && t <= 63) && positions(hist).length > 0;
      }
      case 'BARBUT': {
        const s = Barbut.decode(st);
        return sized(s.wins) && seats(s.throwers) && s.turn >= 0 && s.turn <= s.throwers.length && seat(s.winner) && seat(s.roundWinner) &&
          s.wins.every((w) => w >= 0 && w <= Barbut.TARGET) && Object.entries(s.rolls).every(([k, v]) => seat(+k, 0) && v.length === 2 && v.every((d) => d >= 1 && d <= 6));
      }
      case 'BLACKJACK': {
        const s = Blackjack.decode(st);
        return sized(s.hands, s.bets, s.ready, s.done, s.result, s.outcome) && seat(s.turn) && cards(s.dealer) && s.hands.every((h) => cards(h));
      }
      case 'ROULETTE': {
        const s = Roulette.decode(st);
        return sized(s.bets, s.ready, s.pay) && s.result >= -1 && s.result <= 36 && s.history.every((x) => x >= 0 && x <= 36) && s.bets.every((b) => b.length <= 200);
      }
      case 'SLOTS': {
        const s = Slots.decode(st);
        return n === 1 && s.reels.length === 3 && s.reels.every((r) => r >= 0 && r < Slots.SYMBOLS.length);
      }
      case 'TICTACTOE': case 'CONNECT4': {
        const s = Grid.decode(st);
        return n === 2 && s.cols >= 1 && s.cols <= 12 && s.rows >= 1 && s.rows <= 12 && s.cells.length === s.cols * s.rows && s.cells.every((c) => c >= -1 && c <= 1) &&
          s.turn >= 0 && s.turn <= 1 && s.winner >= -2 && s.winner <= 1 && s.line.every((i) => i >= 0 && i < s.cells.length);
      }
      default: return true; // checked by the apps that play them; here they're only listed
    }
  } catch { return false; }
}

// The seats the game waits on now (empty: nobody in particular, or it's over); secret is the keeper's
export function waitingOn(kind, st, secret) {
  if (st.resigned !== undefined && st.resigned !== null) return [];
  const int = (k) => toInt(st[k]) ?? -1;
  try {
    let r;
    switch (kind) {
      case 'CHESS': r = st.result !== undefined && st.result !== null ? [] : [st.white === true ? 0 : 1]; break;
      case 'TICTACTOE': case 'CONNECT4': r = int('winner') !== -1 ? [] : [int('turn')]; break;
      case 'BARBUT': { const s = Barbut.decode(st); r = s.winner >= 0 ? [] : (s.throwers[s.turn] !== undefined ? [s.throwers[s.turn]] : []); break; }
      case 'BLACKJACK': {
        if (!secret) return [];
        const s = Blackjack.decode(secret);
        if (s.phase === 'bet') r = s.bets.some((b) => b > 0) ? s.ready.map((x, i) => (x ? -1 : i)).filter((i) => i >= 0) : [];
        else if (s.phase === 'play') r = [s.turn];
        else r = [];
        break;
      }
      case 'ROULETTE': { const s = Roulette.decode(st); r = s.phase === 'bet' && s.bets.some((b) => b.length) ? s.ready.map((x, i) => (x ? -1 : i)).filter((i) => i >= 0) : []; break; }
      default: r = [];
    }
    return r.filter((i) => i >= 0);
  } catch { return []; }
}

// What the keeper plays for seat when they're out of time or gone, tried in order
export function autoMoves(kind) {
  switch (kind) {
    case 'BARBUT': return [{ op: 'roll' }];
    case 'BLACKJACK': return [{ op: 'skip' }, { op: 'stand' }];
    case 'ROULETTE': return [{ op: 'ready' }];
    default: return [];
  }
}

// seat gives the game up (why: "resign", "time", "gone"): it ends at once and everyone else wins it
export function forfeit(kind, st, seat, n, why) {
  const others = [...Array(n).keys()].filter((i) => i !== seat);
  const pub = { ...st };
  delete pub.afk;
  pub.resigned = seat; pub.why = why;
  if (n === 2) {
    if (kind === 'CHESS') pub.result = seat === 0 ? 'black' : 'white';
    else if (['TICTACTOE', 'CONNECT4', 'BACKGAMMON'].includes(kind)) pub.winner = 1 - seat;
  }
  return out(pub, { over: others, final: true });
}

// Seat → the Lei on the table not settled yet (what goes back if the table is closed or started again)
export function committed(kind, st, secret) {
  const r = {};
  try {
    if (kind === 'BLACKJACK') {
      if (!secret) return r;
      const s = Blackjack.decode(secret);
      if (s.phase !== 'done') s.bets.forEach((b, i) => { if (b > 0) r[i] = b; });
    } else if (kind === 'ROULETTE') {
      if (!st) return r;
      const s = Roulette.decode(st);
      if (s.phase === 'bet') s.bets.forEach((b, i) => { const v = b.reduce((a, x) => a + x.amount, 0); if (v > 0) r[i] = v; });
    }
  } catch { return {}; }
  return r;
}

// Seat p's move m (as it came on the bus) on state st; null: not allowed
export function move(kind, st, secret, p, m, ctx, rnd = defaultRnd) {
  const int = (k) => toInt(m[k]) ?? -1;
  const op = typeof m.op === 'string' ? m.op : '';
  const names = ctx.players.map((x) => x.name);
  switch (kind) {
    case 'CHESS': {
      const c = Chess.decode(st);
      if (c.white !== (p === 0) || (st.result !== undefined && st.result !== null)) return null;
      const from = int('from'), to = int('to');
      if (!(from >= 0 && from <= 63) || !c.legal(from).includes(to)) return null;
      const promo = typeof m.promo === 'string' && m.promo.length ? m.promo[0] : 'q';
      const before = Array.isArray(st.hist) ? st.hist : [];
      c.apply(from, to, promo);
      let since = [];
      if (c.quiet !== 0) {
        const prior = before.map((x) => {
          if (!Array.isArray(x)) return null;
          return [mustInt(x[0]), mustInt(x[1]), typeof x[2] === 'string' && x[2].length ? x[2][0] : 'q'];
        }).filter(Boolean);
        since = positions(prior).slice(-c.quiet);
      }
      const r = c.result(since);
      const hist = [...before, [from, to, promo]];
      return out({ ...c.encode(), result: r, hist }, { over: r === null ? null : (r === 'white' ? [0] : r === 'black' ? [1] : []), final: r !== null });
    }
    case 'BARBUT': {
      const s = Barbut.decode(st);
      if (op === 'stake') return ctx.host && Barbut.setStake(s, int('v')) === null ? out(Barbut.encode(s)) : null;
      const round = s.round, was = s.winner;
      if (Barbut.roll(s, p, names, rnd) !== null) return null;
      let pay = {};
      if (s.round !== round) {
        const w = s.roundWinner;
        const list = Array(ctx.players.length).fill(0);
        if (w >= 0 && s.stake > 0) for (let i = 0; i < ctx.players.length; i++) if (i !== w) {
          const lose = Math.min(s.stake, ctx.purse(i));
          list[i] = -lose; list[w] += lose;
        }
        s.pay = list;
        list.forEach((v, i) => { if (v !== 0) pay[i] = v; });
      }
      const done = was < 0 && s.winner >= 0;
      return out(Barbut.encode(s), { pay, over: done ? [s.winner] : null, final: done });
    }
    case 'BLACKJACK': {
      if (!secret) return null;
      const s = Blackjack.decode(secret);
      const was = s.phase;
      const staked = s.bets.slice();
      if (Blackjack.move(s, p, op, int('v'), ctx.purse, ctx.host, rnd) !== null) return null;
      const ended = was !== 'done' && s.phase === 'done';
      const playing = s.bets.map((b, i) => (b > 0 ? i : -1)).filter((i) => i >= 0);
      const pay = {};
      if (op !== 'next') s.bets.forEach((b, i) => { const d = staked[i] - b; if (d !== 0) pay[i] = d; });
      if (ended) for (const i of playing) { const back = s.bets[i] + s.result[i]; if (back !== 0) pay[i] = (pay[i] || 0) + back; }
      const profit = {};
      if (ended) for (const i of playing) if (s.result[i] !== 0) profit[i] = s.result[i];
      return out(Blackjack.publicState(s), { secret: Blackjack.encode(s), pay, profit, over: ended ? playing.filter((i) => s.result[i] > 0) : null, played: playing });
    }
    case 'TICTACTOE': case 'CONNECT4': {
      const s = Grid.decode(st);
      if (Grid.play(s, p, int('cell')) !== null) return null;
      const done = s.winner !== -1;
      return out(Grid.encode(s), { over: done ? (s.winner >= 0 ? [s.winner] : []) : null, final: done });
    }
    case 'ROULETTE': {
      const s = Roulette.decode(st);
      const was = s.phase;
      const staked = s.bets.map((b) => b.reduce((a, x) => a + x.amount, 0));
      if (Roulette.move(s, p, op, typeof m.type === 'string' ? m.type : '', int('value'), int('v'), ctx.purse(p), ctx.host, rnd) !== null) return null;
      const spun = was === 'bet' && s.phase === 'spun';
      const playing = s.bets.map((b, i) => (b.length ? i : -1)).filter((i) => i >= 0);
      const pay = {};
      if (op !== 'next') s.bets.forEach((b, i) => { const d = staked[i] - b.reduce((a, x) => a + x.amount, 0); if (d !== 0) pay[i] = d; });
      if (spun) for (const i of playing) { const back = s.pay[i] + s.bets[i].reduce((a, x) => a + x.amount, 0); if (back !== 0) pay[i] = (pay[i] || 0) + back; }
      const profit = {};
      if (spun) for (const i of playing) if (s.pay[i] !== 0) profit[i] = s.pay[i];
      return out(Roulette.encode(s), { pay, profit, over: spun ? playing.filter((i) => s.pay[i] > 0) : null, played: playing });
    }
    case 'SLOTS': {
      const s = Slots.decode(st);
      if (op !== 'spin') return null;
      const d = Slots.spin(s, int('v'), ctx.purse(p), rnd);
      if (d === null) return null;
      return out(Slots.encode(s), { pay: d !== 0 ? { [p]: d } : {}, over: d > 0 ? [p] : [], played: [p] });
    }
    default: return null;
  }
}

export { mustInt };
