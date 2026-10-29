// Games in the web client: the tables of the server (as the apps list them), opening one, sitting down, watching,
// and playing with the PC and Android players at the same table. The bus (../bus.js) and the tables (hub.js) are
// the apps' own protocol; this file is the dialog: the list, a table's frame (players, notes, bets, talk) and its board
// (views.js). Opened from the 🎲 button in the module tools (#moduleTools).

import { $, S, el, on, toast, openDialog, me as meMember } from '../core.js';
import { t, getLang } from '../i18n.js';
import { bus, busSend } from '../bus.js';
import { createHub, fmtLei } from './hub.js';
import * as R from './rules.js';
import { views } from './views.js';

const REACTIONS = ['👏', '😂', '🔥', '😮', '💀', '🍿', '👑', '😡'];

let dlg = null;
let redrawTimer = 0;
const tableUi = new Map(); // table id → its view's own state (a selection, a chip, a stake typed)
let talkOpen = true;
let wagerAmount = 50;

// The page's one bus connection (bus.js): the tables' messages come as bus:g and bus:gchat
const net = { online: () => bus.online, kept: () => bus.kept };
const hub = createHub({
  send: (m) => busSend(m), kept: () => bus.kept, caps: () => new Set(bus.ops), online: () => bus.online,
  me: () => ({ uid: S.uid || '', name: meMember()?.name || S.name || '?' }),
  present: () => {
    if (S.phase !== 'on' || !S.members.size) return null;
    const out = new Set();
    for (const m of S.members.values()) { if (!m.uid) return null; out.add(m.uid); }
    return out;
  },
  text: (k, v) => t(k, v),
  toast: (x) => toast(x),
});
hub.onChange(() => schedule());
hub.start();

// For the tests and the console: the page's games
globalThis.svGames = { hub, bus: net, open: () => openGames() };

const myUid = () => S.uid;
const purse = () => hub.Lei.of(myUid());
const kindName = (k) => t(`g_${k}`);

function schedule() {
  updateButton();
  if (!dlg || redrawTimer) return;
  redrawTimer = setTimeout(() => { redrawTimer = 0; render(false); }, 30);
}

// ====== The top bar's button ======

function waitingOnMe() {
  const me = myUid();
  for (const tb of hub.tables.values()) {
    const seat = tb.players.findIndex((p) => p.uid === me);
    if (seat < 0 || !tb.started || !tb.state || !R.playable(tb.kind)) continue;
    if (R.waitingOn(tb.kind, tb.state, hub.secretOf(tb.id)).includes(seat)) return true;
    if (tb.kind === 'BLACKJACK' && tb.state.phase === 'play' && tb.state.turn === seat) return true;
  }
  return hub.invites.length > 0;
}

let button = null;
function placeButton() {
  let box = $('moduleTools');
  if (!box) {
    const tools = document.querySelector('header.top .tools') || document.querySelector('header .tools');
    if (!tools) return;
    box = el('div', { id: 'moduleTools', class: 'moduletools' });
    tools.prepend(box);
  }
  button = el('button', { id: 'gamesBtn', class: 'ghost icon gm-open', type: 'button', text: '🎲', title: t('gGames'), attrs: { 'aria-label': t('gGames') }, onclick: () => openGames() });
  box.append(button);
  updateButton();
}
function updateButton() {
  if (!button) return;
  button.classList.toggle('hidden', !S.token);
  button.classList.toggle('ping', waitingOnMe());
  button.title = t('gGames');
}
if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', placeButton); else placeButton();

// ====== The connection follows the session ======

on('bus:g', (m) => hub.handle(m));
on('bus:gchat', (m) => hub.handle(m));
on('bus:open', () => schedule());
on('bus:down', () => schedule());
on('bus:kept', (k) => { if (!k || !k.kind || k.kind === 'lei') schedule(); });
on('app:welcome', () => updateButton());
on('app:down', (m) => {
  if (m && m.retry) { schedule(); return; } // coming back: the tables stay, as the apps' while reconnecting
  hub.leftServer();
  if (dlg) dlg.close();
  updateButton();
});
on('app:lang', () => { if (button) button.title = t('gGames'); schedule(); });
// Closing the page closes our tables: the chips and bets on them go back (only the keeper knew them)
addEventListener('pagehide', () => { try { hub.leftServer(); } catch { /* going */ } });

// ====== The dialog ======

export function openGames() {
  const root = el('div', { class: 'gm' });
  dlg = openDialog({ title: t('gGames'), body: root, wide: true, cls: 'games', onclose: () => { dlg = null; } });
  dlg.body = root;
  render(true);
}

// What's on screen, in short: drawn again only when it changed (a button redrawn under the pointer loses the click)
function signature() {
  const tb = hub.open ? hub.tables.get(hub.open) : null;
  const base = [getLang(), bus.online, purse(), hub.invites.length, talkOpen];
  if (!tb) return JSON.stringify([base, [...hub.tables.values()].map((x) => [x.id, x.kind, x.started, x.players])]);
  // The clocks on screen (the last minute to move, a party round's time) tick every second
  const afk = tb.state?.afk || (R.isParty(tb.kind) && tb.state?.dl > 0) ? Math.floor(Date.now() / 1000) : 0;
  // The view's own notes ("_…": what it last drew) aren't part of it
  return JSON.stringify([base, tb, tableUi.get(tb.id) || null, hub.privates.get(tb.id) || null, (hub.talk.get(tb.id) || []).length, afk, hub.betsOpen(tb)], (k, v) => (k.startsWith('_') || k === 'seen' ? undefined : v));
}
let drawn = '';

function render(force) {
  if (!dlg) return;
  const root = dlg.body;
  if (!dlg.root.open || !dlg.root.contains(root)) { dlg = null; return; }
  const sig = signature();
  if (!force && sig === drawn) return;
  drawn = sig;
  // What has the focus (a stake being typed, the talk line) keeps it
  const a = document.activeElement;
  const key = a && root.contains(a) ? a.dataset.k || a.id : '';
  const sel = key && typeof a.selectionStart === 'number' ? [a.selectionStart, a.selectionEnd] : null;
  const scroll = root.querySelector('.gm-talk-lines')?.scrollTop;
  const id = hub.open;
  const tb = id ? hub.tables.get(id) : null;
  if (id && !tb) hub.open = null;
  root.replaceChildren(tb ? tableView(tb) : lobby());
  if (key) {
    const e = root.querySelector(`[data-k="${CSS.escape(key)}"]`) || (a.id ? root.querySelector('#' + CSS.escape(a.id)) : null);
    if (e) { e.focus(); if (sel && e.setSelectionRange) { try { e.setSelectionRange(sel[0], sel[1]); } catch { /* not a text field */ } } }
  }
  const lines = root.querySelector('.gm-talk-lines');
  if (lines) lines.scrollTop = scroll ?? lines.scrollHeight;
}

function head() {
  return el('div', { class: 'gm-head' },
    el('span', { class: 'gm-purse', id: 'gmPurse', text: '💰 ' + fmtLei(purse()) }),
    bus.online ? null : el('span', { class: 'gm-warn', text: t('gConnecting') }));
}

// ====== The list: open a table, sit at one, watch ======

function lobby() {
  const me = myUid();
  const make = el('div', { class: 'gm-kinds' }, ...R.PLAYABLE.map((k) => el('button', {
    type: 'button', class: 'gm-kind', dataset: { kind: k }, disabled: !bus.online, onclick: () => { hub.create(k); render(); },
  }, el('span', { class: 'gm-kind-emoji', text: R.emoji(k) }), el('b', { text: kindName(k) }),
  el('small', { text: R.kindMin(k) === R.kindMax(k) ? t('gPlayersN', { n: R.kindMin(k) }) : t('gPlayersRange', { a: R.kindMin(k), b: R.kindMax(k) }) }))));
  const list = el('div', { class: 'gm-tables' });
  const all = [...hub.tables.values()].sort((x, y) => (x.id < y.id ? -1 : 1));
  if (!all.length) list.append(el('p', { class: 'muted', text: t('gNoTables') }));
  for (const tb of all) {
    const seated = tb.players.some((p) => p.uid === me);
    const full = tb.players.length >= R.kindMax(tb.kind);
    const ok = R.playable(tb.kind);
    const label = seated ? t('gOpen') : !ok ? t('gInApp') : tb.started || full ? t('gWatch') : t('gSit');
    list.append(el('div', { class: 'gm-table-row', dataset: { table: tb.id, kind: tb.kind } },
      el('span', { class: 'gm-kind-emoji', text: R.emoji(tb.kind) }),
      el('div', { class: 'grow' }, el('b', { text: kindName(tb.kind) }),
        el('div', { class: 'muted small', text: tb.players.map((p) => p.name).join(', ') + ' · ' + (tb.started ? t('gPlaying') : `${tb.players.length}/${R.kindMax(tb.kind)}`) })),
      el('button', {
        type: 'button', class: seated ? '' : 'primary', text: label, disabled: !ok && !seated,
        onclick: () => { if (seated || tb.started || full) hub.open = tb.id; else hub.join(tb.id); render(); },
      })));
  }
  const inv = hub.invites.map((i) => el('div', { class: 'gm-invite' }, el('span', { text: t('gInvited', { name: i.from, game: kindName(i.kind) }) }),
    el('button', { type: 'button', class: 'primary small', text: t('gSit'), onclick: () => { hub.join(i.table); render(); } })));
  return el('div', { class: 'gm-lobby' }, head(), ...inv,
    el('h3', { text: t('gOpenTable') }), make,
    el('h3', { text: t('gTables', { n: all.length }) }), list,
    el('p', { class: 'muted small', text: t('gMoreInApp') }));
}

// ====== A table ======

function tableView(tb) {
  const me = myUid();
  const seat = tb.players.findIndex((p) => p.uid === me);
  const host = tb.host === me;
  const st = tb.state;
  if (!tableUi.has(tb.id)) tableUi.set(tb.id, {});
  for (const k of [...tableUi.keys()]) if (!hub.tables.has(k)) tableUi.delete(k);
  const ui = tableUi.get(tb.id);
  const over = !st || st.final === true || st.resigned != null;
  const top = el('div', { class: 'gm-top' },
    el('button', { type: 'button', class: 'ghost small', id: 'gmBack', text: '← ' + t('gAllTables'), onclick: () => { hub.open = null; render(); } }),
    el('span', { class: 'gm-title', text: R.emoji(tb.kind) + ' ' + kindName(tb.kind) }),
    el('span', { class: 'grow' }),
    el('span', { class: 'gm-purse', id: 'gmPurse', text: '💰 ' + fmtLei(purse()) }),
    host && tb.started && !tb.tour ? el('button', { type: 'button', class: 'small', id: 'gmAgain', text: t('gNewGame'), onclick: () => hub.again(tb.id) }) : null,
    seat >= 0 && tb.started && !over && R.turnBased(tb.kind) ? resignButton(tb, ui) : null,
    el('button', { type: 'button', class: 'small' + (talkOpen ? ' on' : ''), text: '💬', title: t('gTalk'), onclick: () => { talkOpen = !talkOpen; render(); } }),
    el('button', { type: 'button', class: 'danger small', id: 'gmLeave', text: host ? t('gCloseTable') : seat >= 0 ? t('gLeaveTable') : t('gStopWatching'), onclick: () => { if (seat >= 0 || host) hub.leave(tb.id); else hub.open = null; render(); } }));
  const notes = [];
  if (tb.note) notes.push(el('div', { class: 'gm-note warn', text: tb.note }));
  if (st && Number.isInteger(st.resigned)) {
    const who = tb.players[st.resigned]?.name || '?';
    const others = tb.players.filter((_, i) => i !== st.resigned).map((p) => p.name).join(', ');
    notes.push(el('div', { class: 'gm-note warn', text: t(st.why === 'time' ? 'gForfeitTime' : st.why === 'gone' ? 'gForfeitGone' : 'gForfeitResign', { name: who }) + ' ' + t('gOthersWon', { names: others }) }));
  }
  if (st && st.afk && typeof st.afk === 'object' && !over) {
    const left = Math.max(0, Math.round(((Number(st.afk.dl) || 0) - Date.now()) / 1000));
    const names = (Array.isArray(st.afk.s) ? st.afk.s : []).map((i) => tb.players[i]?.name).filter(Boolean).join(', ');
    notes.push(el('div', { class: 'gm-note clock', text: '⏳ ' + t(st.afk.f ? 'gClockLose' : 'gClockPlayed', { names, s: left }) }));
  }
  const wagers = wagerBar(tb, seat);
  let main;
  if (!tb.started || !st) main = lobbySeats(tb, host);
  else if (!R.playable(tb.kind)) main = el('div', { class: 'gm-note', text: t('gInAppLong') });
  else if (!host && !R.validState(tb.kind, st, tb.players.length)) main = el('div', { class: 'gm-note warn', text: t('gBadState') });
  else {
    const v = { t: tb, st, seat, host, purse: purse(), priv: hub.privates.get(tb.id) || null, ui, play: (m) => hub.play(tb.id, m), redraw: () => render(true) };
    try { main = views[tb.kind](v); } catch (e) { console.error(e); main = el('div', { class: 'gm-note warn', text: t('gBadState') }); }
  }
  const body = el('div', { class: 'gm-play', dataset: { kind: tb.kind } }, ...notes, wagers, main);
  return el('div', { class: 'gm-tableview' + (talkOpen ? ' talk' : ''), dataset: { table: tb.id } }, top,
    el('div', { class: 'gm-split' }, body, talkOpen ? talkPane(tb) : null));
}

function resignButton(tb, ui) {
  return el('button', {
    type: 'button', class: 'small' + (ui.sure ? ' danger' : ''), id: 'gmResign', text: ui.sure ? t('gResignSure') : t('gResign'),
    onclick: () => {
      if (ui.sure) { ui.sure = false; hub.resign(tb.id); return; }
      ui.sure = true; render();
      setTimeout(() => { ui.sure = false; render(); }, 4000);
    },
  });
}

function lobbySeats(tb, host) {
  const seats = el('div', { class: 'gm-lobby-seats' }, ...tb.players.map((p) => el('div', { class: 'gm-lobby-seat' }, el('span', { class: 'gm-avatar big', text: p.name.slice(0, 1).toUpperCase() }), el('span', { text: p.name }))));
  for (let i = tb.players.length; i < R.kindMax(tb.kind); i++) seats.append(el('div', { class: 'gm-lobby-seat free' }, el('span', { class: 'gm-avatar big', text: '?' }), el('span', { class: 'muted', text: t('gFree') })));
  const enough = tb.players.length >= R.kindMin(tb.kind);
  return el('div', { class: 'gm-waiting' }, el('h3', { text: t('gWaitingAt') }), seats,
    host ? el('button', { type: 'button', class: 'primary', id: 'gmStart', disabled: !enough || !R.playable(tb.kind), text: t('gStart'), onclick: () => hub.startGame(tb.id) }) : el('p', { class: 'muted', text: t('gWaitingHost') }),
    enough ? null : el('p', { class: 'muted small', text: t('gAtLeast', { n: R.kindMin(tb.kind) }) }));
}

// Onlookers bet on a seat in the first two minutes of a duel; paid out by the keeper when it ends
function wagerBar(tb, seat) {
  if (!R.isDuel(tb.kind) || !tb.started) return null;
  const list = tb.wagers.length ? el('div', { class: 'muted small', text: t('gBets') + ': ' + tb.wagers.map((w) => `${w.name} → ${tb.players[w.on]?.name || '?'} (${fmtLei(w.amount)})`).join(', ') }) : null;
  if (seat >= 0 || !hub.betsOpen(tb)) return list ? el('div', { class: 'gm-wagers' }, list) : null;
  const inp = el('input', { type: 'number', min: 1, step: 1, class: 'gm-stake', value: wagerAmount, dataset: { k: 'wager' } });
  inp.oninput = () => { wagerAmount = Math.trunc(Number(inp.value)) || 0; };
  return el('div', { class: 'gm-wagers' }, el('div', { class: 'row wrap' }, el('span', { text: t('gBetOn') }), inp,
    ...tb.players.map((p, i) => el('button', { type: 'button', class: 'small', disabled: wagerAmount < 1 || wagerAmount > purse(), text: t('gOn', { name: p.name }), onclick: () => hub.bet(tb.id, i, wagerAmount) }))), list);
}

function talkPane(tb) {
  const lines = (hub.talk.get(tb.id) || []).filter((l) => !l.emoji);
  const recent = (hub.talk.get(tb.id) || []).filter((l) => l.emoji && Date.now() - l.at < 4000).slice(-6);
  const box = el('div', { class: 'gm-talk-lines' });
  if (!lines.length) box.append(el('p', { class: 'muted small', text: t('gTalkEmpty') }));
  for (const l of lines) box.append(el('div', { class: 'gm-talk-line' }, el('b', { text: l.name }), el('span', { text: l.text })));
  const input = el('input', { maxLength: 200, placeholder: t('gWrite'), dataset: { k: 'talk:' + tb.id } });
  const form = el('form', { class: 'gm-talk-say', onsubmit: (ev) => { ev.preventDefault(); hub.say(tb.id, input.value); input.value = ''; } }, input);
  return el('aside', { class: 'gm-talk' }, el('div', { class: 'gm-sub', text: t('gTalk') }), box,
    recent.length ? el('div', { class: 'gm-reacts' }, ...recent.map((r) => el('span', { title: r.name, text: r.text }))) : null,
    el('div', { class: 'gm-react-row' }, ...REACTIONS.map((e) => el('button', { type: 'button', class: 'ghost icon', text: e, onclick: () => hub.say(tb.id, e, true) }))), form);
}

// The clock's last minute counts down on screen
setInterval(() => {
  if (!dlg || !hub.open) return;
  const tb = hub.tables.get(hub.open);
  if (tb?.state?.afk || (R.isParty(tb?.kind) && tb?.state?.dl > 0)) schedule();
}, 1000);
