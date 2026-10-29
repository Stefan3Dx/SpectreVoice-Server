// Spectre Voice web client: the page the server serves at /app/. Nothing to install: voice and chat, streams
// (watching, and sharing the screen), the server's TV, channel files. The identity is a P-256 key made here and kept
// in IndexedDB; the server is the page's own host.
//
// This file: the connection (/v1/ws: hello v2, welcome, reconnect), the channel tree with its people, and the
// pages. The rest is in modules (each registers itself with on(…) from core.js):
//   core.js (state, events, helpers)  i18n.js (texts)  identity.js (key)  voice.js  chat.js  streams.js  tv.js
//   files.js (channel files)  settings.js

import { $, S, el, send, toast, on, emit, me, may, SERVER_CHAT, store, openDialog } from './core.js';
import { t, initLang, applyTexts, errText } from './i18n.js';
import { loadIdentity, signChallenge } from './identity.js';
import { voiceStart, applyVolumes, updatePTT } from './voice.js';
import { loadChat, openConv, renderChat, updateUnread } from './chat.js';
import { watchRoom, liveOf } from './streams.js';
import './tv.js';
import './files.js';
import './dm.js';
import { openSettings } from './settings.js';

function setStatus(phase) {
  S.phase = phase;
  const e = $('status');
  e.className = 'status ' + ({ on: 'ok', connecting: 'warn', reconnecting: 'warn' }[phase] || '');
  e.textContent = t({ on: 'connected', connecting: 'connecting', reconnecting: 'reconnecting' }[phase] || 'off');
}

// ====== Connection ======

function connect() {
  clearTimeout(S.timer);
  const url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/v1/ws';
  setStatus(S.wasOn ? 'reconnecting' : 'connecting');
  let ws;
  try { ws = new WebSocket(url); } catch { failed(t('errWs')); return; }
  S.ws = ws;
  ws.onmessage = (ev) => {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (ws === S.ws) handle(m);
  };
  ws.onclose = () => {
    if (ws !== S.ws) return;
    S.ws = null;
    emit('app:down', { retry: S.phase !== 'off' });
    if (S.phase === 'off') return;
    // Back off: 1, 2, 4 … 30 s
    const wait = Math.min(30000, 1000 * 2 ** Math.min(S.retry++, 5));
    if (S.wasOn && S.phase === 'on') toast(t('lost'));
    setStatus('reconnecting');
    S.timer = setTimeout(connect, wait);
  };
  ws.onerror = () => { /* onclose follows */ };
}
function failed(msg) {
  // Refused on hello, or asked to leave: no retry by itself
  clearTimeout(S.timer);
  setStatus('off');
  const ws = S.ws;
  S.ws = null;
  if (ws) ws.close();
  emit('app:down', { retry: false });
  S.wasOn = false;
  S.retry = 0;
  $('app').classList.add('hidden');
  $('leave').classList.add('hidden');
  $('connect').classList.remove('hidden');
  const e = $('connectError');
  e.textContent = msg || '';
  e.classList.toggle('hidden', !msg);
  $('go').disabled = false;
}

// The server has a password (or it changed): asked for, then the hello again
function askPassword(wrong) {
  const input = el('input', { type: 'password', id: 'pwInput', maxLength: 200, autocomplete: 'current-password' });
  const form = el('form', { class: 'stack', onsubmit: (ev) => { ev.preventDefault(); go(); } },
    el('p', { class: 'muted', text: t('passwordAsk') }),
    wrong ? el('p', { class: 'error', text: t('errPassword') }) : null,
    el('label', {}, el('span', { text: t('password') }), input));
  let sent = false;
  const go = () => {
    if (!input.value) return false;
    sent = true;
    S.pass = input.value;
    $('connectError').classList.add('hidden');
    $('go').disabled = true;
    S.retry = 0;
    connect();
    d.close();
    return true;
  };
  const d = openDialog({
    title: t('passwordTitle'), body: form,
    actions: [{ text: t('cancel') }, { text: t('connect'), primary: true, id: 'pwGo', onclick: () => go() }],
    onclose: () => { if (!sent) $('go').disabled = false; },
  });
  setTimeout(() => input.focus(), 30);
}

async function handle(m) {
  switch (m.t) {
    case 'challenge': {
      S.version = m.version || '';
      const sig = await signChallenge(S.id, m.n);
      send({ t: 'hello', v: 2, pub: S.id.pub, sig, name: S.name, client: 'web/' + (S.version || 'dev'), password: S.pass, invite: S.invite || undefined, dev: S.dev, ch: S.wasOn && S.lastCh ? S.lastCh : undefined });
      return;
    }
    case 'welcome': welcome(m); break;
    case 'error': {
      if (m.op === 'hello') {
        const wrong = !!S.pass;
        failed(m.why === 'password' ? '' : errText(m));
        if (m.why === 'password') { S.pass = ''; askPassword(wrong); }
        return;
      }
      if (m.op === 'history') break;
      emit('error', m);
      if (!m.handled) toast(errText(m), true);
      return;
    }
    case 'server-info':
      S.server = m.server?.name || S.server;
      $('serverName').textContent = S.server || 'Spectre Voice';
      $('commName').textContent = S.server;
      updateUnread();
      break;
    case 'channel': S.channels.set(m.c.id, m.c); render(); break;
    case 'channel-del': S.channels.delete(m.id); render(); break;
    case 'member': memberChanged(m.m); break;
    case 'member-left': S.members.delete(m.sid); render(); break;
    case 'talk': {
      const mem = S.members.get(m.sid);
      if (mem) { mem.talk = m.on; renderTalk(m.sid, m.on); }
      break;
    }
    case 'moved': toast(t('moved', { by: m.by, ch: S.channels.get(m.ch)?.name || '?' })); break;
    case 'kicked':
      S.phase = 'off';
      failed(t(m.ban ? 'banned' : 'kicked', { by: m.by }) + (m.why ? ': ' + m.why : ''));
      break;
    case 'role':
      if (Array.isArray(m.perms)) S.perms = new Set(m.perms);
      if (typeof m.staff === 'boolean') S.staff = m.staff;
      emit('app:perms');
      render();
      break;
    case 'live': {
      const room = m.s?.room;
      if (!room) break;
      if (m.on) S.live.set(room, m.s); else S.live.delete(room);
      render();
      break;
    }
    case 'poke': toast('👉 ' + m.name + ': ' + (m.text || '')); break;
    case 'announce': toast('📢 ' + m.text); break;
    case 'event-notice': toast(t(m.kind === 'soon' ? 'evSoon' : 'evStart', { title: m.e?.title || '' })); break;
    case 'lei-gift': toast(t('leiGift', { by: m.byName || '', amount: (m.amount > 0 ? '+' : '') + m.amount }) + (m.reason ? ' · ' + m.reason : '')); break;
  }
  emit(m.t, m);
}

function welcome(m) {
  const back = S.wasOn;
  const last = S.lastCh;
  S.sid = m.sid;
  S.uid = m.uid;
  S.token = m.token || '';
  S.retry = 0;
  S.server = m.server?.name || '';
  S.ice = m.server?.ice || [];
  S.perms = new Set(m.perms || []);
  S.staff = !!m.staff;
  S.files = !!m.server?.files;
  S.live = new Map((m.live || []).map((l) => [l.room, l]));
  $('serverName').textContent = S.server || 'Spectre Voice';
  $('motd').textContent = m.server?.motd || '';
  $('motd').classList.toggle('hidden', !m.server?.motd);
  setStatus('on');
  $('connect').classList.add('hidden');
  $('app').classList.remove('hidden');
  $('leave').classList.remove('hidden');
  entered(m);
  if (S.mic || S.snd) send({ t: 'state', mic: S.mic, snd: S.snd });
  voiceStart();
  S.wasOn = true;
  emit('app:welcome', m);
  if (back) {
    toast(t('back'));
    const c = S.channels.get(last);
    if (c && !c.locked && me()?.ch !== last) send({ t: 'join', ch: last });
  }
  // The invite was used in the hello (its channel too): not again on a reconnect
  if (S.invite) S.invite = '';
  else if (S.wantCh) { joinChannel(S.wantCh); S.wantCh = 0; }
}

// entered: the server's channels and people from a welcome
function entered(m) {
  S.channels = new Map((m.channels || []).map((c) => [c.id, c]));
  S.members = new Map((m.members || []).map((x) => [x.sid, x]));
  const keep = S.cur;
  S.chat = new Map();
  const ch = me()?.ch || 0;
  S.lastCh = ch;
  // Back after a drop: the conversation open stays open
  S.cur = keep && (keep.startsWith('g:') || S.channels.has(+keep.slice(2))) && S.wasOn ? keep : (ch ? 'c:' + ch : SERVER_CHAT);
  $('commName').textContent = S.server;
  render();
  loadChat();
}

function memberChanged(mem) {
  const before = S.members.get(mem.sid);
  S.members.set(mem.sid, mem);
  if (mem.sid === S.sid && before?.ch !== mem.ch) {
    S.lastCh = mem.ch;
    if (!S.cur.startsWith('g:') || !before) S.cur = 'c:' + mem.ch;
    S.unread.delete(S.cur);
    updateUnread();
    loadChat();
  }
  render();
  applyVolumes();
}

// ====== Channels and members ======

function joinChannel(id) {
  const c = S.channels.get(id);
  if (!c || c.kind) return;
  if (c.locked && me()?.ch !== id) {
    const input = el('input', { type: 'password', maxLength: 200 });
    const join = () => { send({ t: 'join', ch: id, password: input.value }); };
    openDialog({
      title: t('chPass'), body: el('form', { class: 'stack', onsubmit: (ev) => { ev.preventDefault(); join(); $('dlg').close(); } }, el('label', {}, el('span', { text: c.name }), input)),
      actions: [{ text: t('cancel') }, { text: t('join'), primary: true, onclick: join }],
    });
    setTimeout(() => input.focus(), 30);
    return;
  }
  send({ t: 'join', ch: id });
}

let renderQueued = false;
on('app:dirty', () => {
  if (renderQueued) return;
  renderQueued = true;
  requestAnimationFrame(() => { renderQueued = false; render(); });
});

function unreadBadge(to) {
  const n = S.unread.get(to) || 0;
  return n ? el('span', { class: 'badge', text: n > 99 ? '99+' : String(n) }) : null;
}

function render() {
  const tree = $('tree');
  tree.textContent = '';
  const kids = new Map();
  for (const c of S.channels.values()) {
    if (!kids.has(c.parent)) kids.set(c.parent, []);
    kids.get(c.parent).push(c);
  }
  for (const l of kids.values()) l.sort((a, b) => a.pos - b.pos || a.id - b.id);
  const byCh = new Map();
  for (const mem of S.members.values()) {
    if (!byCh.has(mem.ch)) byCh.set(mem.ch, []);
    byCh.get(mem.ch).push(mem);
  }
  const mine = me()?.ch;
  const draw = (parent, into, depth) => {
    for (const c of kids.get(parent) || []) {
      if (c.kind === 'spacer') {
        into.appendChild(el('div', { class: 'spacer', text: c.name }));
      } else {
        const row = el('div', {
          class: 'ch' + (c.kind === 'text' ? ' text' : '') + (c.locked ? ' locked' : '') + (c.id === mine ? ' mine' : '') + (S.cur === 'c:' + c.id ? ' cur' : ''),
          title: c.topic || (c.temp ? t('temp') : ''), dataset: { id: c.id },
        }, el('span', { class: 'chname', text: (c.temp ? '⏳ ' : '') + (c.icon ? c.icon + ' ' : '') + c.name }));
        const badge = unreadBadge('c:' + c.id);
        if (badge) row.appendChild(badge);
        if (!c.kind) {
          const n = (byCh.get(c.id) || []).length;
          if (n) row.appendChild(el('span', { class: 'cnt', text: n + (c.max ? '/' + c.max : '') }));
          row.onclick = () => { joinChannel(c.id); openConv('c:' + c.id); };
        } else {
          row.onclick = () => { openConv('c:' + c.id); showPane('chat'); };
        }
        into.appendChild(row);
        for (const mem of (byCh.get(c.id) || []).sort((a, b) => a.name.localeCompare(b.name))) into.appendChild(memberRow(mem));
      }
      if (kids.has(c.id) && depth < 10) {
        const box = el('div', { class: 'kids' });
        draw(c.id, box, depth + 1);
        into.appendChild(box);
      }
    }
  };
  draw(0, tree, 0);
  const cur = S.channels.get(+S.cur.slice(2));
  $('tabChannel').textContent = S.cur.startsWith('c:') && cur ? '#' + cur.name : (S.channels.get(mine)?.name ? '#' + S.channels.get(mine).name : t('chat'));
  const view = $('chat').dataset.view || 'chat';
  $('tabChannel').classList.toggle('on', view === 'chat' && S.cur.startsWith('c:'));
  $('tabComm').classList.toggle('on', view === 'chat' && S.cur.startsWith('g:'));
  $('mute').classList.toggle('muted-on', S.mic);
  $('deafen').classList.toggle('muted-on', S.snd);
  updatePTT();
  emit('app:render');
}
on('app:selfstate', render);
on('app:conv', render);
on('app:view', render);

function memberRow(mem) {
  const lives = liveOf(mem.uid);
  const row = el('div', { class: 'mem' + (mem.talk ? ' talking' : '') + (mem.sid === S.sid ? ' me' : '') + (mem.away ? ' away' : ''), dataset: { sid: mem.sid } },
    el('span', { class: 'dot' }),
    el('span', { class: 'mname', text: mem.name }),
    el('span', { class: 'ic', text: (mem.bot ? ' 🤖' : '') + (mem.mic ? ' 🎙️✕' : '') + (mem.snd ? ' 🔇' : '') + (mem.vmuted ? ' 🚫' : '') + (mem.away ? ' 💤' : '') + (S.muted[mem.uid] ? ' 🔕' : '') }));
  for (const l of lives) {
    row.append(el('button', {
      class: 'livebadge', type: 'button', text: t('liveBadge'), title: t('watchName', { name: mem.name }),
      onclick: (ev) => { ev.stopPropagation(); watchRoom(l.room); showPane('stage'); },
    }));
  }
  if (mem.client) row.appendChild(el('span', { class: 'cli', text: mem.client.split('/')[0] }));
  if (mem.sid !== S.sid && !mem.bot) row.onclick = (ev) => personPop(mem, ev);
  return row;
}
function renderTalk(sid, onv) {
  for (const e of document.querySelectorAll(`.mem[data-sid="${sid}"]`)) e.classList.toggle('talking', onv);
}

// A person's card: volume and mute (kept per uid in this browser), and their streams
function personPop(mem, ev) {
  const pop = $('volPop');
  pop.textContent = '';
  const v = el('input', { type: 'range', min: 0, max: 100, id: 'vol', value: Math.round((S.volumes[mem.uid] ?? 1) * 100) });
  const label = el('span', { class: 'muted small', text: v.value + '%' });
  v.oninput = () => {
    S.volumes[mem.uid] = v.value / 100;
    label.textContent = v.value + '%';
    store.set('sv.volumes', JSON.stringify(S.volumes));
    applyVolumes();
  };
  const mute = el('input', { type: 'checkbox', checked: !!S.muted[mem.uid] });
  mute.onchange = () => {
    if (mute.checked) S.muted[mem.uid] = 1; else delete S.muted[mem.uid];
    store.set('sv.mutedPeople', JSON.stringify(S.muted));
    applyVolumes();
    render();
  };
  pop.append(el('div', { class: 'popname', text: mem.name }),
    el('div', { class: 'row' }, el('span', { class: 'small', text: t('volume') }), label), v,
    el('label', { class: 'chk' }, mute, el('span', { text: t('mutePerson') })));
  for (const l of liveOf(mem.uid)) {
    pop.append(el('button', { class: 'primary small', type: 'button', text: '▶ ' + (l.title || t('watch')), onclick: () => { watchRoom(l.room); showPane('stage'); pop.classList.add('hidden'); } }));
  }
  emit('app:personPop', { mem, pop });
  pop.classList.remove('hidden');
  const r = ev.currentTarget.getBoundingClientRect();
  pop.style.left = Math.max(8, Math.min(r.left, innerWidth - 248)) + 'px';
  pop.style.top = Math.min(r.bottom + 4, innerHeight - pop.offsetHeight - 8) + 'px';
  ev.stopPropagation();
}
document.addEventListener('click', (ev) => { if (!$('volPop').contains(ev.target)) $('volPop').classList.add('hidden'); });

// ====== Pages (phones: one pane at a time) ======

export function showPane(name) {
  document.body.dataset.pane = name;
  for (const b of document.querySelectorAll('.mobile-tabs button')) b.classList.toggle('on', b.dataset.pane === name);
}
on('app:showPane', (p) => showPane(p));
for (const b of document.querySelectorAll('.mobile-tabs button')) b.onclick = () => showPane(b.dataset.pane);

on('app:lang', () => { setStatus(S.phase); if (S.phase === 'on') { render(); renderChat(); } });
$('settings').onclick = () => openSettings();
$('leave').onclick = () => { S.phase = 'off'; failed(''); };
$('form').onsubmit = async (ev) => {
  ev.preventDefault();
  S.name = $('name').value.trim();
  if (!S.name) return;
  store.set('sv.name', S.name);
  $('go').disabled = true;
  $('connectError').classList.add('hidden');
  S.retry = 0;
  S.wasOn = false;
  connect();
};

async function init() {
  await initLang();
  const q = new URLSearchParams(location.search);
  S.invite = (q.get('invite') || '').replace(/[^A-Za-z0-9]/g, '').slice(0, 16);
  S.wantCh = +(q.get('ch') || 0) || 0;
  if (q.get('name')) $('name').value = q.get('name').slice(0, 32);
  else $('name').value = store.get('sv.name', '');
  S.open = store.get('sv.open', '') === '1';
  $('openMic').checked = S.open;
  showPane('channels');
  applyTexts();
  emit('app:ptt');
  setStatus('off');
  if (S.invite) {
    $('inviteNote').textContent = t('inviteNote', { code: S.invite });
    $('inviteNote').classList.remove('hidden');
  }
  try {
    S.id = await loadIdentity();
    $('uidNote').textContent = 'ID: ' + S.id.uid;
  } catch (e) {
    // Plain http on a non-local address: no crypto.subtle (nor microphone), whatever the browser
    $('connectError').textContent = window.isSecureContext ? 'WebCrypto/IndexedDB: ' + (e?.message || e) : t('needsHttps');
    $('connectError').classList.remove('hidden');
    $('go').disabled = true;
  }
  document.body.dataset.ready = '1';
}
init();
