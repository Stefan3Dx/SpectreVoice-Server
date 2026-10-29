// Spectre Voice web client: the page the server serves at /app/. Voice (WebRTC through the server's SFU) and chat,
// nothing to install. The identity is a P-256 key made here and kept in IndexedDB; the server is the page's own host.

// ====== Helpers ======

const $ = (id) => document.getElementById(id);
const b64 = {
  enc: (bytes) => btoa(String.fromCharCode(...new Uint8Array(bytes))),
  dec: (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0)),
  url: (bytes) => b64.enc(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''),
};
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* private window */ } },
};
let toastTimer;
function toast(text, err) {
  const el = $('toast');
  el.textContent = text;
  el.classList.toggle('err', !!err);
  el.classList.remove('hidden');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add('hidden'), err ? 6000 : 3500);
}

// ====== Text (RO/EN) ======

const T = {
  ro: {
    off: 'Deconectat', connecting: 'Se conectează…', connected: 'Conectat', reconnecting: 'Reconectare…', leave: 'Ieși',
    welcome: 'Intră pe server', name: 'Numele tău', password: 'Parola serverului (dacă are)', connect: 'Conectează-te',
    idNote: 'Identitatea ta e o cheie făcută în acest browser; nu există cont.', channels: 'Canale', chat: 'Chat',
    serverChat: 'Server', older: 'Mesaje mai vechi', write: 'Scrie un mesaj…', send: 'Trimite',
    ptt: 'Ține apăsat ca să vorbești', openMic: 'Microfon deschis', mute: 'Mut', deafen: 'Surd',
    footer: 'Clientul web: voce și chat. Streamuri, jocuri, fișiere și mesaje private sunt în aplicație:',
    inviteNote: 'Ai o invitație ({code}) pe acest server.', chPass: 'Parola canalului',
    noMic: 'Microfonul nu e disponibil: doar asculți.', micDenied: 'Fără microfon (acces refuzat): doar asculți.',
    joined: 'Ai intrat în {ch}', moved: '{by} te-a mutat în {ch}', kicked: '{by} te-a dat afară', banned: '{by} te-a banat',
    back: 'Din nou pe server', lost: 'Conexiune pierdută; reîncerc…', deleted: '(mesaj șters)', edited: '(editat)',
    volume: 'Volum: {name}', you: 'tu', voiceOn: 'Voce conectată', voiceOff: 'Voce întreruptă', listenOnly: 'doar ascult',
    errPassword: 'Parola serverului e greșită.', errChPassword: 'Parola canalului e greșită.', errBanned: 'Ești banat aici.',
    errFull: 'E plin.', errMany: 'Ești conectat din prea multe locuri.', errName: 'Alege un nume.', errAuth: 'Semnătura nu e pentru acest server.',
    errRate: 'Prea multe mesaje, mai încet.', errInvite: 'Invitația nu mai e valabilă.', errMuted: 'Nu poți scrie aici acum.',
    errRole: 'Nu ai voie.', errGone: 'Nu mai există.', errServer: 'Eroare pe server.', errWs: 'Nu pot ajunge la server.',
    temp: 'Canal temporar', evSoon: '📅 {title}: începe în curând', evStart: '📅 {title}: începe acum', leiGift: '💰 {by}: {amount} Lei',
  },
  en: {
    off: 'Disconnected', connecting: 'Connecting…', connected: 'Connected', reconnecting: 'Reconnecting…', leave: 'Leave',
    welcome: 'Join the server', name: 'Your name', password: 'Server password (if any)', connect: 'Connect',
    idNote: 'Your identity is a key made in this browser; there is no account.', channels: 'Channels', chat: 'Chat',
    serverChat: 'Server', older: 'Older messages', write: 'Write a message…', send: 'Send',
    ptt: 'Hold to talk', openMic: 'Open mic', mute: 'Mute', deafen: 'Deafen',
    footer: 'The web client: voice and chat. Streams, games, files and private messages are in the app:',
    inviteNote: 'You have an invite ({code}) to this server.', chPass: 'Channel password',
    noMic: 'No microphone available: listening only.', micDenied: 'No microphone (access refused): listening only.',
    joined: 'You joined {ch}', moved: '{by} moved you to {ch}', kicked: '{by} kicked you', banned: '{by} banned you',
    back: 'Back on the server', lost: 'Connection lost; retrying…', deleted: '(message deleted)', edited: '(edited)',
    volume: 'Volume: {name}', you: 'you', voiceOn: 'Voice connected', voiceOff: 'Voice dropped', listenOnly: 'listening only',
    errPassword: 'Wrong server password.', errChPassword: 'Wrong channel password.', errBanned: 'You are banned here.',
    errFull: "It's full.", errMany: "You're connected from too many places.", errName: 'Choose a name.', errAuth: 'The signature is not for this server.',
    errRate: 'Too many messages, slow down.', errInvite: 'This invite is no longer valid.', errMuted: "You can't write here for now.",
    errRole: "You aren't allowed to.", errGone: "It's gone.", errServer: 'Server error.', errWs: "Can't reach the server.",
    temp: 'Temporary channel', evSoon: '📅 {title}: starting soon', evStart: '📅 {title}: starting now', leiGift: '💰 {by}: {amount} Lei',
  },
};
let lang = store.get('sv.lang', (navigator.language || 'ro').toLowerCase().startsWith('ro') ? 'ro' : 'en');
if (lang !== 'ro' && lang !== 'en') lang = 'en';
const t = (k, vars) => (T[lang][k] ?? T.en[k] ?? k).replace(/\{(\w+)\}/g, (_, v) => vars?.[v] ?? '');
function applyLang() {
  document.documentElement.lang = lang;
  $('lang').textContent = lang === 'ro' ? 'EN' : 'RO';
  for (const el of document.querySelectorAll('[data-i18n]')) el.textContent = t(el.dataset.i18n);
  for (const el of document.querySelectorAll('[data-i18n-ph]')) el.placeholder = t(el.dataset.i18nPh);
  setStatus(S.phase);
  if (S.phase === 'on') render();
}

// BBCode-light: [b], [i], [url=…]…[/url], [url]…[/url] and bare links; everything else is text.
function richText(s) {
  let h = esc(s);
  h = h.replace(/\[b\]([\s\S]*?)\[\/b\]/gi, '<b>$1</b>').replace(/\[i\]([\s\S]*?)\[\/i\]/gi, '<i>$1</i>');
  h = h.replace(/\[url=(https?:\/\/[^\]\s]+)\]([\s\S]*?)\[\/url\]/gi, (_, u, l) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${l}</a>`);
  h = h.replace(/\[url\](https?:\/\/[^\]\s]+)\[\/url\]/gi, (_, u) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`);
  h = h.replace(/(^|[^"=>])(https?:\/\/[^\s<]+[^\s<.,;:!?)\]])/g, (_, p, u) => `${p}<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`);
  return h;
}

// ====== Identity: a P-256 key in IndexedDB (the private key never leaves the browser) ======

function idb() {
  return new Promise((ok, no) => {
    const req = indexedDB.open('spectrevoice', 1);
    req.onupgradeneeded = () => req.result.createObjectStore('keys');
    req.onsuccess = () => ok(req.result);
    req.onerror = () => no(req.error);
  });
}
function idbGet(db, k) {
  return new Promise((ok, no) => {
    const r = db.transaction('keys').objectStore('keys').get(k);
    r.onsuccess = () => ok(r.result);
    r.onerror = () => no(r.error);
  });
}
function idbPut(db, k, v) {
  return new Promise((ok, no) => {
    const tx = db.transaction('keys', 'readwrite');
    tx.objectStore('keys').put(v, k);
    tx.oncomplete = ok;
    tx.onerror = () => no(tx.error);
  });
}
async function loadIdentity() {
  const db = await idb();
  let pair = await idbGet(db, 'id');
  if (!pair) {
    pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
    await idbPut(db, 'id', pair);
  }
  const spki = new Uint8Array(await crypto.subtle.exportKey('spki', pair.publicKey));
  const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', spki));
  return { priv: pair.privateKey, pub: b64.enc(spki), uid: b64.url(hash.slice(0, 15)) };
}
// WebCrypto gives r‖s; the server wants ASN.1 DER.
function derSig(raw) {
  const int = (b) => {
    let i = 0;
    while (i < b.length - 1 && b[i] === 0) i++;
    b = b.slice(i);
    if (b[0] & 0x80) b = new Uint8Array([0, ...b]);
    return [0x02, b.length, ...b];
  };
  const body = [...int(raw.slice(0, 32)), ...int(raw.slice(32))];
  return new Uint8Array([0x30, body.length, ...body]);
}
async function signChallenge(id, nonceB64) {
  const host = location.hostname.replace(/^\[|\]$/g, '').toLowerCase();
  const prefix = new TextEncoder().encode('spectrevoice-auth-v2:' + host + '\n');
  const nonce = b64.dec(nonceB64);
  const data = new Uint8Array(prefix.length + nonce.length);
  data.set(prefix);
  data.set(nonce, prefix.length);
  const raw = new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, id.priv, data));
  return b64.enc(derSig(raw));
}

// ====== State ======

const S = {
  id: null, ws: null, phase: 'off', sid: 0, uid: '', name: '', pass: '', invite: '', wantCh: 0,
  channels: new Map(), members: new Map(), chat: new Map(), cur: '', ice: [], retry: 0, timer: null, wasOn: false, lastCh: 0,
  mic: false, snd: false, open: false, pressing: false, dev: '', version: '', server: '', volumes: {},
};
try { S.volumes = JSON.parse(store.get('sv.volumes', '{}')); } catch { S.volumes = {}; }
S.dev = store.get('sv.dev', '');
if (!S.dev) {
  S.dev = b64.url(crypto.getRandomValues(new Uint8Array(12)));
  store.set('sv.dev', S.dev);
}
const me = () => S.members.get(S.sid);
// The server chat: everyone on the server
const SERVER_CHAT = 'g:1';

function setStatus(phase) {
  S.phase = phase;
  const el = $('status');
  el.className = 'status ' + ({ on: 'ok', connecting: 'warn', reconnecting: 'warn' }[phase] || '');
  el.textContent = t({ on: 'connected', connecting: 'connecting', reconnecting: 'reconnecting' }[phase] || 'off');
}
function send(m) {
  if (S.ws && S.ws.readyState === 1) S.ws.send(JSON.stringify(m));
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
    voiceStop();
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
  voiceStop();
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
function errText(m) {
  const w = m.why;
  if (w === 'password') return m.op === 'hello' ? t('errPassword') : t('errChPassword');
  const k = { banned: 'errBanned', full: 'errFull', many: 'errMany', name: 'errName', auth: 'errAuth', rate: 'errRate', invite: 'errInvite', muted: 'errMuted', role: 'errRole', gone: 'errGone', server: 'errServer' }[w];
  return k ? t(k) : (m.message || w || 'error');
}

async function handle(m) {
  switch (m.t) {
    case 'challenge': {
      S.version = m.version || '';
      const sig = await signChallenge(S.id, m.n);
      send({ t: 'hello', v: 2, pub: S.id.pub, sig, name: S.name, client: 'web/' + (S.version || 'dev'), password: S.pass, invite: S.invite || undefined, dev: S.dev });
      break;
    }
    case 'welcome': welcome(m); break;
    case 'error':
      if (m.op === 'hello') { failed(errText(m)); if (m.why === 'password') $('password').focus(); break; }
      if (m.op === 'history') break;
      if (m.op === 'say') { const c = S.chat.get(S.cur); if (c) { c.items = c.items.filter((x) => !x.pending); renderChat(); } }
      toast(errText(m), true);
      break;
    case 'server-info':
      S.server = m.server?.name || S.server;
      $('serverName').textContent = S.server || 'Spectre Voice';
      $('commName').textContent = S.server;
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
    case 'msg': message(m); break;
    case 'history': history(m); break;
    case 'moved': toast(t('moved', { by: m.by, ch: S.channels.get(m.ch)?.name || '?' })); break;
    case 'kicked':
      S.phase = 'off';
      failed(t(m.ban ? 'banned' : 'kicked', { by: m.by }) + (m.why ? ': ' + m.why : ''));
      break;
    case 'role': render(); break;
    case 'rtc': case 'ice': case 'rtc-reset': voiceSignal(m); break;
    case 'poke': toast('👉 ' + m.name + ': ' + (m.text || '')); break;
    case 'announce': toast('📢 ' + m.text); break;
    case 'event-notice': toast(t(m.kind === 'soon' ? 'evSoon' : 'evStart', { title: m.e?.title || '' })); break;
    case 'lei-gift': toast(t('leiGift', { by: m.byName || '', amount: (m.amount > 0 ? '+' : '') + m.amount }) + (m.reason ? ' · ' + m.reason : '')); break;
  }
}

function welcome(m) {
  const back = S.wasOn;
  const last = S.lastCh;
  S.sid = m.sid;
  S.uid = m.uid;
  S.retry = 0;
  S.server = m.server?.name || '';
  S.ice = m.server?.ice || [];
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
  if (back) {
    toast(t('back'));
    const c = S.channels.get(last);
    if (c && !c.locked && me()?.ch !== last) send({ t: 'join', ch: last });
  }
  S.wasOn = true;
  // The invite was used in the hello (its channel too): not again on a reconnect
  if (S.invite) S.invite = '';
  else if (S.wantCh) { joinChannel(S.wantCh); S.wantCh = 0; }
}

// entered: the server's channels and people from a welcome
function entered(m) {
  S.channels = new Map((m.channels || []).map((c) => [c.id, c]));
  S.members = new Map((m.members || []).map((x) => [x.sid, x]));
  S.chat = new Map();
  const ch = me()?.ch || 0;
  S.lastCh = ch;
  S.cur = ch ? 'c:' + ch : SERVER_CHAT;
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
    loadChat();
  }
  render();
}

// ====== Channels and members ======

function joinChannel(id) {
  const c = S.channels.get(id);
  if (!c || c.kind) return;
  if (c.locked && me()?.ch !== id) {
    const p = prompt(t('chPass'));
    if (p === null) return;
    send({ t: 'join', ch: id, password: p });
    return;
  }
  send({ t: 'join', ch: id });
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
        const sp = document.createElement('div');
        sp.className = 'spacer';
        sp.textContent = c.name;
        into.appendChild(sp);
      } else {
        const el = document.createElement('div');
        el.className = 'ch' + (c.kind === 'text' ? ' text' : '') + (c.locked ? ' locked' : '') + (c.id === mine ? ' mine' : '') + (S.cur === 'c:' + c.id ? ' cur' : '');
        el.textContent = (c.temp ? '⏳ ' : '') + (c.icon ? c.icon + ' ' : '') + c.name;
        el.title = c.topic || (c.temp ? t('temp') : '');
        if (!c.kind) {
          const n = (byCh.get(c.id) || []).length;
          if (n) {
            const cnt = document.createElement('span');
            cnt.className = 'cnt';
            cnt.textContent = n + (c.max ? '/' + c.max : '');
            el.appendChild(cnt);
          }
          el.onclick = () => { joinChannel(c.id); S.cur = 'c:' + c.id; loadChat(); render(); };
        } else {
          el.onclick = () => { S.cur = 'c:' + c.id; loadChat(); render(); showPane('chat'); };
        }
        into.appendChild(el);
        for (const mem of (byCh.get(c.id) || []).sort((a, b) => a.name.localeCompare(b.name))) into.appendChild(memberRow(mem));
      }
      if (kids.has(c.id) && depth < 10) {
        const box = document.createElement('div');
        box.className = 'kids';
        draw(c.id, box, depth + 1);
        into.appendChild(box);
      }
    }
  };
  draw(0, tree, 0);
  const cur = S.channels.get(+S.cur.slice(2));
  $('tabChannel').textContent = S.cur.startsWith('c:') && cur ? '#' + cur.name : (S.channels.get(mine)?.name ? '#' + S.channels.get(mine).name : t('chat'));
  $('tabChannel').classList.toggle('on', S.cur.startsWith('c:'));
  $('tabComm').classList.toggle('on', S.cur.startsWith('g:'));
  $('mute').classList.toggle('muted-on', S.mic);
  $('deafen').classList.toggle('muted-on', S.snd);
  $('ptt').classList.toggle('off', S.mic || !V.track);
}
function memberRow(mem) {
  const el = document.createElement('div');
  el.className = 'mem' + (mem.talk ? ' talking' : '') + (mem.sid === S.sid ? ' me' : '') + (mem.away ? ' away' : '');
  el.dataset.sid = mem.sid;
  const dot = document.createElement('span');
  dot.className = 'dot';
  el.appendChild(dot);
  el.appendChild(document.createTextNode(mem.name));
  const ic = document.createElement('span');
  ic.className = 'ic';
  ic.textContent = (mem.bot ? ' 🤖' : '') + (mem.mic ? ' 🎙️✕' : '') + (mem.snd ? ' 🔇' : '') + (mem.vmuted ? ' 🚫' : '') + (mem.away ? ' 💤' : '');
  el.appendChild(ic);
  if (mem.client) {
    const cli = document.createElement('span');
    cli.className = 'cli';
    cli.textContent = mem.client.split('/')[0];
    el.appendChild(cli);
  }
  if (mem.sid !== S.sid && !mem.bot) el.onclick = (ev) => volumePop(mem, ev);
  return el;
}
function renderTalk(sid, on) {
  for (const el of document.querySelectorAll(`.mem[data-sid="${sid}"]`)) el.classList.toggle('talking', on);
}
// Per-person volume (kept per uid in this browser)
function volumePop(mem, ev) {
  const pop = $('volPop');
  $('volName').textContent = t('volume', { name: mem.name });
  const v = $('vol');
  v.value = Math.round((S.volumes[mem.uid] ?? 1) * 100);
  v.oninput = () => {
    S.volumes[mem.uid] = v.value / 100;
    store.set('sv.volumes', JSON.stringify(S.volumes));
    applyVolumes();
  };
  pop.classList.remove('hidden');
  const r = ev.currentTarget.getBoundingClientRect();
  pop.style.left = Math.min(r.left, innerWidth - 240) + 'px';
  pop.style.top = Math.min(r.bottom + 4, innerHeight - 90) + 'px';
  ev.stopPropagation();
}
document.addEventListener('click', (ev) => { if (!$('volPop').contains(ev.target)) $('volPop').classList.add('hidden'); });

// ====== Chat ======

function conv(to) {
  if (!S.chat.has(to)) S.chat.set(to, { items: [], more: false, loaded: false });
  return S.chat.get(to);
}
function loadChat() {
  if (!S.cur) { renderChat(); return; }
  const c = conv(S.cur);
  if (!c.loaded) send({ t: 'history', to: S.cur });
  renderChat();
}
function history(m) {
  const c = conv(m.to);
  c.loaded = true;
  c.more = !!m.more;
  const byID = new Map(c.items.filter((x) => !x.pending).map((x) => [x.id, x]));
  for (const x of m.items || []) byID.set(x.id, x);
  c.items = [...byID.values()].sort((a, b) => a.id - b.id).slice(-2000).concat(c.items.filter((x) => x.pending));
  if (m.to === S.cur) renderChat(!m.before);
}
function message(m) {
  const msg = m.m;
  const c = conv(msg.to);
  const i = c.items.findIndex((x) => x.id === msg.id || (m.n && x.n === m.n));
  if (i >= 0) c.items[i] = msg; else c.items.push(msg);
  if (msg.to === S.cur) renderChat(true);
  else if (msg.from !== S.uid && msg.to.startsWith('g:')) $('tabComm').classList.add('unread');
}
function renderChat(stick) {
  const list = $('msgList');
  const box = $('messages');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
  list.textContent = '';
  const c = S.cur ? conv(S.cur) : { items: [], more: false };
  $('older').classList.toggle('hidden', !c.more);
  for (const msg of c.items) {
    const el = document.createElement('div');
    el.className = 'msg' + (msg.del ? ' del' : '') + (msg.pending ? ' pending' : '');
    const who = document.createElement('span');
    who.className = 'who';
    who.textContent = msg.name;
    const when = document.createElement('span');
    when.className = 'when';
    when.textContent = new Date(msg.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    const text = document.createElement('div');
    text.className = 'text';
    if (msg.del) text.textContent = t('deleted');
    else text.innerHTML = richText(msg.text) + (msg.edit ? `<span class="edit">${esc(t('edited'))}</span>` : '');
    el.append(who, when, text);
    list.appendChild(el);
  }
  $('say').disabled = !S.cur;
  if (stick || atEnd) box.scrollTop = box.scrollHeight;
}
$('older').onclick = () => {
  const c = conv(S.cur);
  if (c.items.length) send({ t: 'history', to: S.cur, before: c.items[0].id });
};
$('sayForm').onsubmit = (ev) => {
  ev.preventDefault();
  const text = $('say').value.trim();
  if (!text || !S.cur) return;
  const n = 'w' + Date.now() + Math.random().toString(36).slice(2, 6);
  conv(S.cur).items.push({ id: Number.MAX_SAFE_INTEGER, n, to: S.cur, from: S.uid, name: S.name, text, at: Date.now(), pending: true });
  send({ t: 'say', to: S.cur, text, n });
  $('say').value = '';
  renderChat(true);
};
$('tabChannel').onclick = () => { const ch = me()?.ch; if (ch) { S.cur = 'c:' + ch; loadChat(); render(); } };
$('tabComm').onclick = () => { S.cur = SERVER_CHAT; $('tabComm').classList.remove('unread'); loadChat(); render(); };

// ====== Voice (WebRTC to the server's SFU) ======

const V = { pc: null, stream: null, track: null, audios: new Map(), early: [], gen: 0 };

async function voiceStart() {
  voiceStop();
  const gen = ++V.gen;
  if (!V.stream) {
    try {
      V.stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
      V.track = V.stream.getAudioTracks()[0];
      V.track.enabled = false;
    } catch (e) {
      toast(t(e?.name === 'NotAllowedError' ? 'micDenied' : 'noMic'), true);
      V.stream = null;
      V.track = null;
    }
  }
  if (gen !== V.gen || !S.ws) return;
  const pc = new RTCPeerConnection({ iceServers: S.ice.map((s) => ({ urls: s.urls, username: s.username, credential: s.credential })) });
  V.pc = pc;
  V.early = [];
  pc.onicecandidate = (ev) => {
    if (!ev.candidate || V.pc !== pc) return;
    send({ t: 'ice', c: ev.candidate.candidate, mid: ev.candidate.sdpMid, mline: ev.candidate.sdpMLineIndex });
  };
  pc.ontrack = (ev) => {
    if (ev.track.kind !== 'audio') return;
    // One stream per voice, named "s<sid>"
    const sid = +((ev.streams[0]?.id || '').replace(/^s/, ''));
    const old = V.audios.get(ev.transceiver);
    if (old) { old.el.pause(); old.el.srcObject = null; }
    const el = new Audio();
    el.autoplay = true;
    el.srcObject = ev.streams[0] || new MediaStream([ev.track]);
    V.audios.set(ev.transceiver, { el, sid });
    applyVolumes();
    el.play().catch(() => { /* a user gesture came first (the connect button) */ });
  };
  pc.onconnectionstatechange = () => {
    if (V.pc !== pc) return;
    document.body.dataset.rtc = pc.connectionState;
    if (pc.connectionState === 'connected') toast(t('voiceOn') + (V.track ? '' : ' (' + t('listenOnly') + ')'));
    if (pc.connectionState === 'failed') { toast(t('voiceOff'), true); voiceStart(); }
  };
  if (V.track) pc.addTrack(V.track, V.stream);
  else pc.addTransceiver('audio', { direction: 'recvonly' });
  const offer = await pc.createOffer();
  if (V.pc !== pc) return;
  await pc.setLocalDescription(offer);
  send({ t: 'rtc', type: 'offer', sdp: pc.localDescription.sdp });
  updatePTT();
  render();
}
function voiceStop() {
  if (V.pc) { V.pc.onconnectionstatechange = null; V.pc.close(); }
  V.pc = null;
  for (const { el } of V.audios.values()) { el.pause(); el.srcObject = null; }
  V.audios.clear();
  document.body.dataset.rtc = 'new';
}
async function voiceSignal(m) {
  const pc = V.pc;
  if (m.t === 'rtc-reset') { voiceStart(); return; }
  if (!pc) return;
  try {
    if (m.t === 'ice') {
      const c = { candidate: m.c, sdpMid: m.mid ?? null, sdpMLineIndex: m.mline ?? null };
      if (!pc.remoteDescription) V.early.push(c); else await pc.addIceCandidate(c);
      return;
    }
    if (m.type === 'answer') {
      if (pc.signalingState !== 'have-local-offer') return;
      await pc.setRemoteDescription({ type: 'answer', sdp: m.sdp });
    } else if (m.type === 'offer') {
      // Ours is out and unanswered: the server rolls back and offers again after its answer
      if (pc.signalingState !== 'stable') return;
      await pc.setRemoteDescription({ type: 'offer', sdp: m.sdp });
      const ans = await pc.createAnswer();
      await pc.setLocalDescription(ans);
      send({ t: 'rtc', type: 'answer', sdp: pc.localDescription.sdp });
    }
    for (const c of V.early.splice(0)) await pc.addIceCandidate(c).catch(() => {});
    pruneAudios();
  } catch (e) {
    console.warn('rtc', e);
  }
}
// Voices the server stopped sending: their players go
function pruneAudios() {
  for (const [tr, a] of V.audios) {
    if (tr.stopped || !tr.currentDirection || !tr.currentDirection.includes('recv')) {
      a.el.pause();
      a.el.srcObject = null;
      V.audios.delete(tr);
    }
  }
}
function applyVolumes() {
  for (const { el, sid } of V.audios.values()) {
    const uid = S.members.get(sid)?.uid;
    el.volume = Math.max(0, Math.min(1, S.volumes[uid] ?? 1));
    el.muted = S.snd;
  }
}
function updatePTT() {
  const live = !!V.track && !S.mic && (S.open || S.pressing);
  if (V.track) V.track.enabled = live;
  $('ptt').classList.toggle('live', live);
}
const ptt = $('ptt');
const press = (on) => (ev) => { if (ev.type.startsWith('pointer')) ev.preventDefault(); S.pressing = on; updatePTT(); };
ptt.onpointerdown = press(true);
ptt.onpointerup = press(false);
ptt.onpointerleave = press(false);
ptt.onpointercancel = press(false);
ptt.oncontextmenu = (ev) => ev.preventDefault();
document.addEventListener('keydown', (ev) => {
  if (ev.code !== 'Space' || ev.repeat || S.phase !== 'on') return;
  if (/^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName)) return;
  ev.preventDefault();
  S.pressing = true;
  updatePTT();
});
document.addEventListener('keyup', (ev) => {
  if (ev.code !== 'Space' || !S.pressing) return;
  S.pressing = false;
  updatePTT();
});
window.addEventListener('blur', () => { S.pressing = false; updatePTT(); });
$('openMic').onchange = () => { S.open = $('openMic').checked; store.set('sv.open', S.open ? '1' : ''); updatePTT(); };
$('mute').onclick = () => { S.mic = !S.mic; send({ t: 'state', mic: S.mic }); updatePTT(); render(); };
$('deafen').onclick = () => { S.snd = !S.snd; send({ t: 'state', snd: S.snd }); applyVolumes(); render(); };

// ====== Pages ======

function showPane(name) {
  for (const b of document.querySelectorAll('.mobile-tabs button')) b.classList.toggle('on', b.dataset.pane === name);
  for (const p of document.querySelectorAll('.pane')) p.classList.toggle('on', p.id === name);
}
for (const b of document.querySelectorAll('.mobile-tabs button')) b.onclick = () => showPane(b.dataset.pane);

$('lang').onclick = () => { lang = lang === 'ro' ? 'en' : 'ro'; store.set('sv.lang', lang); applyLang(); };
$('leave').onclick = () => { S.phase = 'off'; failed(''); };
$('form').onsubmit = async (ev) => {
  ev.preventDefault();
  S.name = $('name').value.trim();
  S.pass = $('password').value;
  if (!S.name) return;
  store.set('sv.name', S.name);
  $('go').disabled = true;
  $('connectError').classList.add('hidden');
  S.retry = 0;
  S.wasOn = false;
  connect();
};

async function init() {
  const q = new URLSearchParams(location.search);
  S.invite = (q.get('invite') || '').replace(/[^A-Za-z0-9]/g, '').slice(0, 16);
  S.wantCh = +(q.get('ch') || 0) || 0;
  if (q.get('name')) $('name').value = q.get('name').slice(0, 32);
  else $('name').value = store.get('sv.name', '');
  S.open = store.get('sv.open', '') === '1';
  $('openMic').checked = S.open;
  applyLang();
  if (S.invite) {
    $('inviteNote').textContent = t('inviteNote', { code: S.invite });
    $('inviteNote').classList.remove('hidden');
  }
  try {
    S.id = await loadIdentity();
    $('uidNote').textContent = 'ID: ' + S.id.uid;
  } catch (e) {
    $('connectError').textContent = 'WebCrypto/IndexedDB: ' + (e?.message || e);
    $('connectError').classList.remove('hidden');
    $('go').disabled = true;
  }
}
init();
