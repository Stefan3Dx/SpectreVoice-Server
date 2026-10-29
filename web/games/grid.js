// X și 0 (3×3) and 4 în linie (7 columns × 6 rows), as SmallGames.kt's Grid: two players take turns.
// State on the bus: {cols, rows, need, cells, turn, winner, line, gravity}; winner −1 playing, −2 a draw.

import { mustInt, toInt, ints } from './util.js';

export const ticTacToe = () => ({ cols: 3, rows: 3, need: 3, cells: Array(9).fill(-1), turn: 0, winner: -1, line: [], gravity: false });
export const connectFour = () => ({ cols: 7, rows: 6, need: 4, cells: Array(42).fill(-1), turn: 0, winner: -1, line: [], gravity: true });

export const encode = (s) => ({ cols: s.cols, rows: s.rows, need: s.need, cells: s.cells.slice(), turn: s.turn, winner: s.winner, line: s.line.slice(), gravity: s.gravity });

export function decode(m) {
  return {
    cols: mustInt(m.cols), rows: mustInt(m.rows), need: mustInt(m.need), cells: ints(m.cells), turn: mustInt(m.turn),
    winner: toInt(m.winner) ?? -1, line: Array.isArray(m.line) ? ints(m.line) : [], gravity: m.gravity === true,
  };
}

// Player p plays cell (with gravity: the column). null when done, else why not.
export function play(s, p, cell) {
  if (s.winner !== -1) return 'over';
  if (p !== s.turn) return 'not your turn';
  let at;
  if (s.gravity) {
    if (!(cell >= 0 && cell < s.cols)) return 'column';
    at = -1;
    for (let r = s.rows - 1; r >= 0; r--) if (s.cells[r * s.cols + cell] === -1) { at = r * s.cols + cell; break; }
    if (at < 0) return 'full';
  } else {
    if (!(cell >= 0 && cell < s.cells.length) || s.cells[cell] !== -1) return 'taken';
    at = cell;
  }
  s.cells[at] = p;
  const r = Math.floor(at / s.cols), c = at % s.cols;
  for (const [dr, dc] of [[0, 1], [1, 0], [1, 1], [1, -1]]) {
    const line = [at];
    for (const sign of [1, -1]) {
      let rr = r + dr * sign, cc = c + dc * sign;
      while (rr >= 0 && rr < s.rows && cc >= 0 && cc < s.cols && s.cells[rr * s.cols + cc] === p) { line.push(rr * s.cols + cc); rr += dr * sign; cc += dc * sign; }
    }
    if (line.length >= s.need) { s.winner = p; s.line = line; return null; }
  }
  if (!s.cells.includes(-1)) s.winner = -2;
  s.turn = 1 - s.turn;
  return null;
}
