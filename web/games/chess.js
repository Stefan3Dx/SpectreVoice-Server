// Chess with all its rules, as Chess.kt: castling, en passant, promotion, check, checkmate, stalemate, the 50 moves,
// the threefold repetition and too little material. The board is 64 squares, a8 first (0) to h1 (63); white in
// capitals. State on the bus: {board, white, castling, ep, quiet, lf, lt} plus the table's "hist" and "result".

import { toInt, mustInt } from './util.js';

export const START = 'rnbqkbnrpppppppp................................PPPPPPPPRNBQKBNR';

const isWhite = (c) => c !== '.' && c === c.toUpperCase() && c !== c.toLowerCase();
const lower = (c) => c.toLowerCase();
const mine = (c, w) => c !== '.' && isWhite(c) === w;
const KNIGHT = [[1, 2], [2, 1], [-1, 2], [-2, 1], [1, -2], [2, -1], [-1, -2], [-2, -1]];
const DIAG = [[1, 1], [1, -1], [-1, 1], [-1, -1]];
const STRAIGHT = [[1, 0], [-1, 0], [0, 1], [0, -1]];
const ALL = [...DIAG, ...STRAIGHT];
const on = (r, f) => r >= 0 && r <= 7 && f >= 0 && f <= 7;

export const squareName = (i) => String.fromCharCode(97 + (i % 8)) + (8 - Math.floor(i / 8));

export class Chess {
  constructor(board = START.split(''), white = true, castling = 'KQkq', ep = -1, quiet = 0, last = null) {
    this.board = board; this.white = white; this.castling = castling; this.ep = ep; this.quiet = quiet; this.last = last;
  }

  static decode(m) {
    if (typeof m.board !== 'string') throw new TypeError('board');
    const lf = toInt(m.lf);
    return new Chess(m.board.split(''), m.white === true, typeof m.castling === 'string' ? m.castling : '',
      toInt(m.ep) ?? -1, toInt(m.quiet) ?? 0, lf === null ? null : [lf, toInt(m.lt) ?? 0]);
  }

  encode() {
    return { board: this.board.join(''), white: this.white, castling: this.castling, ep: this.ep, quiet: this.quiet, lf: this.last ? this.last[0] : null, lt: this.last ? this.last[1] : null };
  }

  copy() { return new Chess(this.board.slice(), this.white, this.castling, this.ep, this.quiet, this.last ? this.last.slice() : null); }

  attacked(sq, byWhite) {
    const b = this.board;
    const r = Math.floor(sq / 8), f = sq % 8;
    const at = (rr, ff) => (on(rr, ff) ? b[rr * 8 + ff] : '#');
    const pr = byWhite ? r + 1 : r - 1;
    for (const df of [-1, 1]) if (at(pr, f + df) === (byWhite ? 'P' : 'p')) return true;
    for (const [dr, df] of KNIGHT) if (at(r + dr, f + df) === (byWhite ? 'N' : 'n')) return true;
    for (let dr = -1; dr <= 1; dr++) for (let df = -1; df <= 1; df++) if ((dr !== 0 || df !== 0) && at(r + dr, f + df) === (byWhite ? 'K' : 'k')) return true;
    const rook = byWhite ? 'RQ' : 'rq', bishop = byWhite ? 'BQ' : 'bq';
    for (const [dirs, kinds] of [[STRAIGHT, rook], [DIAG, bishop]]) {
      for (const [dr, df] of dirs) {
        let rr = r + dr, ff = f + df;
        while (on(rr, ff)) { const c = b[rr * 8 + ff]; if (c !== '.') { if (kinds.includes(c)) return true; break; } rr += dr; ff += df; }
      }
    }
    return false;
  }

  inCheck(w) {
    const k = this.board.indexOf(w ? 'K' : 'k');
    return k >= 0 && this.attacked(k, !w);
  }

  pseudo(from) {
    const b = this.board;
    const c = b[from];
    if (c === '.') return [];
    const w = isWhite(c);
    const r = Math.floor(from / 8), f = from % 8;
    const out = [];
    const free = (rr, ff) => on(rr, ff) && b[rr * 8 + ff] === '.';
    const enemy = (rr, ff) => on(rr, ff) && b[rr * 8 + ff] !== '.' && isWhite(b[rr * 8 + ff]) !== w;
    const slide = (dirs, far) => {
      for (const [dr, df] of dirs) {
        let rr = r + dr, ff = f + df;
        while (on(rr, ff)) {
          const t = b[rr * 8 + ff];
          if (t === '.') out.push(rr * 8 + ff); else { if (isWhite(t) !== w) out.push(rr * 8 + ff); break; }
          if (!far) break;
          rr += dr; ff += df;
        }
      }
    };
    switch (lower(c)) {
      case 'p': {
        const dir = w ? -1 : 1;
        if (free(r + dir, f)) {
          out.push((r + dir) * 8 + f);
          const home = w ? 6 : 1;
          if (r === home && free(r + 2 * dir, f)) out.push((r + 2 * dir) * 8 + f);
        }
        for (const df of [-1, 1]) {
          if (enemy(r + dir, f + df)) out.push((r + dir) * 8 + f + df);
          if (on(r + dir, f + df) && (r + dir) * 8 + f + df === this.ep) out.push(this.ep);
        }
        break;
      }
      case 'n':
        for (const [dr, df] of KNIGHT) { const rr = r + dr, ff = f + df; if (on(rr, ff) && !mine(b[rr * 8 + ff], w)) out.push(rr * 8 + ff); }
        break;
      case 'b': slide(DIAG, true); break;
      case 'r': slide(STRAIGHT, true); break;
      case 'q': slide(ALL, true); break;
      case 'k': {
        slide(ALL, false);
        const home = w ? 60 : 4;
        if (from === home && !this.attacked(home, !w)) {
          const ks = w ? 'K' : 'k', qs = w ? 'Q' : 'q', rk = w ? 'R' : 'r';
          if (this.castling.includes(ks) && b[home + 1] === '.' && b[home + 2] === '.' && b[home + 3] === rk && !this.attacked(home + 1, !w) && !this.attacked(home + 2, !w)) out.push(home + 2);
          if (this.castling.includes(qs) && b[home - 1] === '.' && b[home - 2] === '.' && b[home - 3] === '.' && b[home - 4] === rk && !this.attacked(home - 1, !w) && !this.attacked(home - 2, !w)) out.push(home - 2);
        }
        break;
      }
    }
    return out;
  }

  legal(from) {
    if (!(from >= 0 && from < 64) || !mine(this.board[from], this.white)) return [];
    return this.pseudo(from).filter((to) => { const d = this.copy(); d.apply(from, to, 'q'); return !d.inCheck(this.white); });
  }

  anyLegal() { for (let i = 0; i < 64; i++) if (this.legal(i).length) return true; return false; }

  apply(from, to, promo) {
    const b = this.board;
    const c = b[from];
    const w = isWhite(c);
    const captured = b[to];
    let capture = captured !== '.';
    if (lower(c) === 'p' && to === this.ep && captured === '.') { b[to + (w ? 8 : -8)] = '.'; capture = true; }
    b[to] = c;
    b[from] = '.';
    if (lower(c) === 'k' && Math.abs(to - from) === 2) {
      if (to > from) { b[to - 1] = b[to + 1]; b[to + 1] = '.'; } else { b[to + 1] = b[to - 2]; b[to - 2] = '.'; }
    }
    if (lower(c) === 'p' && (Math.floor(to / 8) === 0 || Math.floor(to / 8) === 7)) {
      let p = lower(promo || 'q');
      if (!'qrbn'.includes(p) || p.length !== 1) p = 'q';
      b[to] = w ? p.toUpperCase() : p;
    }
    this.ep = lower(c) === 'p' && Math.abs(to - from) === 16 ? (from + to) / 2 : -1;
    const drop = (ch) => { this.castling = this.castling.split(ch).join(''); };
    switch (from) { case 60: drop('K'); drop('Q'); break; case 4: drop('k'); drop('q'); break; case 63: drop('K'); break; case 56: drop('Q'); break; case 7: drop('k'); break; case 0: drop('q'); break; }
    switch (to) { case 63: drop('K'); break; case 56: drop('Q'); break; case 7: drop('k'); break; case 0: drop('q'); break; }
    this.quiet = capture || lower(c) === 'p' ? 0 : this.quiet + 1;
    this.white = !this.white;
    this.last = [from, to];
  }

  san(from, to, promo = 'q') {
    const b = this.board;
    const c = b[from];
    const piece = c.toUpperCase();
    const capture = b[to] !== '.' || (piece === 'P' && to === this.ep);
    let text;
    if (piece === 'K' && Math.abs(to - from) === 2) text = to > from ? 'O-O' : 'O-O-O';
    else if (piece === 'P') {
      const pr = 'QRBN'.includes(String(promo).toUpperCase()) && String(promo).length === 1 ? String(promo).toUpperCase() : 'Q';
      text = (capture ? String.fromCharCode(97 + (from % 8)) + 'x' : '') + squareName(to) + (Math.floor(to / 8) === 0 || Math.floor(to / 8) === 7 ? '=' + pr : '');
    } else {
      const others = [];
      for (let i = 0; i < 64; i++) if (i !== from && b[i] === c && this.legal(i).includes(to)) others.push(i);
      let dis = '';
      if (others.length) {
        if (!others.some((o) => o % 8 === from % 8)) dis = String.fromCharCode(97 + (from % 8));
        else if (!others.some((o) => Math.floor(o / 8) === Math.floor(from / 8))) dis = String(8 - Math.floor(from / 8));
        else dis = squareName(from);
      }
      text = piece + dis + (capture ? 'x' : '') + squareName(to);
    }
    const after = this.copy();
    after.apply(from, to, promo);
    const chk = after.inCheck(after.white);
    return text + (chk && !after.anyLegal() ? '#' : chk ? '+' : '');
  }

  result(history = []) {
    if (!this.anyLegal()) return this.inCheck(this.white) ? (this.white ? 'black' : 'white') : 'draw:stalemate';
    if (this.quiet >= 100) return 'draw:50';
    if (this.insufficient()) return 'draw:material';
    if (this.repeated(history)) return 'draw:repetition';
    return null;
  }

  insufficient() {
    const rest = [];
    for (let i = 0; i < 64; i++) if (this.board[i] !== '.' && lower(this.board[i]) !== 'k') rest.push(i);
    if (!rest.length) return true;
    if (rest.length === 1 && 'bn'.includes(lower(this.board[rest[0]]))) return true;
    if (rest.every((i) => lower(this.board[i]) === 'b')) return new Set(rest.map((i) => (Math.floor(i / 8) + (i % 8)) % 2)).size === 1;
    return false;
  }

  key() {
    let epLive = false;
    if (this.ep >= 0) for (let i = 0; i < 64 && !epLive; i++) if (this.board[i] === (this.white ? 'P' : 'p') && this.pseudo(i).includes(this.ep)) epLive = true;
    return this.board.join('') + (this.white ? 'w' : 'b') + this.castling + (epLive ? this.ep : -1);
  }

  repeated(history) {
    const k = this.key();
    let n = 0;
    for (const h of history) if (h.key() === k) n++;
    return n >= 2;
  }
}

// The table's "hist": [[from, to, "q"], …] as moves; throws like the Kotlin when an entry isn't one
export function histMoves(hist) {
  return (Array.isArray(hist) ? hist : []).map((x) => {
    if (!Array.isArray(x)) throw new TypeError('hist');
    const p = typeof x[2] === 'string' && x[2].length ? x[2][0] : 'q';
    return [mustInt(x[0]), mustInt(x[1]), p];
  });
}

// The positions after each move (the start first)
export function positions(moves) {
  const out = [];
  let c = new Chess();
  out.push(c.copy());
  for (const [f, t, p] of moves) { c = c.copy(); c.apply(f, t, p); out.push(c); }
  return out;
}

export function sans(moves) {
  const c = new Chess();
  return moves.map(([f, t, p]) => { const s = c.san(f, t, p); c.apply(f, t, p); return s; });
}

// FEN to a position (board, side, castling, en passant), for the tests
export function fromFen(f) {
  const parts = f.split(' ');
  let b = '';
  for (const ch of parts[0]) { if (ch === '/') continue; if (/\d/.test(ch)) b += '.'.repeat(+ch); else b += ch; }
  const ep = parts[3] === '-' ? -1 : (8 - (+parts[3][1])) * 8 + (parts[3].charCodeAt(0) - 97);
  return new Chess(b.split(''), parts[1] === 'w', parts[2] === '-' ? '' : parts[2], ep);
}
