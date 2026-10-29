// The game tables on the server's bus, as the apps' GameHub.kt: whoever opens a table keeps the game. The others
// send their moves to it ({"t":"g","op":"move","id","m","name"}); it checks them with the rules and sends everyone
// the whole table ({"t":"g","op":"table",…}, again every 15 s); it pays out the Lei, keeps the scores, takes the
// onlookers' bets and watches the clock. A table nobody announced for 45 s is gone; a keeper that leaves closes its
// tables ({"op":"closed"}), giving back the chips and bets on them.
//
// No DOM: createHub(env) takes the bus and the person (env.send, env.kept, env.caps, env.online, env.me,
// env.present, env.now, env.text, env.rnd, env.toast), so node can drive it in the tests.

import { toInt, defaultRnd, shuffled } from './util.js';
import * as R from './rules.js';

export const BET_WINDOW_MS = 120000;
export const TURN_MS = 180000;
export const WARN_MS = 60000;
export const AFK_TURN_MS = 20000;
export const GONE_MS = 60000;
export const IDLE_CLOSE_MS = 30 * 60000;

export const LEI_START = 1000, LEI_REFILL = 100, LEI_DAILY = 100;
export const fmtLei = (v) => String(Math.abs(Math.trunc(v))).replace(/\B(?=(\d{3})+(?!\d))/g, '.').replace(/^/, v < 0 ? '-' : '') + ' Lei';

const pad = (n, w = 2) => String(n).padStart(w, '0');
export const localDate = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
// "2026-W44" (ISO week, as Scores.thisWeek)
export function isoWeek(d) {
  const t = new Date(Date.UTC(d.getFullYear(), d.getMonth(), d.getDate()));
  const day = t.getUTCDay() || 7;
  t.setUTCDate(t.getUTCDate() + 4 - day);
  const y = t.getUTCFullYear();
  const wk = Math.ceil(((t - Date.UTC(y, 0, 1)) / 86400000 + 1) / 7);
  return `${y}-W${pad(wk)}`;
}

export function createHub(env) {
  const now = () => (env.now ? env.now() : Date.now());
  const rnd = env.rnd || defaultRnd;
  const text = env.text || ((k) => k);
  const tables = new Map();
  const secret = new Map();
  const privates = new Map();
  const talk = new Map();
  let invites = [];
  const lastMove = new Map(), afk = new Map(), gotUp = new Map(), goneSince = new Map();
  const listeners = [];
  let lastUid = '';
  let openId = null;

  const changed = () => { for (const f of listeners) { try { f(); } catch (e) { console.error(e); } } };
  const me = () => { const m = env.me(); if (m.uid) lastUid = m.uid; return { uid: m.uid || lastUid, name: m.name }; };
  let sendFn = (msg) => env.send(msg);
  const send = (msg) => sendFn(msg);
  const title = (k) => text('g_' + k);

  // ====== Lei: one purse per player, kept by the server (bus kind "lei"); the keeper pays out and takes in ======

  const fresh = new Map(); // uid → {v: entry, base, at}: what was just written, until the server's answer
  const keptLei = () => (env.kept().lei || {});
  function leiEntry(uid) {
    const server = keptLei()[uid];
    const f = fresh.get(uid);
    if (f && f.base === server && now() - f.at < 8000) return f.v;
    return server || {};
  }
  const Lei = {
    of(uid) { const v = toInt(leiEntry(uid).v); return Math.max(v === null ? LEI_START : v, 0); },
    write(uid, name, delta, once, strict, ref) {
      const base = keptLei()[uid];
      const e = { ...leiEntry(uid) };
      const v = Math.max((toInt(e.v) ?? LEI_START) + delta, 0);
      e.v = v; e.n = name;
      if (once) e.d = once;
      fresh.set(uid, { v: e, base, at: now() });
      const msg = { t: 'add', kind: 'lei', id: uid, d: delta, start: LEI_START, min: 0, set: { n: name } };
      if (once) msg.once = once;
      if (strict) msg.strict = true;
      if (ref) msg.ref = ref;
      send(msg);
      return v;
    },
    add(uid, name, delta) { return Lei.write(uid, name, delta); },
    daily: (v) => Math.max(LEI_DAILY, LEI_REFILL - v),
    claimDaily(uid, name) {
      if (!uid || !('lei' in env.kept())) return 0;
      const today = localDate(new Date(now()));
      if (leiEntry(uid).d === today) return 0;
      const give = Lei.daily(Lei.of(uid));
      Lei.write(uid, name, give, today);
      return give;
    },
  };

  // ====== Scores (bus kind "scores"): wins, games, streaks, Lei won; this week's kings archived once ======

  const scoreEntries = () => Object.entries(env.kept().scores || {}).filter(([id]) => !id.startsWith('_') && !id.startsWith('~'));
  const group = (v, k) => (v && typeof v[k] === 'object' && v[k] ? v[k] : {});
  const weekScore = (v, k) => toInt(group(v, R.isChance(k) ? 'p7' : 'w7')[k]) ?? 0;
  const allScore = (v, k) => toInt(group(v, R.isChance(k) ? 'p' : 'w')[k]) ?? 0;
  const Scores = {
    archiveLastWeek() {
      const kept = env.kept();
      if (!('scores' in kept)) return;
      const d = new Date(now());
      d.setDate(d.getDate() - 7);
      const last = isoWeek(d);
      if (('_hist:' + last) in kept.scores) return;
      const kings = {};
      for (const k of Object.keys(R.KINDS)) {
        let best = null;
        for (const [uid, v] of scoreEntries()) {
          if (v.wk !== last || weekScore(v, k) <= 0) continue;
          if (!best || weekScore(v, k) > weekScore(best[1], k) || (weekScore(v, k) === weekScore(best[1], k) && allScore(v, k) > allScore(best[1], k))) best = [uid, v];
        }
        if (best) kings[k] = { u: best[0], n: typeof best[1].n === 'string' ? best[1].n : '?', s: weekScore(best[1], k) };
      }
      if (!Object.keys(kings).length) return;
      send({ t: 'put', kind: 'scores', id: '_hist:' + last, v: kings });
    },
    change(uid, name, inc, max, set) {
      Scores.archiveLastWeek();
      if (!env.caps().has('incr')) return;
      send({ t: 'incr', kind: 'scores', id: uid, inc, max, set: { ...set, n: name }, week: isoWeek(new Date(now())), weekly: ['w7', 'p7'] });
    },
    result(kind, players, winners) {
      const entries = env.kept().scores || {};
      players.forEach((p, i) => {
        const won = winners.includes(i);
        const streak = won ? (toInt(group(entries[p.uid], 's')[kind]) ?? 0) + 1 : 0;
        Scores.change(p.uid, p.name, { ['g.' + kind]: 1, ...(won ? { ['w.' + kind]: 1, ['w7.' + kind]: 1 } : {}) }, { ['bs.' + kind]: streak }, { ['s.' + kind]: streak });
      });
    },
    profit(uid, name, kind, delta) {
      if (delta === 0) return;
      if (delta > 0 && uid && env.caps().has('incr')) send({ t: 'incr', kind: 'scores', id: '_day:' + localDate(new Date(now())), inc: { ['l.' + uid]: delta }, set: { ['n.' + uid]: name } });
      Scores.change(uid, name, { ['p.' + kind]: delta, ['p7.' + kind]: delta }, { ['big.' + kind]: delta }, {});
    },
  };

  function feed(e, x) {
    if (!env.online()) return;
    const t = now();
    send({ t: 'put', kind: 'scores', id: '_feed:' + t.toString(36) + rnd.nextInt(1000), v: { e, x: x.slice(0, 160), at: t } });
  }

  // ====== Tables ======

  const encode = (t) => ({
    t: 'g', op: 'table', id: t.id, kind: t.kind, host: t.host, players: t.players.map((p) => ({ uid: p.uid, name: p.name })), started: t.started,
    state: t.state, note: t.note, at: t.startedAt, wagers: t.wagers.map((w) => [w.uid, w.name, w.on, w.amount]), tour: t.tour,
  });
  const announce = (t) => send(encode(t));
  function update(t) {
    const n = { ...t, seen: now() };
    tables.set(t.id, n);
    announce(t);
    changed();
  }
  const forget = (id) => { lastMove.delete(id); afk.delete(id); gotUp.delete(id); };
  const betsOpen = (t) => R.isDuel(t.kind) && t.started && t.wagers.length < 40 && now() - t.startedAt < BET_WINDOW_MS && t.state?.final !== true;

  // A table as it came over the bus, checked before it's taken (GameHub.tableFrom); null: not taken
  function tableFrom(from, m) {
    const id = m.id;
    if (typeof id !== 'string' || typeof m.kind !== 'string' || !(m.kind in R.KINDS)) return null;
    const kind = m.kind;
    if (m.host !== from) return null;
    const known = tables.get(id);
    if (known && known.host !== from) return null;
    const players = (Array.isArray(m.players) ? m.players : []).filter((p) => p && typeof p === 'object' && typeof p.uid === 'string')
      .map((p) => ({ uid: p.uid, name: (typeof p.name === 'string' ? p.name : '?').slice(0, 40) }));
    const wagers = (Array.isArray(m.wagers) ? m.wagers : []).filter((w) => Array.isArray(w) && w.length === 4 && typeof w[0] === 'string' && toInt(w[2]) !== null && toInt(w[3]) !== null)
      .map((w) => ({ uid: w[0], name: typeof w[1] === 'string' ? w[1] : '?', on: toInt(w[2]), amount: toInt(w[3]) }));
    const started = m.started === true;
    const tour = typeof m.tour === 'string' ? m.tour : '';
    const state = m.state && typeof m.state === 'object' && !Array.isArray(m.state) ? m.state : null;
    if (players.length > R.kindMax(kind) || (started && !tour && players.length < R.kindMin(kind))) return null;
    if (started && !R.validState(kind, state, players.length)) return null;
    return { id, kind, host: from, players, started, state, note: typeof m.note === 'string' ? m.note : '', seen: now(), startedAt: toInt(m.at) ?? 0, wagers, tour };
  }

  function handle(m) {
    if (m.t === 'gchat') {
      const id = m.id;
      if (!tables.has(id)) return;
      const e = m.e === true;
      const x = typeof m.x === 'string' ? m.x.trim().slice(0, e ? 8 : 200) : '';
      if (!x) return;
      heard(id, { name: (typeof m.n === 'string' ? m.n : '').slice(0, 40) || '?', uid: typeof m.u === 'string' ? m.u : '', text: x, emoji: e, at: now() });
      return;
    }
    if (m.t !== 'g') return;
    const from = m.from, id = m.id;
    if (typeof from !== 'string' || typeof id !== 'string') return;
    const my = me();
    switch (m.op) {
      case 'table': {
        const t = tableFrom(from, m);
        if (!t) return;
        tables.set(id, t);
        changed();
        return;
      }
      case 'closed':
        if (tables.get(id)?.host === from) { tables.delete(id); prune(); }
        if (openId === id && !tables.has(id)) openId = null;
        changed();
        return;
      case 'private':
        if (m.to === my.uid && tables.get(id)?.host === from) { privates.set(id, m.v && typeof m.v === 'object' ? m.v : {}); changed(); }
        return;
      case 'invite': {
        if (m.to !== my.uid) return;
        const t = tables.get(id);
        if (!t) return;
        invites = invites.filter((i) => i.table !== id).concat([{ table: id, from: typeof m.name === 'string' ? m.name : '?', kind: t.kind, at: now() }]);
        env.toast?.(text('gInvited', { name: typeof m.name === 'string' ? m.name : '?', game: title(t.kind) }));
        changed();
        return;
      }
      case 'join': case 'leave': case 'move': {
        const t = tables.get(id);
        if (!t || t.host !== my.uid) return;
        const who = { uid: from, name: typeof m.name === 'string' ? m.name : '?' };
        if (m.op === 'join') {
          if (!t.started && !t.tour && !t.players.some((p) => p.uid === from) && t.players.length < R.kindMax(t.kind)) update({ ...t, players: [...t.players, who] });
        } else if (m.op === 'leave') leftTable(t, who);
        else if (m.m && typeof m.m === 'object' && !Array.isArray(m.m)) move(t.id, from, m.m, who.name);
      }
    }
  }

  function leftTable(t0, who) {
    const cur = tables.get(t0.id);
    if (!cur) return;
    if (!cur.started) { update({ ...cur, players: cur.players.filter((p) => p.uid !== who.uid) }); return; }
    const seat = cur.players.findIndex((p) => p.uid === who.uid);
    if (['BLACKJACK', 'POKER', 'ROULETTE'].includes(cur.kind)) move(cur.id, who.uid, { op: 'leave' }, who.name);
    else if (seat >= 0 && cur.state?.final !== true && cur.state?.resigned == null && R.turnBased(cur.kind)) {
      if (forfeits(cur)) { if (cur.state) settle(cur, R.forfeit(cur.kind, cur.state, seat, cur.players.length, 'gone')); return; }
      if (!gotUp.has(cur.id)) gotUp.set(cur.id, new Set());
      gotUp.get(cur.id).add(who.uid);
    }
    const n = tables.get(t0.id) || cur;
    update({ ...n, note: text('gNoteLeft', { name: who.name }) });
  }

  const forfeits = (t) => R.turnBased(t.kind) && t.players.length === 2;
  const ctxOf = (players, host) => ({ players, host, purse: (i) => Lei.of(players[i].uid) });

  function refund(t) {
    for (const w of t.wagers) Lei.add(w.uid, w.name, w.amount);
    if (t.started) for (const [seat, v] of Object.entries(R.committed(t.kind, t.state, secret.get(t.id)))) { const p = t.players[+seat]; if (p) Lei.add(p.uid, p.name, v); }
  }

  function newId() { return 'g' + now().toString(36) + rnd.nextInt(100); }

  function create(kind) {
    const my = me();
    if (!my.uid || !R.playable(kind)) return null;
    const t = { id: newId(), kind, host: my.uid, players: [my], started: false, state: null, note: '', seen: now(), startedAt: 0, wagers: [], tour: '' };
    update(t);
    openId = t.id;
    if (R.kindMin(kind) === 1 && R.kindMax(kind) === 1) startGame(t.id);
    return t.id;
  }

  function join(id) {
    const t = tables.get(id);
    if (!t) return;
    const my = me();
    invites = invites.filter((i) => i.table !== id);
    if (t.players.some((p) => p.uid === my.uid)) { openId = id; changed(); return; }
    if (t.host === my.uid) {
      if (!t.started && t.players.length < R.kindMax(t.kind)) update({ ...t, players: [...t.players, my] });
      openId = id; changed(); return;
    }
    send({ t: 'g', op: 'join', id, name: my.name });
    openId = id;
    changed();
  }

  function leave(id) {
    const t = tables.get(id);
    if (!t) return;
    const my = me();
    if (t.host === my.uid) {
      refund(t);
      send({ t: 'g', op: 'closed', id });
      tables.delete(id);
      secret.delete(id);
      forget(id);
      prune();
    } else send({ t: 'g', op: 'leave', id, name: my.name });
    if (openId === id) openId = null;
    changed();
  }

  function startGame(id, shuffle = true) {
    const t = tables.get(id);
    if (!t || t.host !== me().uid || (t.players.length < R.kindMin(t.kind) && !t.tour) || !R.playable(t.kind)) return;
    refund(t);
    const players = shuffle && t.kind !== 'POKER' && t.kind !== 'ROULETTE' ? shuffled(t.players, rnd) : t.players.slice();
    const o = R.start(t.kind, ctxOf(players, true), t.state, rnd, now());
    if (o.secret) secret.set(id, o.secret); else secret.delete(id);
    forget(id);
    lastMove.set(id, now());
    update({ ...t, players, started: true, state: o.state, note: '', startedAt: now(), wagers: [] });
    sendPrivates({ ...t, players }, o);
  }

  function play(id, m) {
    const t = tables.get(id);
    if (!t) return;
    const my = me();
    if (t.host === my.uid) move(t.id, my.uid, m, my.name);
    else send({ t: 'g', op: 'move', id, m, name: my.name });
  }

  // The keeper applies from's move m if the rules allow it, and settles what it decided; true when taken
  function move(id, from, m, name, auto = false) {
    const t = tables.get(id);
    if (!t || !t.state) return false;
    const st = t.state;
    if (m.op === 'wager') { wager(t, from, name, m); return true; }
    if (st.resigned !== undefined && st.resigned !== null) return false;
    const p = !from && m.op === 'tick' ? -1 : t.players.findIndex((x) => x.uid === from);
    if (p < 0 && (from || m.op !== 'tick')) return false;
    if (m.op === 'resign') {
      if (!R.turnBased(t.kind) || p < 0 || st.final === true) return false;
      settle(t, R.forfeit(t.kind, st, p, t.players.length, 'resign'));
      return true;
    }
    let o;
    try { o = R.move(t.kind, st, secret.get(t.id) || null, p, m, ctxOf(t.players, (from || t.host) === t.host), rnd, now()); } catch (e) { o = null; }
    if (!o) return false;
    if (p >= 0 && o.pay[p] !== undefined && o.pay[p] < 0 && -o.pay[p] > Lei.of(from)) return false;
    if (!auto && p >= 0) afk.get(id)?.delete(from);
    settle(t, o);
    return true;
  }

  function settle(t, o) {
    if (o.secret) secret.set(t.id, o.secret);
    lastMove.set(t.id, now());
    for (const [i, d] of Object.entries(o.pay)) { const pl = t.players[+i]; if (pl) Lei.add(pl.uid, pl.name, d); }
    for (const [i, d] of Object.entries(o.profit ?? o.pay)) { const pl = t.players[+i]; if (pl) Scores.profit(pl.uid, pl.name, t.kind, d); }
    if (o.over) {
      const seats = o.played ?? [...t.players.keys()];
      Scores.result(t.kind, seats.map((s) => t.players[s]), o.over.map((w) => seats.indexOf(w)).filter((i) => i >= 0));
    }
    try {
      if (o.final && o.over && o.over.length) feed(R.emoji(t.kind), text('gFeedWon', { names: o.over.map((i) => t.players[i]?.name).filter(Boolean).join(', '), game: title(t.kind) }));
      for (const [i, d] of Object.entries(o.profit ?? o.pay)) if (d >= 1000) { const pl = t.players[+i]; if (pl) feed('💰', text('gFeedBig', { name: pl.name, amount: fmtLei(d), game: title(t.kind) })); }
    } catch { /* the feed is extra */ }
    let next = { ...t, state: o.final ? { ...o.state, final: true } : o.state, note: '' };
    if (o.final) next = settleWagers(next, o.over || []);
    update(next);
    sendPrivates(t, o);
  }

  function sendPrivates(t, o) {
    const my = me().uid;
    for (const [i, v] of Object.entries(o.privates || {})) {
      const pl = t.players[+i];
      if (!pl) continue;
      if (pl.uid === my) privates.set(t.id, v);
      else send({ t: 'g', op: 'private', id: t.id, to: pl.uid, v });
    }
  }

  function wager(t, from, name, m) {
    const on = toInt(m.on), amount = toInt(m.v);
    if (on === null || amount === null) return;
    if (!betsOpen(t) || on < 0 || on >= t.players.length || t.players.some((p) => p.uid === from)) return;
    if (amount < 1 || amount > Lei.of(from)) return;
    Lei.add(from, name, -amount);
    update({ ...t, wagers: [...t.wagers, { uid: from, name, on, amount }] });
  }

  // The pool goes to those who bet on the winner, by how much each put in; nobody right (or a draw): all back
  function settleWagers(t, winners) {
    if (!t.wagers.length) return t;
    const pool = t.wagers.reduce((a, w) => a + w.amount, 0);
    const right = t.wagers.filter((w) => winners.includes(w.on));
    if (!right.length) for (const w of t.wagers) Lei.add(w.uid, w.name, w.amount);
    else {
      const stake = right.reduce((a, w) => a + w.amount, 0);
      const shares = right.map((w) => Math.floor(pool * w.amount / stake));
      const rest = pool - shares.reduce((a, b) => a + b, 0);
      right.forEach((w, k) => {
        const back = shares[k] + (k === 0 ? rest : 0);
        Lei.add(w.uid, w.name, back);
        Scores.profit(w.uid, w.name, t.kind, back - w.amount);
      });
      for (const w of t.wagers) if (!right.includes(w)) Scores.profit(w.uid, w.name, t.kind, -w.amount);
    }
    return { ...t, wagers: [], note: right.length ? text('gBetsPaid', { amount: fmtLei(pool) }) : text('gBetsBack') };
  }

  // ====== The clock (the keeper watches the player to move, and who's gone) ======

  function watchTurns(t0) {
    const my = me().uid;
    const mine = [...tables.values()].filter((t) => t.host === my && t.started && !R.isParty(t.kind));
    if (!mine.length) return;
    if (!env.online()) { for (const t of mine) lastMove.set(t.id, t0); return; }
    const here = env.present();
    if (here) {
      const seated = new Set(mine.flatMap((t) => t.players.map((p) => p.uid)));
      for (const u of seated) { if (u === my || here.has(u)) goneSince.delete(u); else if (!goneSince.has(u)) goneSince.set(u, t0); }
      for (const u of [...goneSince.keys()]) if (!seated.has(u)) goneSince.delete(u);
    }
    for (const t of mine) {
      const st = t.state;
      if (!st || st.final === true || st.resigned != null) continue;
      const waiting = R.waitingOn(t.kind, st, secret.get(t.id)).filter((i) => i >= 0 && i < t.players.length);
      if (!lastMove.has(t.id)) lastMove.set(t.id, t0);
      const since = lastMove.get(t.id);
      if (!waiting.length) continue;
      const skipped = afk.get(t.id) || new Set();
      const limit = (seat) => (skipped.has(t.players[seat].uid) ? AFK_TURN_MS : TURN_MS);
      for (const seat of waiting) {
        const uid = t.players[seat].uid;
        const gone = gotUp.get(t.id)?.has(uid) || (goneSince.has(uid) && t0 - goneSince.get(uid) >= GONE_MS);
        if (gone || t0 - since >= limit(seat)) { timeUp(t.id, seat, uid, gone ? 'gone' : 'time'); return; }
      }
      const late = waiting.filter((s) => since + limit(s) - t0 <= WARN_MS);
      if (late.length) warn(t.id, late, since + Math.min(...late.map(limit)));
    }
  }

  function warn(id, seats, deadline) {
    const t = tables.get(id);
    if (!t?.state) return;
    const was = t.state.afk;
    if (was && toInt(was.dl) === deadline && JSON.stringify(was.s) === JSON.stringify(seats)) return;
    update({ ...t, state: { ...t.state, afk: { s: seats, dl: deadline, f: forfeits(t) } } });
  }

  function timeUp(id, seat, uid, why) {
    const t = tables.get(id);
    const pl = t?.players[seat];
    if (!pl || pl.uid !== uid || !t.state) return;
    const st = t.state;
    if (st.final === true || st.resigned != null || !R.waitingOn(t.kind, st, secret.get(id)).includes(seat)) return;
    const t0 = now();
    const gone = gotUp.get(id)?.has(uid) || (goneSince.has(uid) && t0 - goneSince.get(uid) >= GONE_MS);
    if (!gone) {
      const since = lastMove.get(id);
      if (since === undefined || t0 - since < (afk.get(id)?.has(uid) ? AFK_TURN_MS : TURN_MS)) return;
    }
    if (forfeits(t)) { settle(t, R.forfeit(t.kind, st, seat, t.players.length, why)); return; }
    if (why === 'time') { if (!afk.has(id)) afk.set(id, new Set()); afk.get(id).add(uid); }
    let played = false;
    for (let k = 0; k < 3; k++) {
      const cur = tables.get(id)?.state;
      if (!cur) return;
      if (!R.waitingOn(t.kind, cur, secret.get(id)).includes(seat)) break;
      const tries = R.isChance(t.kind) && why === 'gone' ? [{ op: 'leave' }] : R.autoMoves(t.kind, cur, secret.get(id), seat);
      if (!tries.some((m) => move(id, uid, m, pl.name, true))) break;
      played = true;
    }
    const n = tables.get(id);
    if (!n) return;
    if (!played) {
      if (R.turnBased(t.kind)) { if (n.state) settle(n, R.forfeit(t.kind, n.state, seat, t.players.length, why)); } else lastMove.set(id, now());
      return;
    }
    update({ ...n, note: text(why === 'time' ? 'gNoteTime' : 'gNoteGone', { name: pl.name }) });
  }

  // Party games move on by the clock: the keeper's "tick" (from no seat) once a round's time is up
  function partyTick(t0) {
    const my = me().uid;
    for (const t of [...tables.values()]) {
      if (t.host !== my || !t.started || !R.isParty(t.kind) || !t.state) continue;
      const dl = typeof t.state.dl === 'number' ? t.state.dl : 0;
      if (t.state.final !== true && dl > 0 && t0 >= dl) move(t.id, '', { op: 'tick' }, '');
    }
  }

  // ====== Talk at a table ======

  function heard(id, line) {
    talk.set(id, [...(talk.get(id) || []), line].slice(-60));
    changed();
  }
  function say(id, x, emojiOnly = false) {
    const s = String(x).trim().slice(0, emojiOnly ? 8 : 200);
    if (!s) return;
    const my = me();
    heard(id, { name: my.name, uid: my.uid, text: s, emoji: emojiOnly, at: now() });
    send({ t: 'gchat', id, n: my.name, u: my.uid, x: s, e: emojiOnly });
  }

  function prune() {
    for (const m of [privates, talk]) for (const k of [...m.keys()]) if (!tables.has(k)) m.delete(k);
  }

  // Every 15 s: our tables announced again, others' that went quiet dropped, idle casino tables closed, the daily bonus
  function slowTick() {
    const my = me();
    const t0 = now();
    for (const t of tables.values()) if (t.host === my.uid) announce(t);
    for (const [id, t] of [...tables]) if (t.host !== my.uid && t0 - t.seen >= 45000) tables.delete(id);
    prune();
    for (const t of [...tables.values()]) if (t.host === my.uid && R.isChance(t.kind) && t.started && t0 - (lastMove.get(t.id) ?? t0) > IDLE_CLOSE_MS) leave(t.id);
    invites = invites.filter((i) => t0 - i.at < 120000 && tables.has(i.table));
    if (env.online() && my.uid) {
      const got = Lei.claimDaily(my.uid, my.name);
      if (got > 0) env.toast?.(text('gDaily', { amount: fmtLei(got) }));
      Scores.archiveLastWeek();
    }
    changed();
  }

  // The server is left: our tables close (their chips and bets go back), we get up from the others'
  // (sendVia: where those last messages go when the page's bus is already down)
  function leftServer(sendVia) {
    const my = me().uid;
    const was = sendFn;
    if (sendVia) sendFn = sendVia;
    try {
      for (const t of [...tables.values()]) if (t.host === my || t.players.some((p) => p.uid === my)) { try { leave(t.id); } catch { /* going anyway */ } }
    } finally { sendFn = was; }
    tables.clear(); secret.clear(); privates.clear(); talk.clear(); invites = [];
    openId = null;
    changed();
  }

  const timers = [];
  function start(setIntervalFn = setInterval) {
    // As the apps: the first round 5 s in (the daily bonus), then every 15 s
    const slow = () => { try { slowTick(); } catch (e) { console.error('games', e); } };
    setTimeout(slow, 5000);
    timers.push(setIntervalFn(slow, 15000));
    timers.push(setIntervalFn(() => { try { const t0 = now(); partyTick(t0); watchTurns(t0); } catch (e) { console.error('games', e); } }, 400));
  }
  function stop() { for (const t of timers.splice(0)) clearInterval(t); }

  return {
    tables, privates, talk, Lei, Scores,
    get invites() { return invites; },
    get open() { return openId; }, set open(v) { openId = v; changed(); },
    onChange: (f) => listeners.push(f),
    handle, tableFrom, create, join, leave, startGame, play, again: (id) => startGame(id),
    resign: (id) => play(id, { op: 'resign' }), bet: (id, seat, amount) => play(id, { op: 'wager', on: seat, v: amount }),
    invite: (id, uid) => send({ t: 'g', op: 'invite', id, to: uid, name: me().name }),
    say, betsOpen, watchTurns, partyTick, slowTick, leftServer, start, stop, me,
    secretOf: (id) => secret.get(id),
  };
}
