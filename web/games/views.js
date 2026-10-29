// The web client's game boards (DOM): one function per kind, given {t (the table), st (its state), seat (yours, −1
// watching), host (you keep it), purse (your Lei), priv (what the keeper sent only you), play(move), ui (this
// table's own view state), redraw()}.
// Only what's on screen lives here; the rules are in rules.js and the others' moves come from the keeper.

import { el } from '../core.js';
import { t } from '../i18n.js';
import * as Grid from './grid.js';
import { Chess, histMoves, sans } from './chess.js';
import { Barbut, Roulette, Slots } from './casino.js';
import { Blackjack } from './blackjack.js';
import { Backgammon } from './backgammon.js';
import { Poker } from './poker.js';
import { Hangman } from './party.js';
import { fmtLei } from './hub.js';

const name = (v, i) => v.t.players[i]?.name ?? '?';
const banner = (text, cls) => el('div', { class: 'gm-banner' + (cls ? ' ' + cls : ''), text });
function seatChip(v, i, active, under, badge) {
  const p = v.t.players[i];
  return el('div', { class: 'gm-seat' + (active ? ' on' : '') + (i === v.seat ? ' me' : '') },
    el('span', { class: 'gm-avatar', text: (p?.name || '?').slice(0, 1).toUpperCase() }),
    el('span', { class: 'gm-seat-txt' }, el('b', { text: p?.name || '?' }), under ? el('small', { text: under }) : null),
    badge ? el('span', { class: 'gm-badge', text: badge }) : null);
}
function stakeInput(v, key, value, max) {
  const inp = el('input', { type: 'number', class: 'gm-stake', min: 1, max: Math.max(1, max), step: 1, value: v.ui[key] ?? value, dataset: { k: v.t.id + ':' + key } });
  inp.oninput = () => { v.ui[key] = inp.value; };
  return inp;
}
const readStake = (v, key, d) => { const x = Math.trunc(Number(v.ui[key] ?? d)); return Number.isFinite(x) ? x : d; };

// ====== Tic-tac-toe and Connect four ======

function grid(v) {
  let s;
  try { s = Grid.decode(v.st); } catch { return banner(t('gBadState'), 'warn'); }
  const myTurn = s.winner === -1 && s.turn === v.seat && v.st.resigned == null;
  const mark = (c) => (c === 0 ? '✕' : c === 1 ? '◯' : '');
  const box = el('div', { class: 'gm-grid ' + (s.gravity ? 'c4' : 'ttt') });
  box.style.gridTemplateColumns = `repeat(${s.cols}, 1fr)`;
  for (let i = 0; i < s.cells.length; i++) {
    const c = s.cells[i];
    const col = i % s.cols;
    const can = myTurn && (s.gravity ? s.cells[col] === -1 : c === -1);
    box.append(el('button', {
      type: 'button', class: 'gm-cell' + (c >= 0 ? ' p' + c : '') + (s.line.includes(i) ? ' win' : ''), disabled: !can,
      text: s.gravity ? '' : mark(c), dataset: { cell: String(i), col: String(col) }, attrs: { 'aria-label': String(i) },
      onclick: () => v.play({ cell: s.gravity ? col : i }),
    }));
  }
  let msg;
  if (s.winner >= 0) msg = t('gWins', { name: name(v, s.winner) });
  else if (s.winner === -2) msg = t('gDraw');
  else msg = myTurn ? t('gYourTurn') : t('gTurnOf', { name: name(v, s.turn) });
  return el('div', { class: 'gm-board-wrap' },
    el('div', { class: 'gm-seats' }, seatChip(v, 0, s.winner === -1 && s.turn === 0, s.gravity ? '🔴' : '✕'), seatChip(v, 1, s.winner === -1 && s.turn === 1, s.gravity ? '🟡' : '◯')),
    box, banner(msg, s.winner >= 0 ? (s.winner === v.seat ? 'good' : v.seat >= 0 ? 'bad' : 'good') : ''));
}

// ====== Chess ======

const GLYPH = { k: '♚', q: '♛', r: '♜', b: '♝', n: '♞', p: '♟' };
const glyph = (c) => (GLYPH[c.toLowerCase()] || '') + '︎';
const VALUE = { p: 1, n: 3, b: 3, r: 5, q: 9 };

function chess(v) {
  let live, hist;
  try { live = Chess.decode(v.st); hist = histMoves(v.st.hist); if (live.board.length !== 64) throw new Error('board'); } catch { return banner(t('gBadState'), 'warn'); }
  const result = typeof v.st.result === 'string' ? v.st.result : null;
  const over = result !== null || v.st.resigned != null;
  const myTurn = !over && ((v.seat === 0 && live.white) || (v.seat === 1 && !live.white));
  const ui = v.ui;
  if (ui._histLen !== hist.length) { ui._histLen = hist.length; ui.sel = -1; ui.promo = null; }
  const flip = v.seat === 1;
  const moves = ui.sel >= 0 && myTurn ? new Set(live.legal(ui.sel)) : new Set();
  const checkKing = !over && live.inCheck(live.white) ? live.board.indexOf(live.white ? 'K' : 'k') : -1;
  const board = el('div', { class: 'gm-chess' + (flip ? ' flip' : '') });
  const send = (from, to, promo) => { ui.sel = -1; ui.promo = null; v.play({ from, to, promo }); };
  for (let row = 0; row < 8; row++) for (let col = 0; col < 8; col++) {
    const i = flip ? (7 - row) * 8 + (7 - col) : row * 8 + col;
    const light = (Math.floor(i / 8) + (i % 8)) % 2 === 0;
    const pc = live.board[i];
    const cls = ['gm-sq', light ? 'l' : 'd'];
    if (live.last && (live.last[0] === i || live.last[1] === i)) cls.push('last');
    if (i === ui.sel) cls.push('sel');
    if (moves.has(i)) cls.push(pc === '.' ? 'to' : 'cap');
    if (i === checkKing) cls.push('check');
    const b = el('button', {
      type: 'button', class: cls.join(' '), dataset: { sq: String(i) }, disabled: !myTurn, attrs: { 'aria-label': String.fromCharCode(97 + (i % 8)) + (8 - Math.floor(i / 8)) },
      onclick: () => {
        if (ui.sel >= 0 && moves.has(i)) {
          const p = live.board[ui.sel];
          if (p.toLowerCase() === 'p' && (Math.floor(i / 8) === 0 || Math.floor(i / 8) === 7)) { ui.promo = [ui.sel, i]; v.redraw(); return; }
          send(ui.sel, i, 'q');
        } else if (pc !== '.' && (pc === pc.toUpperCase()) === live.white) { ui.sel = i; v.redraw(); } else { ui.sel = -1; v.redraw(); }
      },
    });
    if (pc !== '.') b.append(el('span', { class: 'gm-pc ' + (pc === pc.toUpperCase() ? 'w' : 'b'), text: glyph(pc) }));
    if (col === 0) b.append(el('span', { class: 'gm-rank', text: String(8 - Math.floor(i / 8)) }));
    if (row === 7) b.append(el('span', { class: 'gm-file', text: String.fromCharCode(97 + (i % 8)) }));
    board.append(b);
  }
  // Pieces taken by each side
  const count = (s) => { const m = {}; for (const c of s) m[c] = (m[c] || 0) + 1; return m; };
  const start = count('rnbqkbnrpppppppp' + 'PPPPPPPPRNBQKBNR'), now = count(live.board.join(''));
  const taken = (white) => [...'qrbnp'].flatMap((p) => { const ch = white ? p.toUpperCase() : p; return Array(Math.max(0, (start[ch] || 0) - (now[ch] || 0))).fill(ch); });
  const material = taken(false).reduce((a, c) => a + VALUE[c.toLowerCase()], 0) - taken(true).reduce((a, c) => a + VALUE[c.toLowerCase()], 0);
  const top = flip ? 0 : 1, bottom = 1 - top;
  const side = (seatNo) => el('div', { class: 'gm-chess-side' },
    seatChip(v, seatNo, !over && (seatNo === 0) === live.white, seatNo === 0 ? t('gWhite') : t('gBlack')),
    el('span', { class: 'gm-taken', text: taken(seatNo === 1).map(glyph).join('') + ((seatNo === 0 ? material : -material) > 0 ? ' +' + Math.abs(material) : '') }));
  let msg, cls = '';
  if (result === 'white' || result === 'black') {
    const w = result === 'white' ? 0 : 1;
    msg = v.st.resigned != null ? t('gWins', { name: name(v, w) }) : t('gMate', { name: name(v, w) });
    cls = v.seat < 0 || v.seat === w ? 'good' : 'bad';
  } else if (result) msg = t({ 'draw:stalemate': 'gDrawStalemate', 'draw:50': 'gDraw50', 'draw:material': 'gDrawMaterial', 'draw:repetition': 'gDrawRepetition' }[result] || 'gDraw');
  else msg = (myTurn ? t('gYourTurn') : t('gTurnOf', { name: name(v, live.white ? 0 : 1) })) + (live.inCheck(live.white) ? ' · ' + t('gCheck') : '');
  let list = [];
  try { list = sans(hist); } catch { list = []; }
  const moveList = el('ol', { class: 'gm-moves' });
  for (let i = 0; i < list.length; i += 2) moveList.append(el('li', {}, el('span', { text: list[i] }), list[i + 1] ? el('span', { text: list[i + 1] }) : null));
  const promo = ui.promo ? el('div', { class: 'gm-promo' }, el('span', { text: t('gPromote') }),
    ...'qrbn'.split('').map((p) => el('button', { type: 'button', class: 'gm-pc-btn', dataset: { promo: p }, text: glyph(live.white ? p.toUpperCase() : p), onclick: () => send(ui.promo[0], ui.promo[1], p) }))) : null;
  return el('div', { class: 'gm-chess-wrap' },
    el('div', { class: 'gm-chess-main' }, side(top), board, side(bottom), promo, banner(msg, cls)),
    el('div', { class: 'gm-chess-list' }, el('div', { class: 'gm-sub', text: t('gMoves') }), moveList));
}

// ====== Barbut ======

const DIE = ['', '⚀', '⚁', '⚂', '⚃', '⚄', '⚅'];
function barbut(v) {
  let s;
  try { s = Barbut.decode(v.st); } catch { return banner(t('gBadState'), 'warn'); }
  const thrower = s.throwers[s.turn];
  const myTurn = s.winner < 0 && thrower === v.seat && v.st.resigned == null;
  const rows = el('div', { class: 'gm-rows' });
  s.wins.forEach((w, i) => {
    const r = s.rolls[i];
    const pay = s.pay[i] || 0;
    rows.append(el('div', { class: 'gm-row' + (thrower === i && s.winner < 0 ? ' on' : '') + (s.roundWinner === i ? ' won' : '') },
      seatChip(v, i, thrower === i && s.winner < 0, '★'.repeat(w) + '☆'.repeat(Math.max(0, Barbut.TARGET - w))),
      el('span', { class: 'gm-dice', text: r ? DIE[r[0]] + DIE[r[1]] : '' }),
      pay ? el('span', { class: 'gm-pay ' + (pay > 0 ? 'good' : 'bad'), text: (pay > 0 ? '+' : '') + fmtLei(pay) }) : null));
  });
  const kids = [el('div', { class: 'gm-sub', text: t('gStake', { amount: fmtLei(s.stake) }) + ' · ' + t('gRound', { n: s.round }) }), rows];
  if (s.lastRound) kids.push(el('div', { class: 'gm-note', text: s.lastRound }));
  if (s.winner >= 0) kids.push(banner(t('gWins', { name: name(v, s.winner) }), s.winner === v.seat || v.seat < 0 ? 'good' : 'bad'));
  else {
    kids.push(banner(myTurn ? t('gYourTurn') : t('gTurnOf', { name: name(v, thrower) })));
    if (myTurn) kids.push(el('button', { type: 'button', class: 'primary gm-big', id: 'gmRoll', text: '🎲 ' + t('gRoll'), onclick: () => v.play({ op: 'roll' }) }));
    if (v.host && s.turn === 0 && s.throwers.length === s.wins.length) {
      const inp = stakeInput(v, 'stake', s.stake, 1000000);
      inp.min = 0;
      kids.push(el('div', { class: 'row gm-ctl' }, el('span', { class: 'muted', text: t('gSetStake') }), inp,
        el('button', { type: 'button', text: t('gSet'), onclick: () => v.play({ op: 'stake', v: readStake(v, 'stake', s.stake) }) })));
    }
  }
  return el('div', { class: 'gm-casino' }, ...kids);
}

// ====== Roulette ======

const CHIPS = [5, 10, 25, 50, 100, 500];
function roulette(v) {
  let s;
  try { s = Roulette.decode(v.st); } catch { return banner(t('gBadState'), 'warn'); }
  const ui = v.ui;
  ui.chip = ui.chip || 10;
  const canBet = v.seat >= 0 && s.phase === 'bet' && !s.ready[v.seat];
  const mine = v.seat >= 0 ? s.bets[v.seat] : [];
  const onCell = (type, value) => mine.filter((b) => b.type === type && b.value === value).reduce((a, b) => a + b.amount, 0);
  const bet = (type, value) => { if (canBet) v.play({ op: 'bet', type, value, v: ui.chip }); };
  const cell = (label, type, value, cls) => {
    const amt = onCell(type, value);
    const win = s.phase === 'spun' && Roulette.multiplier(type, value, s.result) > 0;
    return el('button', { type: 'button', class: 'gm-rc ' + cls + (win ? ' hit' : ''), disabled: !canBet, dataset: { bet: type + ':' + value }, onclick: () => bet(type, value) },
      el('span', { text: label }), amt ? el('span', { class: 'gm-chip', text: String(amt) }) : null);
  };
  const color = (n) => (n === 0 ? 'z' : Roulette.RED.has(n) ? 'r' : 'k');
  const table = el('div', { class: 'gm-rt' });
  table.append(cell('0', 'n', 0, 'z zero'));
  for (let row = 0; row < 3; row++) for (let c = 0; c < 12; c++) {
    const n = c * 3 + (3 - row);
    const b = cell(String(n), 'n', n, color(n));
    b.style.gridRow = String(row + 1); b.style.gridColumn = String(c + 2);
    table.append(b);
  }
  for (let row = 0; row < 3; row++) { const b = cell('2:1', 'column', 3 - row, 'out'); b.style.gridRow = String(row + 1); b.style.gridColumn = '14'; table.append(b); }
  [[t('gDozen1'), 1], [t('gDozen2'), 2], [t('gDozen3'), 3]].forEach(([l, d]) => { const b = cell(l, 'dozen', d, 'out'); b.style.gridRow = '4'; b.style.gridColumn = `${2 + (d - 1) * 4} / span 4`; table.append(b); });
  [['1-18', 'low'], [t('gEven'), 'even'], ['◆', 'red'], ['◆', 'black'], [t('gOdd'), 'odd'], ['19-36', 'high']].forEach(([l, ty], k) => {
    const b = cell(l, ty, 0, 'out ' + (ty === 'red' ? 'r' : ty === 'black' ? 'k' : ''));
    b.style.gridRow = '5'; b.style.gridColumn = `${2 + k * 2} / span 2`;
    table.append(b);
  });
  const res = s.result >= 0 ? el('div', { class: 'gm-result ' + color(s.result), text: String(s.result) }) : el('div', { class: 'gm-result none', text: '?' });
  const hist = el('div', { class: 'gm-hist' }, ...s.history.map((n) => el('span', { class: color(n), text: String(n) })));
  const players = el('div', { class: 'gm-rows' }, ...v.t.players.map((p, i) => {
    const sum = s.bets[i].reduce((a, b) => a + b.amount, 0);
    const pay = s.phase === 'spun' ? s.pay[i] : 0;
    return el('div', { class: 'gm-row' }, seatChip(v, i, s.phase === 'bet' && !s.ready[i], sum ? fmtLei(sum) : '', s.ready[i] && s.phase === 'bet' ? '✓' : ''),
      pay ? el('span', { class: 'gm-pay ' + (pay > 0 ? 'good' : 'bad'), text: (pay > 0 ? '+' : '') + fmtLei(pay) }) : null);
  }));
  const ctl = el('div', { class: 'row wrap gm-ctl' });
  if (canBet) {
    ctl.append(...CHIPS.map((c) => el('button', { type: 'button', class: 'gm-chipbtn' + (ui.chip === c ? ' on' : ''), text: String(c), disabled: c > v.purse, onclick: () => { ui.chip = c; v.redraw(); } })));
    ctl.append(el('button', { type: 'button', text: t('gUndo'), disabled: !mine.length, onclick: () => v.play({ op: 'undo' }) }),
      el('button', { type: 'button', text: t('gClear'), disabled: !mine.length, onclick: () => v.play({ op: 'clear' }) }),
      el('button', { type: 'button', class: 'primary', id: 'gmReady', text: t('gReady'), onclick: () => v.play({ op: 'ready' }) }));
  }
  if (v.host && s.phase === 'bet' && s.bets.some((b) => b.length)) ctl.append(el('button', { type: 'button', class: 'primary', text: t('gSpin'), onclick: () => v.play({ op: 'spin' }) }));
  if (s.phase === 'spun' && v.seat >= 0) ctl.append(el('button', { type: 'button', class: 'primary', id: 'gmNext', text: t('gNextRound'), onclick: () => v.play({ op: 'next' }) }));
  const hint = canBet ? t('gRouletteHint') : s.phase === 'bet' ? t('gWaitingOthers') : '';
  return el('div', { class: 'gm-casino' }, el('div', { class: 'row gm-rhead' }, res, hist), table, hint ? el('div', { class: 'gm-note', text: hint }) : null, ctl, players);
}

// ====== Slots ======

function slots(v) {
  let s;
  try { s = Slots.decode(v.st); } catch { return banner(t('gBadState'), 'warn'); }
  const reels = el('div', { class: 'gm-reels' + (v.ui._spun !== s.spins ? ' spin' : '') }, ...s.reels.map((r) => el('div', { class: 'gm-reel' + (Slots.SYMBOLS[r] === '7' ? ' seven' : ''), text: Slots.SYMBOLS[r] })));
  v.ui._spun = s.spins;
  const msg = s.spins === 0 ? t('gGoodLuck') : s.last > 0 ? t('gYouWin', { amount: fmtLei(s.last + s.stake) }) : s.last === 0 ? t('gStakeBack') : t('gNothing');
  const kids = [el('div', { class: 'gm-slots-title', text: '★ GHOST SLOTS ★' }), reels, banner(msg, s.spins && s.last > 0 ? 'good' : '')];
  if (v.seat >= 0) {
    const inp = stakeInput(v, 'stake', Math.min(s.stake, Math.max(1, v.purse)), v.purse);
    const stake = () => readStake(v, 'stake', s.stake);
    kids.push(el('div', { class: 'row gm-ctl' }, el('span', { class: 'muted', text: t('gStakeLabel') }), inp,
      el('button', { type: 'button', class: 'primary gm-big', id: 'gmSpin', text: '🎰 ' + t('gSpin'), disabled: v.purse < 1, onclick: () => v.play({ op: 'spin', v: stake() }) })));
  }
  kids.push(el('div', { class: 'gm-hist' }, ...s.history.map((d) => el('span', { class: d > 0 ? 'good' : d < 0 ? 'bad' : '', text: (d > 0 ? '+' : '') + d }))));
  kids.push(el('div', { class: 'gm-paytable' }, ...Slots.SYMBOLS.map((sym, i) => el('span', { text: `${sym}${sym}${sym} ×${Slots.THREE[i]}` }))));
  return el('div', { class: 'gm-casino gm-slots' }, ...kids);
}

// ====== Blackjack ======

const SUITS = ['♠', '♥', '♦', '♣'];
const RANKS = ['', 'A', '2', '3', '4', '5', '6', '7', '8', '9', '10', 'J', 'Q', 'K'];
export function card(c) {
  if (c < 0) return el('span', { class: 'gm-card back', text: '🂠' });
  const su = Blackjack.suit(c);
  return el('span', { class: 'gm-card' + (su === 1 || su === 2 ? ' red' : '') }, el('b', { text: RANKS[Blackjack.rank(c)] }), el('i', { text: SUITS[su] }));
}
const OUTCOME = { 'Ai depășit 21': 'bjBust', 'Blackjack!': 'bjBlackjack', 'Dealerul are blackjack': 'bjDealerBj', Egalitate: 'bjPush', 'Dealerul a depășit': 'bjDealerBust', 'Câștigat': 'bjWin', Pierdut: 'bjLose' };
function handValue(cards) {
  if (!cards.length || cards.includes(-1)) return '';
  const [val, soft] = Blackjack.value(cards);
  return Blackjack.blackjack(cards) ? 'Blackjack' : soft && val < 21 ? `${val - 10} / ${val}` : String(val);
}
function blackjack(v) {
  let s;
  try { s = Blackjack.decode({ ...v.st, shoe: [] }); } catch { return banner(t('gBadState'), 'warn'); }
  const dealer = el('div', { class: 'gm-hand dealer' }, el('div', { class: 'gm-sub', text: t('bjDealer') + (handValue(s.dealer) ? ' · ' + handValue(s.dealer) : '') }), el('div', { class: 'gm-cards' }, ...s.dealer.map(card)));
  const rows = el('div', { class: 'gm-rows' }, ...v.t.players.map((p, i) => {
    const h = s.hands[i] || [];
    const under = s.bets[i] > 0 ? fmtLei(s.bets[i]) : s.phase === 'bet' && s.ready[i] ? t('bjSitsOut') : '';
    const res = s.phase === 'done' && s.outcome[i] ? el('span', { class: 'gm-pay ' + (s.result[i] > 0 ? 'good' : s.result[i] < 0 ? 'bad' : ''), text: t(OUTCOME[s.outcome[i]] || 'bjPush') + (s.result[i] ? ' ' + (s.result[i] > 0 ? '+' : '') + fmtLei(s.result[i]) : '') }) : null;
    return el('div', { class: 'gm-row' + (s.phase === 'play' && s.turn === i ? ' on' : '') }, seatChip(v, i, s.phase === 'play' && s.turn === i, under),
      el('div', { class: 'gm-cards' }, ...h.map(card)), h.length ? el('span', { class: 'gm-val', text: handValue(h) }) : null, res);
  }));
  const ctl = el('div', { class: 'row wrap gm-ctl' });
  const seat = v.seat;
  if (seat >= 0 && s.phase === 'bet' && !s.ready[seat]) {
    const inp = stakeInput(v, 'bet', Math.min(v.ui.lastBet || 50, Math.max(1, v.purse)), v.purse);
    ctl.append(el('span', { class: 'muted', text: t('gStakeLabel') }), inp,
      el('button', { type: 'button', class: 'primary', id: 'gmBet', text: t('bjBet'), disabled: v.purse < 1, onclick: () => { const b = readStake(v, 'bet', 50); v.ui.lastBet = b; v.play({ op: 'bet', v: b }); } }),
      el('button', { type: 'button', text: t('bjSitOut'), onclick: () => v.play({ op: 'skip' }) }));
  } else if (s.phase === 'bet' && v.host && s.bets.some((b) => b > 0)) ctl.append(el('button', { type: 'button', class: 'primary', text: t('bjDealNow'), onclick: () => v.play({ op: 'deal' }) }));
  if (seat >= 0 && s.phase === 'play' && s.turn === seat) {
    ctl.append(el('button', { type: 'button', class: 'primary gm-big', id: 'gmHit', text: t('bjHit'), onclick: () => v.play({ op: 'hit' }) }),
      el('button', { type: 'button', class: 'gm-big', id: 'gmStand', text: t('bjStand'), onclick: () => v.play({ op: 'stand' }) }));
    if (s.hands[seat].length === 2 && v.purse >= s.bets[seat]) ctl.append(el('button', { type: 'button', class: 'gm-big', text: t('bjDouble'), onclick: () => v.play({ op: 'double' }) }));
  }
  if (s.phase === 'done' && seat >= 0) ctl.append(el('button', { type: 'button', class: 'primary', id: 'gmNext', text: t('bjNextHand'), onclick: () => v.play({ op: 'next' }) }));
  const msg = s.phase === 'bet' ? t('bjPlaceBets') : s.phase === 'play' ? (s.turn === seat ? t('gYourTurn') : t('gTurnOf', { name: name(v, s.turn) })) : t('bjHandOver');
  return el('div', { class: 'gm-casino' }, dealer, rows, banner(msg), ctl);
}

// ====== Backgammon ======

function backgammon(v) {
  let s;
  try { s = Backgammon.decode(v.st); } catch { return banner(t('gBadState'), 'warn'); }
  const { BAR, OFF } = Backgammon;
  const ui = v.ui;
  const myTurn = s.winner < 0 && s.gameWinner < 0 && s.turn === v.seat && v.st.resigned == null;
  const legal = myTurn && s.dice.length ? Backgammon.legal(s) : [];
  if (ui._dice !== JSON.stringify([s.dice, s.turn, s.last])) { ui._dice = JSON.stringify([s.dice, s.turn, s.last]); ui.from = null; }
  const froms = new Set(legal.map((m) => m[0]));
  const targets = ui.from !== null && ui.from !== undefined ? legal.filter((m) => m[0] === ui.from).map((m) => m[1]) : [];
  const pick = (from) => {
    const to = [...new Set(legal.filter((m) => m[0] === from).map((m) => m[1]))];
    if (to.length === 1) { ui.from = null; v.play({ op: 'move', from, to: to[0] }); return; }
    ui.from = from; v.redraw();
  };
  const go = (to) => { if (ui.from !== null && ui.from !== undefined && targets.includes(to)) { const from = ui.from; ui.from = null; v.play({ op: 'move', from, to }); } };
  const stack = (n, who, max = 5) => {
    const k = Math.abs(n);
    const box = el('span', { class: 'gm-stack' });
    for (let i = 0; i < Math.min(k, max); i++) box.append(el('i', { class: 'gm-chk ' + who }));
    if (k > max) box.append(el('b', { class: 'gm-more', text: String(k) }));
    return box;
  };
  const point = (i, top) => {
    const n = s.points[i];
    const cls = ['gm-pt', top ? 'gm-up' : 'gm-dn', i % 2 ? 'a' : 'b'];
    if (froms.has(i) && ui.from == null) cls.push('can');
    if (ui.from === i) cls.push('sel');
    if (targets.includes(i)) cls.push('to');
    if (s.last.some((m) => m[1] === i)) cls.push('last');
    return el('button', {
      type: 'button', class: cls.join(' '), dataset: { pt: String(i) }, disabled: !myTurn, attrs: { 'aria-label': String(i + 1) },
      onclick: () => { if (targets.includes(i)) go(i); else if (froms.has(i)) pick(i); else { ui.from = null; v.redraw(); } },
    }, el('span', { class: 'gm-ptn', text: String(i + 1) }), n ? stack(n, n > 0 ? 'w' : 'k') : null);
  };
  // White's home bottom right; for black the rows swap, black's home then bottom right
  const upper = [...Array(12).keys()].map((k) => 12 + k);
  const lower = [...Array(12).keys()].map((k) => 11 - k);
  const [topRow, botRow] = v.seat === 1 ? [lower, upper] : [upper, lower];
  const row = (idxs, top) => el('div', { class: 'gm-bg-row' }, ...idxs.slice(0, 6).map((i) => point(i, top)), el('span', { class: 'gm-bg-mid' }), ...idxs.slice(6).map((i) => point(i, top)));
  const barBtn = (p) => el('button', {
    type: 'button', class: 'gm-bar' + (myTurn && p === v.seat && froms.has(BAR) ? ' can' : '') + (ui.from === BAR && p === v.seat ? ' sel' : ''), dataset: { bar: String(p) },
    disabled: !(myTurn && p === v.seat && froms.has(BAR)), onclick: () => pick(BAR),
  }, s.bar[p] ? stack(s.bar[p], p === 0 ? 'w' : 'k', 3) : null);
  const offBtn = el('button', {
    type: 'button', class: 'gm-off' + (targets.includes(OFF) ? ' to' : ''), id: 'gmOff', disabled: !targets.includes(OFF), onclick: () => go(OFF),
  }, el('span', { text: '⬇ ' + t('bgOff') }), el('small', { text: `${s.off[0]} · ${s.off[1]}` }));
  const board = el('div', { class: 'gm-bg' }, row(topRow, true), el('div', { class: 'gm-bg-bars' }, barBtn(0), barBtn(1), offBtn), row(botRow, false));
  const dice = el('div', { class: 'gm-bg-dice' }, ...s.rolled.map((d, i) => el('span', { class: 'gm-die' + (s.dice.includes(d) || (s.rolled[0] === s.rolled[1] && s.dice.length > i) ? '' : ' used'), text: DIE[d] })));
  let msg, cls = '';
  if (s.winner >= 0) { msg = t('gWins', { name: name(v, s.winner) }); cls = v.seat < 0 || v.seat === s.winner ? 'good' : 'bad'; }
  else if (s.gameWinner >= 0) msg = t(s.lastPoints === 3 ? 'bgWonTriple' : s.lastPoints === 2 ? 'bgWonDouble' : 'bgWonGame', { name: name(v, s.gameWinner) });
  else if (Backgammon.blocked(s)) msg = t('bgBlocked', { name: name(v, 1 - s.turn) });
  else msg = myTurn ? (s.dice.length ? t('bgMove') : t('gYourTurn')) : t('gTurnOf', { name: name(v, s.turn) });
  const ctl = el('div', { class: 'row gm-ctl' });
  if (myTurn && !s.dice.length) ctl.append(el('button', { type: 'button', class: 'primary gm-big', id: 'gmRoll', text: '🎲 ' + t('gRoll'), onclick: () => v.play({ op: 'roll' }) }));
  if (s.gameWinner >= 0 && s.winner < 0 && v.seat >= 0) ctl.append(el('button', { type: 'button', class: 'primary', id: 'gmNext', text: t('bgNextGame'), onclick: () => v.play({ op: 'again' }) }));
  const seatInfo = (i) => seatChip(v, i, s.winner < 0 && s.gameWinner < 0 && s.turn === i, `${t('bgScore', { n: s.score[i], of: Backgammon.TARGET })} · ${t('bgPips', { n: Backgammon.pips(s, i) })}`, i === 0 ? '⚪' : '⚫');
  return el('div', { class: 'gm-casino gm-bg-wrap' }, el('div', { class: 'gm-seats' }, seatInfo(0), seatInfo(1)), board, dice, banner(msg, cls), ctl);
}

// ====== Poker ======

const PK_NOTE = { 'Ceilalți au renunțat': 'pkFolded', Egalitate: 'bjPush', 'Prea puțini jucători cu bani': 'pkTooFew' };
const pkHand = (x) => { const i = Poker.NAMES.indexOf(x); return i >= 0 ? t(`pkH${i}`) : x; };
function poker(v) {
  let s;
  try { s = Poker.decode({ ...v.st, deck: [] }); } catch { return banner(t('gBadState'), 'warn'); }
  const seat = v.seat;
  const priv = v.priv && v.priv.hand === s.hand && Array.isArray(v.priv.cards) ? v.priv.cards : null;
  const pot = s.total.reduce((a, b) => a + b, 0);
  const boardCards = el('div', { class: 'gm-cards gm-board' }, ...Array.from({ length: 5 }, (_, i) => (s.board[i] !== undefined ? card(s.board[i]) : el('span', { class: 'gm-card slot' }))));
  const rows = el('div', { class: 'gm-rows' }, ...v.t.players.map((p, i) => {
    const mine = i === seat && priv ? priv : s.hands[i] || [];
    const tag = s.out[i] ? t('pkOut') : s.folded[i] ? t('pkFoldedTag') : s.stack[i] === 0 && s.phase !== 'done' && s.total[i] > 0 ? 'ALL IN' : '';
    const under = `${fmtLei(s.stack[i])}${s.bet[i] ? ' · ' + t('pkBet', { amount: fmtLei(s.bet[i]) }) : ''}`;
    const badge = i === s.button ? 'Ⓓ' : '';
    return el('div', { class: 'gm-row' + (s.turn === i ? ' on' : '') + (s.folded[i] || s.out[i] ? ' dim' : '') }, seatChip(v, i, s.turn === i, under, badge),
      el('div', { class: 'gm-cards' }, ...mine.map(card)),
      tag ? el('span', { class: 'gm-val', text: tag }) : null,
      s.phase === 'done' && s.best[i] ? el('span', { class: 'gm-val', text: pkHand(s.best[i]) }) : null,
      s.phase === 'done' && s.result[i] ? el('span', { class: 'gm-pay ' + (s.result[i] > 0 ? 'good' : 'bad'), text: (s.result[i] > 0 ? '+' : '') + fmtLei(s.result[i]) }) : null);
  }));
  const ctl = el('div', { class: 'row wrap gm-ctl' });
  if (s.phase !== 'done' && s.turn === seat && seat >= 0) {
    const owe = s.current - s.bet[seat];
    const most = s.bet[seat] + Math.min(s.stack[seat], v.purse);
    const minTo = s.current + s.minRaise;
    ctl.append(el('button', { type: 'button', class: 'gm-big', id: 'gmFold', text: t('pkFold'), onclick: () => v.play({ op: 'fold' }) }));
    if (owe <= 0) ctl.append(el('button', { type: 'button', class: 'primary gm-big', id: 'gmCheck', text: t('pkCheck'), onclick: () => v.play({ op: 'check' }) }));
    else ctl.append(el('button', { type: 'button', class: 'primary gm-big', id: 'gmCall', text: t('pkCall', { amount: fmtLei(Math.min(owe, s.stack[seat])) }), onclick: () => v.play({ op: 'call' }) }));
    if (most > s.current) {
      const inp = stakeInput(v, 'raise', Math.min(Math.max(minTo, Number(v.ui.raise) || 0), most), most);
      inp.min = Math.min(minTo, most);
      ctl.append(inp, el('button', { type: 'button', class: 'gm-big', id: 'gmRaise', text: t('pkRaise'), onclick: () => v.play({ op: 'raise', v: readStake(v, 'raise', minTo) }) }),
        el('button', { type: 'button', class: 'danger gm-big', text: 'ALL IN', onclick: () => v.play({ op: 'allin' }) }));
    }
  }
  if (s.phase === 'done') {
    if (seat >= 0) ctl.append(el('button', { type: 'button', class: 'primary', id: 'gmNext', text: t('bjNextHand'), onclick: () => v.play({ op: 'next' }) }));
    if (v.host) ctl.append(el('span', { class: 'muted', text: t('pkBlinds') }), ...[5, 10, 25, 50, 100].map((b) => el('button', { type: 'button', class: 'small' + (s.small === b ? ' on' : ''), text: `${b}/${b * 2}`, onclick: () => v.play({ op: 'blinds', v: b }) })));
  }
  const phase = { preflop: 'Preflop', flop: 'Flop', turn: 'Turn', river: 'River' }[s.phase];
  let msg;
  if (s.phase === 'done') msg = s.note ? (PK_NOTE[s.note] ? t(PK_NOTE[s.note]) : s.note.split(', ').map(pkHand).join(', ')) : t('bjHandOver');
  else msg = (s.turn === seat ? t('gYourTurn') : t('gTurnOf', { name: name(v, s.turn) })) + ' · ' + phase;
  return el('div', { class: 'gm-casino' }, el('div', { class: 'gm-sub', text: t('pkPot', { amount: fmtLei(pot) }) + ' · ' + t('pkBlindsNow', { a: s.small, b: s.big }) + ' · ' + t('pkHandNo', { n: s.hand }) }),
    boardCards, rows, banner(msg), ctl);
}

// ====== Party games: the scores, the clock ======

function scoreRow(v, sc, done = []) {
  return el('div', { class: 'gm-seats' }, ...v.t.players.map((p, i) => seatChip(v, i, false, String(sc[i] ?? 0), done.includes(i) ? '✓' : '')));
}
function countdown(dl, total) {
  const left = Math.max(0, (Number(dl) || 0) - Date.now());
  const bar = el('div', { class: 'gm-clock' }, el('i'), el('span', { text: Math.ceil(left / 1000) + ' s' }));
  bar.firstChild.style.width = Math.max(0, Math.min(100, (left / total) * 100)) + '%';
  return bar;
}
function partyEnd(v, sc) {
  const best = Math.max(0, ...sc);
  const winners = best > 0 ? v.t.players.filter((_, i) => sc[i] === best).map((p) => p.name) : [];
  const order = v.t.players.map((p, i) => [p.name, sc[i] ?? 0]).sort((a, b) => b[1] - a[1]);
  return el('div', { class: 'gm-casino' }, banner(winners.length ? t('gWins', { name: winners.join(', ') }) : t('gDraw'), 'good'),
    el('ol', { class: 'gm-final' }, ...order.map(([n, x]) => el('li', {}, el('b', { text: n }), el('span', { text: String(x) })))));
}
const arr = (x) => (Array.isArray(x) ? x : []);
const num = (x, d = 0) => (typeof x === 'number' ? x : d);

// ====== Trivia ======

function trivia(v) {
  const st = v.st;
  if (st.ph === 'q' && v.ui._q !== st.q) { v.ui._q = st.q; v.ui.pick = undefined; }
  const sc = arr(st.sc);
  if (st.ph === 'end') return partyEnd(v, sc);
  const done = arr(st.done);
  const opts = arr(st.opts);
  const answer = num(st.answer, -1);
  const res = st.ph === 'res';
  const picks = arr(st.picks), gain = arr(st.gain);
  const mine = v.seat >= 0 ? (res ? num(picks[v.seat], -1) : v.ui.pick) : undefined;
  const grid = el('div', { class: 'gm-opts' }, ...opts.map((o, i) => el('button', {
    type: 'button', class: 'gm-opt o' + i + (res && i === answer ? ' right' : '') + (res && i === mine && i !== answer ? ' wrong' : '') + (!res && i === mine ? ' mine' : ''),
    dataset: { opt: String(i) }, disabled: res || v.seat < 0 || done.includes(v.seat), text: (res && i === answer ? '✅ ' : '') + String(o),
    onclick: () => { v.ui.pick = i; v.play({ op: 'answer', i }); },
  })));
  const kids = [scoreRow(v, sc, res ? [] : done),
    el('div', { class: 'gm-sub', text: t('trQuestion', { n: num(st.rd) + 1, of: num(st.of), cat: String(st.cat ?? '') }) }),
    countdown(st.dl, res ? 5000 : 15000), el('div', { class: 'gm-question', text: String(st.q ?? '') }), grid];
  if (res) kids.push(el('div', { class: 'gm-rows' }, ...v.t.players.map((p, i) => el('div', { class: 'gm-row' }, el('b', { text: p.name }),
    el('span', { text: num(picks[i], -1) < 0 ? '—' : picks[i] === answer ? '✅' : '❌' }), gain[i] ? el('span', { class: 'gm-pay good', text: '+' + gain[i] }) : null))));
  else if (v.seat >= 0 && done.includes(v.seat)) kids.push(banner(t('trAnswered', { n: done.length, of: v.t.players.length }), 'good'));
  else if (v.seat < 0) kids.push(banner(t('trWatching')));
  return el('div', { class: 'gm-casino gm-party' }, ...kids);
}

// ====== Hangman ======

function hangman(v) {
  const st = v.st;
  const sc = arr(st.sc);
  if (st.ph === 'end') return partyEnd(v, sc);
  const res = st.ph === 'res';
  const turn = num(st.turn);
  const myTurn = !res && v.seat === turn;
  const miss = num(st.miss);
  const tried = arr(st.tried).map(String);
  const kids = [scoreRow(v, sc), el('div', { class: 'gm-sub', text: t('hmWord', { n: num(st.rd) + 1, of: num(st.of), cat: String(st.cat ?? '') }) }),
    countdown(st.dl, res ? 6000 : 20000),
    el('div', { class: 'gm-gallows', text: '♥'.repeat(Math.max(0, Hangman.LIVES - miss)) + '♡'.repeat(Math.min(Hangman.LIVES, miss)) }),
    el('div', { class: 'gm-mask' + (res ? (num(st.solved, -1) >= 0 ? ' good' : ' bad') : '') }, ...String(st.mask ?? '').split('').map((c) => el('span', { class: c === '_' ? 'blank' : '', text: c === '_' ? '' : c.toUpperCase() }))),
    arr(st.wrong).length ? el('div', { class: 'gm-note', text: t('hmWrong') + ' ' + arr(st.wrong).map((x) => String(x).toUpperCase()).join(' ') }) : null];
  if (res) kids.push(banner(num(st.solved, -1) >= 0 ? t('hmSolved', { name: name(v, st.solved) }) : t('hmHanged'), num(st.solved, -1) >= 0 ? 'good' : 'bad'));
  else kids.push(banner(myTurn ? t('hmYourTurn') : t('hmPicks', { name: name(v, turn) })));
  if (!res) kids.push(el('div', { class: 'gm-letters' }, ...Hangman.LETTERS.map((l) => el('button', {
    type: 'button', class: 'gm-letter', dataset: { letter: l }, disabled: !myTurn || tried.includes(l), text: l.toUpperCase(), onclick: () => v.play({ op: 'letter', v: l }),
  }))));
  if (myTurn) {
    const inp = el('input', { maxLength: 30, placeholder: t('hmSolveHint'), dataset: { k: v.t.id + ':solve' } });
    inp.value = v.ui.solve || '';
    inp.oninput = () => { v.ui.solve = inp.value; };
    kids.push(el('form', { class: 'row gm-ctl', onsubmit: (ev) => { ev.preventDefault(); const x = inp.value.trim(); if (x) { v.ui.solve = ''; v.play({ op: 'solve', v: x }); } } },
      inp, el('button', { type: 'submit', class: 'primary', text: t('hmSolve') })));
  }
  return el('div', { class: 'gm-casino gm-party' }, ...kids);
}

export const views = { TICTACTOE: grid, CONNECT4: grid, CHESS: chess, BACKGAMMON: backgammon, BARBUT: barbut, ROULETTE: roulette, SLOTS: slots, BLACKJACK: blackjack, POKER: poker, TRIVIA: trivia, HANGMAN: hangman };
