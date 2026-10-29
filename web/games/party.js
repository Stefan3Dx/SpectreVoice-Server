// Two of the apps' party games (Party.kt): rounds against the clock, kept by the table's host. Each keeps what
// everyone sees (pub) and what only the host knows (sec); "ph" is the phase, "dl" when the host moves it on by itself
// (a "tick" from no seat), "sc" the points. Trivia: ten questions, four options, points for being right and fast.
// Hangman: five words, the first and last letters given, a letter or the whole word per turn, six lives.

import { TRIVIA, HANGMAN } from './partydata.js';
import { defaultRnd, shuffled } from './util.js';

// The JSON-like maps as Party.kt reads them
const int = (m, k) => (typeof m[k] === 'number' && Number.isFinite(m[k]) ? Math.max(-2147483648, Math.min(2147483647, Math.trunc(m[k]))) : 0);
const long = (m, k) => (typeof m[k] === 'number' && Number.isFinite(m[k]) ? Math.trunc(m[k]) : 0);
const str = (m, k) => (typeof m[k] === 'string' ? m[k] : '');
function ints(m, k) { const l = (Array.isArray(m[k]) ? m[k] : []).map((x) => (typeof x === 'number' ? Math.trunc(x) : 0)); m[k] = l; return l; }
function list(m, k) { if (!Array.isArray(m[k])) m[k] = []; return m[k]; }
function map(m, k) { if (!m[k] || typeof m[k] !== 'object' || Array.isArray(m[k])) m[k] = {}; return m[k]; }
const deep = (v) => (v === undefined ? {} : JSON.parse(JSON.stringify(v ?? {})));

// Lower case, no diacritics, letters/digits and single spaces only: how guesses are compared
const FOLD = { 'ă': 'a', 'â': 'a', 'á': 'a', 'à': 'a', 'ä': 'a', 'î': 'i', 'í': 'i', 'ï': 'i', 'ș': 's', 'ş': 's', 'ț': 't', 'ţ': 't', 'é': 'e', 'è': 'e', 'ë': 'e', 'ó': 'o', 'ö': 'o', 'ú': 'u', 'ü': 'u' };
const letterOrDigit = (c) => /^[\p{L}\p{Nd}]$/u.test(c);
export function fold(s) {
  let out = '';
  for (const ch of String(s).toLowerCase().split('')) {
    const c = FOLD[ch] || ch;
    if (letterOrDigit(c)) out += c; else if (out && out[out.length - 1] !== ' ') out += ' ';
  }
  return out.trim();
}

const out = (pub, sec, o = {}) => ({ pub, sec, privates: {}, over: null, final: false, ...o });
function end(pub, sec) {
  pub.ph = 'end'; pub.dl = 0;
  const sc = ints(pub, 'sc');
  const best = sc.length ? Math.max(...sc) : 0;
  return out(pub, sec, { over: best > 0 ? sc.map((v, i) => (v === best ? i : -1)).filter((i) => i >= 0) : [], final: true });
}
const due = (pub, now) => { const dl = long(pub, 'dl'); return dl > 0 && now >= dl; };

// ====== Trivia ======

export const Trivia = {
  Q_MS: 15000, RES_MS: 5000, ROUNDS: 10,
  start(n, now, rnd = defaultRnd) {
    const pub = { rd: 0, of: Trivia.ROUNDS, sc: Array(n).fill(0) };
    const order = shuffled([...TRIVIA.keys()], rnd).slice(0, Trivia.ROUNDS);
    pub.of = order.length;
    return Trivia.round(pub, { order }, n, now);
  },
  round(pub, sec, n, now) {
    const q = TRIVIA[ints(sec, 'order')[int(pub, 'rd')]];
    sec.answer = q[2]; sec.picks = {};
    pub.ph = 'q'; pub.q = q[0]; pub.opts = q[1].slice(); pub.cat = q[3]; pub.dl = now + Trivia.Q_MS; pub.start = now;
    pub.done = []; pub.answer = -1; pub.picks = []; pub.gain = [];
    return out(pub, sec);
  },
  act(pub, sec, p, op, m, n, host, now) {
    const ph = str(pub, 'ph');
    if (op === 'answer') {
      if (ph !== 'q' || p < 0) return null;
      if (typeof m.i !== 'number') return null;
      const i = Math.trunc(m.i);
      if (i < 0 || i > 3) return null;
      const picks = map(sec, 'picks');
      if (String(p) in picks) return null;
      picks[String(p)] = [i, now];
      ints(pub, 'done').push(p);
      return Object.keys(picks).length >= n ? Trivia.results(pub, sec, n, now) : out(pub, sec);
    }
    if (op === 'tick') {
      if (!due(pub, now)) return null;
      if (ph === 'q') return Trivia.results(pub, sec, n, now);
      if (ph === 'res') { if (int(pub, 'rd') + 1 >= int(pub, 'of')) return end(pub, sec); pub.rd = int(pub, 'rd') + 1; return Trivia.round(pub, sec, n, now); }
      return null;
    }
    return null;
  },
  results(pub, sec, n, now) {
    const answer = int(sec, 'answer');
    const start = long(pub, 'start');
    const picks = Array(n).fill(-1), gain = Array(n).fill(0);
    for (const [k, v] of Object.entries(map(sec, 'picks'))) {
      if (!Array.isArray(v)) continue;
      const seat = Number(k);
      const i = Math.trunc(v[0]), at = Math.trunc(v[1]);
      picks[seat] = i;
      if (i === answer) gain[seat] = 500 + Math.floor((500 * Math.min(Trivia.Q_MS, Math.max(0, Trivia.Q_MS - (at - start)))) / Trivia.Q_MS);
    }
    const sc = ints(pub, 'sc');
    for (let i = 0; i < n; i++) sc[i] += gain[i];
    pub.answer = answer; pub.picks = picks; pub.gain = gain;
    pub.ph = 'res'; pub.dl = now + Trivia.RES_MS;
    return out(pub, sec);
  },
};

// ====== Hangman ======

const isLetter = (c) => /^\p{L}$/u.test(c);
export const Hangman = {
  TURN_MS: 20000, RES_MS: 6000, ROUNDS: 5, LIVES: 6,
  LETTERS: 'aăâbcdefghiîjklmnopqrsștțuvwxyz'.split(''),
  mask: (w, shown) => w.split('').map((c) => (!isLetter(c) || shown.has(c) ? c : '_')).join(''),
  start(n, now, rnd = defaultRnd) {
    const pub = { rd: 0, of: Hangman.ROUNDS, sc: Array(n).fill(0), turn: rnd.nextInt(n) };
    return Hangman.round(pub, { used: [] }, now, rnd);
  },
  round(pub, sec, now, rnd) {
    const used = list(sec, 'used').map(String);
    let pool = HANGMAN.filter((x) => !used.includes(x[0]));
    if (!pool.length) pool = HANGMAN;
    const [word, cat] = pool[rnd.nextInt(pool.length)];
    list(sec, 'used').push(word);
    const w = word.toLowerCase();
    sec.word = w;
    const shown = [...new Set([w[0], w[w.length - 1]])];
    pub.ph = 'guess'; pub.cat = cat; pub.tried = shown.slice(); pub.wrong = [];
    pub.miss = 0; pub.mask = Hangman.mask(w, new Set(shown)); pub.word = ''; pub.dl = now + Hangman.TURN_MS; pub.solved = -1;
    return out(pub, sec);
  },
  act(pub, sec, p, op, m, n, host, now, rnd = defaultRnd) {
    const ph = str(pub, 'ph');
    const w = str(sec, 'word');
    if (op === 'letter') {
      if (ph !== 'guess' || p !== int(pub, 'turn')) return null;
      if (typeof m.v !== 'string' || !m.v.length) return null;
      const l = m.v.toLowerCase()[0];
      if (!Hangman.LETTERS.includes(l)) return null;
      const tried = list(pub, 'tried');
      if (tried.includes(l)) return null;
      tried.push(l);
      const shown = new Set(tried.filter((x) => typeof x === 'string' && x.length).map((x) => x[0]));
      pub.mask = Hangman.mask(w, shown);
      const sc = ints(pub, 'sc');
      const count = w.split('').filter((c) => c === l).length;
      if (count > 0) {
        sc[p] += 10 * count;
        pub.dl = now + Hangman.TURN_MS;
        if (!str(pub, 'mask').includes('_')) return Hangman.results(pub, sec, now, p);
        return out(pub, sec);
      }
      list(pub, 'wrong').push(l);
      return Hangman.miss(pub, sec, n, now);
    }
    if (op === 'solve') {
      if (ph !== 'guess' || p !== int(pub, 'turn')) return null;
      const text = (typeof m.v === 'string' ? m.v : '').trim();
      if (!text) return null;
      if (fold(text) === fold(w)) {
        const hidden = str(pub, 'mask').split('').filter((c) => c === '_').length;
        ints(pub, 'sc')[p] += 20 + 10 * hidden;
        return Hangman.results(pub, sec, now, p);
      }
      return Hangman.miss(pub, sec, n, now);
    }
    if (op === 'tick') {
      if (!due(pub, now)) return null;
      if (ph === 'guess') return Hangman.miss(pub, sec, n, now);
      if (ph === 'res') { if (int(pub, 'rd') + 1 >= int(pub, 'of')) return end(pub, sec); pub.rd = int(pub, 'rd') + 1; return Hangman.round(pub, sec, now, rnd); }
      return null;
    }
    return null;
  },
  miss(pub, sec, n, now) {
    const miss = int(pub, 'miss') + 1;
    pub.miss = miss;
    if (miss >= Hangman.LIVES) return Hangman.results(pub, sec, now, -1);
    pub.turn = (int(pub, 'turn') + 1) % n;
    pub.dl = now + Hangman.TURN_MS;
    return out(pub, sec);
  },
  results(pub, sec, now, solved) {
    pub.ph = 'res'; pub.word = str(sec, 'word'); pub.mask = str(sec, 'word'); pub.solved = solved; pub.dl = now + Hangman.RES_MS;
    return out(pub, sec);
  },
};

export const PARTIES = { TRIVIA: Trivia, HANGMAN: Hangman };

// As Rules.kt does for a party game: the state and the secret copied deep, the outcome with every seat as played
export function partyStart(kind, n, now, rnd) {
  return toOutcome(PARTIES[kind].start(n, now, rnd), n);
}
export function partyMove(kind, st, secret, p, m, n, host, now, rnd) {
  const r = PARTIES[kind].act(deep(st), deep(secret), p, typeof m.op === 'string' ? m.op : '', m, n, host, now, rnd);
  return r ? toOutcome(r, n) : null;
}
const toOutcome = (r, n) => ({ state: r.pub, secret: r.sec, pay: {}, profit: null, over: r.over, played: r.over !== null ? [...Array(n).keys()] : null, final: r.final, privates: r.privates });
