// The web client's game boards (DOM): one function per kind, given {t (the table), st (its state), seat (yours, −1
// watching), host (you keep it), purse (your Lei), play(move), ui (this table's own view state), redraw()}.
// Only what's on screen lives here; the rules are in rules.js and the others' moves come from the keeper.

import { el } from '../core.js';
import { t } from '../i18n.js';
import * as Grid from './grid.js';
import { Chess, histMoves, sans } from './chess.js';
import { Barbut, Roulette, Slots } from './casino.js';
import { Blackjack } from './blackjack.js';
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

export const views = { TICTACTOE: grid, CONNECT4: grid, CHESS: chess, BARBUT: barbut, ROULETTE: roulette, SLOTS: slots, BLACKJACK: blackjack };
