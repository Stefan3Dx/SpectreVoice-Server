// Barbut (SmallGames.kt), European roulette and the slot machine (Casino.kt), the same states on the bus.

import { mustInt, toInt, intOr, ints, bools, mustList, mustMap, defaultRnd } from './util.js';

// ====== Barbut: everyone throws two dice; a double beats any other throw, then the higher sum; ties throw again ======

export const Barbut = {
  TARGET: 5,
  STAKE: 50,
  rank: (a, b) => (a === b ? 100 + a : a + b),

  start(players, stake = 50) {
    return { wins: Array(players).fill(0), throwers: [...Array(players).keys()], turn: 0, rolls: {}, round: 1, winner: -1, lastRound: '', stake, roundWinner: -1, pay: Array(players).fill(0) };
  },

  setStake(s, v) {
    if (!(v >= 0 && v <= 1000000)) return 'stake';
    if (s.winner >= 0 || s.turn !== 0 || s.throwers.length !== s.wins.length) return 'mid-round';
    s.stake = v;
    return null;
  },

  encode(s) {
    const rolls = {};
    for (const [k, v] of Object.entries(s.rolls)) rolls[String(k)] = v.slice();
    return { wins: s.wins.slice(), throwers: s.throwers.slice(), turn: s.turn, rolls, round: s.round, winner: s.winner, last: s.lastRound, stake: s.stake, rw: s.roundWinner, pay: s.pay.slice() };
  },

  decode(m) {
    const wins = ints(m.wins);
    const rolls = {};
    for (const [k, v] of Object.entries(mustMap(m.rolls))) {
      if (!/^-?\d+$/.test(k)) throw new TypeError('roll seat');
      rolls[+k] = ints(v);
    }
    return {
      wins, throwers: ints(m.throwers), turn: mustInt(m.turn), rolls, round: mustInt(m.round), winner: intOr(m.winner, -1),
      lastRound: typeof m.last === 'string' ? m.last : '', stake: intOr(m.stake, 50), roundWinner: intOr(m.rw, -1),
      pay: Array.isArray(m.pay) ? m.pay.map((x) => intOr(x, 0)) : Array(wins.length).fill(0),
    };
  },

  // Player p throws (when it's them); names for the round's line. null when done, else why not
  roll(s, p, names, rnd = defaultRnd) {
    if (s.winner >= 0) return 'over';
    if (s.throwers[s.turn] !== p || s.turn < 0) return 'not your turn';
    const a = rnd.nextInt(1, 7), b = rnd.nextInt(1, 7);
    if (s.turn === 0 && s.throwers.length === s.wins.length) { s.roundWinner = -1; s.pay = Array(s.wins.length).fill(0); }
    s.rolls[p] = [a, b];
    s.turn++;
    if (s.turn < s.throwers.length) return null;
    const rk = (i) => Barbut.rank(s.rolls[i][0], s.rolls[i][1]);
    const best = Math.max(...s.throwers.map(rk));
    const top = s.throwers.filter((i) => rk(i) === best);
    const name = (i) => names[i] ?? '?';
    if (top.length > 1) {
      s.lastRound = 'Egalitate: ' + top.map(name).join(', ') + ' dau din nou';
      s.throwers = top.slice();
    } else {
      const w = top[0];
      s.roundWinner = w;
      s.wins[w]++;
      s.lastRound = `Runda ${s.round}: ${name(w)} (${s.rolls[w][0]}-${s.rolls[w][1]})`;
      if (s.wins[w] >= Barbut.TARGET) s.winner = w;
      s.round++;
      s.throwers = [...s.wins.keys()];
    }
    s.turn = 0;
    return null;
  },
};

// ====== Roulette: one zero; straight 35:1, dozens and columns 2:1, the even chances 1:1 ======

const RED = new Set([1, 3, 5, 7, 9, 12, 14, 16, 18, 19, 21, 23, 25, 27, 30, 32, 34, 36]);

export const Roulette = {
  RED,
  WHEEL: [0, 32, 15, 19, 4, 21, 2, 25, 17, 34, 6, 27, 13, 36, 11, 30, 8, 23, 10, 5, 24, 16, 33, 1, 20, 14, 31, 9, 22, 18, 29, 7, 28, 12, 35, 3, 26],

  start(players) {
    return { bets: Array.from({ length: players }, () => []), ready: Array(players).fill(false), phase: 'bet', result: -1, history: [], pay: Array(players).fill(0), round: 1, left: Array(players).fill(false) };
  },

  encode(s) {
    return { bets: s.bets.map((b) => b.map((x) => [x.type, x.value, x.amount])), ready: s.ready.slice(), phase: s.phase, result: s.result, history: s.history.slice(), pay: s.pay.slice(), round: s.round, left: s.left.slice() };
  },

  decode(m) {
    const bets = mustList(m.bets).map((b) => mustList(b).map((x) => {
      const l = mustList(x);
      if (typeof l[0] !== 'string') throw new TypeError('bet type');
      return { type: l[0], value: mustInt(l[1]), amount: mustInt(l[2]) };
    }));
    if (typeof m.phase !== 'string') throw new TypeError('phase');
    return {
      bets, ready: bools(m.ready), phase: m.phase, result: mustInt(m.result), history: ints(m.history), pay: ints(m.pay), round: mustInt(m.round),
      left: Array.isArray(m.left) ? bools(m.left) : Array(bets.length).fill(false),
    };
  },

  multiplier(type, value, n) {
    switch (type) {
      case 'n': return n === value ? 35 : 0;
      case 'red': return RED.has(n) ? 1 : 0;
      case 'black': return n !== 0 && !RED.has(n) ? 1 : 0;
      case 'even': return n !== 0 && n % 2 === 0 ? 1 : 0;
      case 'odd': return n % 2 === 1 ? 1 : 0;
      case 'low': return n >= 1 && n <= 18 ? 1 : 0;
      case 'high': return n >= 19 && n <= 36 ? 1 : 0;
      case 'dozen': return n !== 0 && Math.floor((n - 1) / 12) + 1 === value ? 2 : 0;
      case 'column': return n !== 0 && ((n - 1) % 3) + 1 === value ? 2 : 0;
      default: return 0;
    }
  },

  valid(type, value) {
    switch (type) {
      case 'n': return value >= 0 && value <= 36;
      case 'red': case 'black': case 'even': case 'odd': case 'low': case 'high': return true;
      case 'dozen': case 'column': return value >= 1 && value <= 3;
      default: return false;
    }
  },

  // Player p: "bet" (type, value, amount), "undo", "clear", "ready", "spin" (the keeper), "next", "leave"
  move(s, p, op, type, value, amount, purse, host, rnd = defaultRnd) {
    switch (op) {
      case 'bet':
        if (s.phase !== 'bet' || s.ready[p]) return 'not now';
        if (!Roulette.valid(type, value) || amount < 1) return 'bet';
        if (amount > purse) return 'purse';
        if (s.bets[p].length >= 40) return 'too many';
        s.bets[p].push({ type, value, amount });
        break;
      case 'undo': if (s.phase !== 'bet' || s.ready[p]) return 'not now'; s.bets[p].pop(); break;
      case 'clear': if (s.phase !== 'bet' || s.ready[p]) return 'not now'; s.bets[p].length = 0; break;
      case 'ready': case 'leave':
        if (op === 'leave') s.left[p] = true;
        if (s.phase !== 'bet') return op === 'leave' ? null : 'not now';
        s.ready[p] = true;
        if (s.ready.every(Boolean) && s.bets.some((b) => b.length)) Roulette.spin(s, rnd);
        break;
      case 'spin':
        if (s.phase !== 'bet' || !host || s.bets.every((b) => !b.length)) return 'not now';
        Roulette.spin(s, rnd);
        break;
      case 'next':
        if (s.phase !== 'spun') return 'not now';
        s.phase = 'bet'; s.round++;
        for (let i = 0; i < s.bets.length; i++) { s.bets[i] = []; s.ready[i] = s.left[i]; s.pay[i] = 0; }
        break;
      default: return 'what?';
    }
    return null;
  },

  spin(s, rnd = defaultRnd) {
    const n = rnd.nextInt(37);
    s.result = n;
    s.history.unshift(n);
    while (s.history.length > 14) s.history.pop();
    for (let i = 0; i < s.bets.length; i++) s.pay[i] = s.bets[i].reduce((a, b) => { const k = Roulette.multiplier(b.type, b.value, n); return a + (k > 0 ? b.amount * k : -b.amount); }, 0);
    s.phase = 'spun';
  },
};

// ====== Slots: three reels; three alike pays big, two alike gives the stake back ======

const WEIGHTS = [300, 250, 200, 120, 70, 40, 15, 5];
export const Slots = {
  SYMBOLS: ['🍒', '🍋', '🍊', '🍇', '🔔', '⭐', '7', '💎'],
  THREE: [6, 8, 12, 16, 25, 50, 150, 500],
  start: () => ({ reels: [6, 6, 6], stake: 10, last: 0, spins: 0, history: [] }),
  encode: (s) => ({ reels: s.reels.slice(), stake: s.stake, last: s.last, spins: s.spins, history: s.history.slice() }),
  decode: (m) => ({ reels: ints(m.reels), stake: mustInt(m.stake), last: mustInt(m.last), spins: mustInt(m.spins), history: ints(m.history) }),
  pick(rnd) {
    let r = rnd.nextInt(WEIGHTS.reduce((a, b) => a + b, 0));
    for (let i = 0; i < WEIGHTS.length; i++) { if (r < WEIGHTS[i]) return i; r -= WEIGHTS[i]; }
    return 0;
  },
  payout(reels, stake) {
    const [a, b, c] = reels;
    if (a === b && b === c) return stake * Slots.THREE[a];
    if (a === b || b === c || a === c) return stake;
    return 0;
  },
  // Spins for stake: the change to the purse; null when it can't
  spin(s, stake, purse, rnd = defaultRnd) {
    if (stake < 1 || stake > purse) return null;
    s.reels = [Slots.pick(rnd), Slots.pick(rnd), Slots.pick(rnd)];
    s.stake = stake;
    s.last = Slots.payout(s.reels, stake) - stake;
    s.spins++;
    s.history.unshift(s.last);
    while (s.history.length > 10) s.history.pop();
    return s.last;
  },
};

export { toInt };
