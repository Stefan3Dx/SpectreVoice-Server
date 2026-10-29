// Spectre Voice web client: the page the server serves at /app/. Nothing to install: voice and chat, streams
// (watching, and sharing the screen), the server's TV, channel files. The identity is a P-256 key made here and kept
// in IndexedDB; the server is the page's own host.
//
// This file: the connection (/v1/ws: hello v2, welcome, reconnect), the channel tree with its people, and the
// pages. The rest is in modules (each registers itself with on(…) from core.js):
//   core.js (state, events, helpers)  i18n.js (texts)  identity.js (key)  voice.js  chat.js  streams.js  tv.js
//   files.js (channel files)  settings.js

import { $, S, el, send, toast, on, emit, me, may, SERVER_CHAT, store, openDialog, chLabel } from './core.js';
import { t, initLang, applyTexts, errText } from './i18n.js';
import { loadIdentity, signChallenge } from './identity.js';
import { voiceStart, applyVolumes, updatePTT } from './voice.js';
import { loadChat, openConv, renderChat, updateUnread } from './chat.js';
import { watchRoom, liveOf, watching, stopWatching } from './streams.js';
import './bus.js';
import './tv.js';
import './files.js';
import './dm.js';
import './soundboard.js'; import './music.js';
import './games/ui.js';
import { openSettings } from './settings.js';
import { icon, setIcon } from './icons.js';
import { avatar, avatarPath, hueOf, initials } from './avatar.js';

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
  document.body.classList.remove('inapp');
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
      $('srvIcon').textContent = initials(S.server || 'Spectre Voice');
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
    case 'moved': toast(t('moved', { by: m.by, ch: S.channels.get(m.ch)?.name || m.name || '?' })); break;
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
  $('srvIcon').textContent = initials(S.server || 'Spectre Voice');
  $('motd').textContent = m.server?.motd || '';
  $('motd').classList.toggle('hidden', !m.server?.motd);
  setStatus('on');
  $('connect').classList.add('hidden');
  $('app').classList.remove('hidden');
  document.body.classList.add('inapp');
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
  const liveIn = new Set();
  for (const mem of S.members.values()) if (liveOf(mem.uid).length) liveIn.add(mem.ch);
  // The PC's tree: text channels first (they open their chat), then the voice channels with their people
  const texts = [...S.channels.values()].filter((c) => c.kind === 'text').sort((a, b) => a.pos - b.pos || a.id - b.id);
  const chRow = (c) => {
    const open = S.cur === 'c:' + c.id;
    const unread = S.unread.get('c:' + c.id) || 0;
    const row = el('div', {
      class: 'ch' + (c.kind === 'text' ? ' text' : ' voice') + (c.locked ? ' locked' : '') + (c.id === mine ? ' mine' : '') + (open ? ' cur' : '') + (c.temp ? ' temp' : '') + (unread ? ' unread' : ''),
      title: c.topic || (c.temp ? t('temp') : ''), dataset: { id: c.id },
    }, el('span', { class: 'chname', text: (c.icon ? c.icon + ' ' : '') + c.name }));
    // A voice channel shows a microphone (drawn first by CSS order; the name stays the row's first child); text keeps its #
    if (!c.kind) row.appendChild(el('span', { class: 'chic', title: t('voiceChannel') }, icon('mic')));
    const badge = unreadBadge('c:' + c.id);
    if (badge) row.appendChild(badge);
    if (!c.kind) {
      if (liveIn.has(c.id)) row.appendChild(el('span', { class: 'chflag', text: '📺', title: t('liveNow') }));
      const n = (byCh.get(c.id) || []).length;
      if (n) row.appendChild(el('span', { class: 'cnt', text: n + (c.max ? '/' + c.max : '') }));
      if (c.locked) row.appendChild(el('span', { class: 'chlock', title: t('chPass') }, icon('lock')));
      if (c.id !== mine) row.appendChild(el('button', { class: 'joinbtn', type: 'button', text: t('join'), onclick: (ev) => { ev.stopPropagation(); joinChannel(c.id); openConv('c:' + c.id); } }));
      row.onclick = () => { joinChannel(c.id); openConv('c:' + c.id); };
    } else {
      row.onclick = () => { openConv('c:' + c.id); showPane('chat'); };
    }
    return row;
  };
  if (texts.length) {
    tree.append(el('div', { class: 'group', text: t('textChannels') }));
    for (const c of texts) tree.append(chRow(c));
    tree.append(el('div', { class: 'group', text: t('voiceChannels') }));
  }
  const draw = (parent, into, depth) => {
    for (const c of kids.get(parent) || []) {
      if (c.kind === 'spacer') {
        into.appendChild(el('div', { class: 'spacer', text: c.name.replace(/^[─\-—\s]+|[─\-—\s]+$/g, '') }));
      } else if (c.kind !== 'text') {
        into.appendChild(chRow(c));
        const people = (byCh.get(c.id) || []).sort((a, b) => a.name.localeCompare(b.name));
        if (people.length) into.appendChild(el('div', { class: 'mems' }, people.map((mem) => memberRow(mem))));
      }
      if (kids.has(c.id) && depth < 10) {
        const box = el('div', { class: 'kids' });
        draw(c.id, box, depth + 1);
        if (box.childNodes.length) into.appendChild(box);
      }
    }
  };
  draw(0, tree, 0);
  const cur = S.channels.get(+S.cur.slice(2));
  $('tabChannel').textContent = S.cur.startsWith('c:') && cur ? chLabel(cur) : (S.channels.get(mine)?.name ? chLabel(S.channels.get(mine)) : t('chat'));
  const view = $('chat').dataset.view || 'chat';
  $('tabChannel').classList.toggle('on', view === 'chat' && S.cur.startsWith('c:'));
  $('tabComm').classList.toggle('on', view === 'chat' && S.cur.startsWith('g:'));
  renderMe();
  renderPeople();
  updatePTT();
  emit('app:render');
}
on('app:selfstate', render);
on('app:conv', render);
on('app:view', render);
on('app:rtc', renderMe);

// You, at the bottom of the channel list: your ghost (lit while you talk), name, channel, the voice's state, buttons
function renderMe() {
  const m = me();
  const box = $('meAvatar');
  const uid = S.uid;
  if (box.dataset.uid !== uid || box.dataset.v !== avatarPath(uid) + hueOf(uid, S.name)) {
    box.textContent = '';
    box.append(avatar(S.name, uid, 38));
    box.dataset.uid = uid;
    box.dataset.v = avatarPath(uid) + hueOf(uid, S.name);
  }
  box.classList.toggle('talking', !!m?.talk);
  box.classList.toggle('mic-off', S.mic);
  $('meName').textContent = m?.name || S.name;
  $('meWhere').textContent = chLabel(S.channels.get(m?.ch));
  const rtc = document.body.dataset.rtc || 'new';
  const up = rtc === 'connected';
  const down = rtc === 'failed' || rtc === 'disconnected';
  $('voiceDot').className = 'vdot' + (up ? ' up' : down ? ' down' : '');
  $('voiceText').textContent = t(up ? 'voiceOn' : down ? 'voiceDown' : 'voiceConnecting');
  const mute = $('mute');
  setIcon(mute, S.mic ? 'micOff' : 'mic');
  mute.classList.toggle('muted-on', S.mic);
  mute.setAttribute('aria-pressed', S.mic ? 'true' : 'false');
  const deaf = $('deafen');
  setIcon(deaf, S.snd ? 'headsetOff' : 'headset');
  deaf.classList.toggle('muted-on', S.snd);
  deaf.setAttribute('aria-pressed', S.snd ? 'true' : 'false');
}

// The right column: who talks now, who's live, the people in your channel
function renderPeople() {
  const ch = me()?.ch;
  const here = [...S.members.values()].filter((x) => x.ch === ch).sort((a, b) => a.name.localeCompare(b.name));
  renderTalkers(here);
  const live = $('peopleLive');
  live.textContent = '';
  const lives = here.flatMap((mem) => liveOf(mem.uid).map((l) => [mem, l]));
  if (lives.length) live.append(el('div', { class: 'section', text: t('liveTab') }));
  for (const [mem, l] of lives) {
    const self = mem.sid === S.sid;
    live.append(el('div', { class: 'liverow' }, icon('tv'),
      el('span', { class: 'lname', text: self ? t('youShare') : mem.name + (l.title ? ' · ' + l.title : '') }),
      self ? null : watching(l.room)
        ? el('button', { class: 'small', type: 'button', text: t('stopWatching'), onclick: () => { stopWatching(l.room); emit('app:dirty'); } })
        : el('button', { class: 'small primary', type: 'button', text: t('watch'), onclick: () => { watchRoom(l.room); showPane('stage'); emit('app:dirty'); } })));
  }
  $('hereTitle').textContent = t('inChannel', { ch: S.channels.get(ch)?.name || t('channel'), n: here.length });
  const list = $('hereList');
  list.textContent = '';
  for (const mem of here) list.append(memberRow(mem, true));
}
function renderTalkers(here) {
  const box = $('talkers');
  box.textContent = '';
  const talking = (here || [...S.members.values()].filter((x) => x.ch === me()?.ch)).filter((x) => x.talk);
  if (!talking.length) box.append(el('span', { class: 'quiet', text: t('quiet') }));
  for (const mem of talking) box.append(el('div', { class: 'talker' }, avatar(mem.name, mem.uid, 44), el('span', { class: 'tn', text: mem.name })));
}

function memberRow(mem, big) {
  const lives = liveOf(mem.uid);
  const self = mem.sid === S.sid;
  const row = el('div', { class: 'mem' + (big ? ' big' : '') + (mem.talk ? ' talking' : '') + (self ? ' me' : '') + (mem.away ? ' away' : ''), dataset: { sid: mem.sid } },
    avatar(mem.name, mem.uid, big ? 34 : 24),
    el('span', { class: 'mname', text: mem.name + (self ? t('youSuffix') : '') }));
  const flags = el('span', { class: 'flags' });
  const flag = (cls, title, kid) => flags.append(el('span', { class: cls, title }, kid));
  if (mem.bot) flag('f-emo', t('bot'), '🤖');
  if (mem.vmuted) flag('f-vm', t('cantBeHeard'), icon('micOff'));
  if (mem.away) flag('f-away', t('away') + (mem.awayMsg ? ': ' + mem.awayMsg : ''), icon('moon'));
  for (const l of lives) {
    flags.append(el('button', {
      class: 'livebadge', type: 'button', text: t('liveBadge'), title: t('watchName', { name: mem.name }),
      onclick: (ev) => { ev.stopPropagation(); watchRoom(l.room); showPane('stage'); },
    }));
  }
  if (S.muted[mem.uid]) flag('f-lm', t('mutedForYou'), icon('volOff'));
  if (mem.mic) flag('f-mic', t('micOffFlag'), icon('micOff'));
  if (mem.snd) flag('f-snd', t('soundOffFlag'), icon('headsetOff'));
  if (mem.client?.startsWith('web/')) flag('cli', mem.client, 'web');
  if (flags.childNodes.length) row.append(flags);
  if (!self && !mem.bot) row.onclick = (ev) => personPop(mem, ev);
  return row;
}
function renderTalk(sid, onv) {
  for (const e of document.querySelectorAll(`.mem[data-sid="${sid}"]`)) e.classList.toggle('talking', onv);
  if (sid === S.sid) $('meAvatar').classList.toggle('talking', onv);
  const mem = S.members.get(sid);
  if (mem && mem.ch === me()?.ch) renderTalkers();
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
  pop.append(el('div', { class: 'popname' }, avatar(mem.name, mem.uid, 40), el('span', { text: mem.name })),
    el('div', { class: 'row' }, el('span', { class: 'small', text: t('volume') }), label), v,
    el('label', { class: 'chk' }, mute, el('span', { text: t('mutePerson') })));
  for (const l of liveOf(mem.uid)) {
    pop.append(el('button', { class: 'primary small', type: 'button', text: '▶ ' + (l.title || t('watch')), onclick: () => { watchRoom(l.room); showPane('stage'); pop.classList.add('hidden'); } }));
  }
  // Moderators and admins (the move permission): to one's own channel, or any voice channel; the server checks ranks
  if (may('move')) {
    const mine = me()?.ch;
    const moveTo = (ch) => { send({ t: 'move', sid: mem.sid, ch }); pop.classList.add('hidden'); };
    if (mine && mem.ch !== mine) pop.append(el('button', { type: 'button', class: 'small', id: 'moveHere', text: t('moveHere'), onclick: () => moveTo(mine) }));
    pop.append(el('button', { type: 'button', class: 'small', id: 'moveTo', text: t('moveTo'), onclick: (e) => { e.stopPropagation(); movePicker(mem, pop, moveTo); } }));
  }
  emit('app:personPop', { mem, pop });
  pop.classList.remove('hidden');
  const r = ev.currentTarget.getBoundingClientRect();
  pop.style.left = Math.max(8, Math.min(r.left, innerWidth - 248)) + 'px';
  pop.style.top = Math.min(r.bottom + 4, innerHeight - pop.offsetHeight - 8) + 'px';
  ev.stopPropagation();
}
document.addEventListener('click', (ev) => { if (!$('volPop').contains(ev.target)) $('volPop').classList.add('hidden'); });

// "Move to…": the voice channels in the tree's order, the person's own marked
function movePicker(mem, pop, moveTo) {
  pop.textContent = '';
  pop.append(el('div', { class: 'popname' }, avatar(mem.name, mem.uid, 28), el('span', { text: t('moveTitle', { name: mem.name }) })));
  const kids = new Map();
  for (const c of S.channels.values()) { if (!kids.has(c.parent)) kids.set(c.parent, []); kids.get(c.parent).push(c); }
  for (const l of kids.values()) l.sort((a, b) => a.pos - b.pos || a.id - b.id);
  const list = el('div', { class: 'movelist' });
  const walk = (parent, depth) => {
    for (const c of kids.get(parent) || []) {
      if (!c.kind) {
        const here = c.id === mem.ch;
        list.append(el('button', { type: 'button', class: 'movech' + (here ? ' here' : ''), disabled: here, dataset: { id: c.id }, onclick: () => moveTo(c.id) },
          el('span', { class: 'chic' }, icon('mic')), el('span', { class: 'chname', text: (c.temp ? '⏳ ' : '') + (c.icon ? c.icon + ' ' : '') + c.name }),
          c.locked ? el('span', { class: 'chlock' }, icon('lock')) : null));
        list.lastChild.dataset.depth = String(Math.min(depth, 6));
      }
      if (kids.has(c.id) && depth < 10) walk(c.id, depth + 1);
    }
  };
  walk(0, 0);
  pop.append(list);
}

// ====== Pages (phones: one pane at a time) ======

export function showPane(name) {
  document.body.dataset.pane = name;
  for (const b of document.querySelectorAll('.mobile-tabs button')) b.classList.toggle('on', b.dataset.pane === name);
}
on('app:showPane', (p) => showPane(p));
for (const b of document.querySelectorAll('.mobile-tabs button')) b.onclick = () => showPane(b.dataset.pane);

on('app:lang', () => { setStatus(S.phase); if (S.phase === 'on') { render(); renderChat(); } });
$('settings').onclick = () => openSettings();
$('settingsPre').onclick = () => openSettings();
setIcon($('settings'), 'settings');
setIcon($('leave'), 'power');
setIcon($('shareBtn'), 'share');
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
