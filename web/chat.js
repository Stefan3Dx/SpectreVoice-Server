// Chat: a channel's chat ("c:<id>") and the server chat ("g:1"), history from the server, BBCode-light, pictures
// shared on this server shown in the line (only from this server: no other host learns who reads), files and
// pictures sent from here (/files), unread counters, and notifications while the tab is hidden (opt-in).
// Other conversations (private ones) are another module's: messages whose "to" isn't c:/g: are left alone here.

import { $, S, el, esc, send, toast, on, emit, store, api, me, SERVER_CHAT, fmtSize } from './core.js';
import { t } from './i18n.js';

export const isOurs = (to) => typeof to === 'string' && (to.startsWith('c:') || to.startsWith('g:'));

// BBCode-light: [b], [i], [url=…]…[/url], [url]…[/url] and bare links; everything else is text.
export function richText(s) {
  let h = esc(s);
  h = h.replace(/\[b\]([\s\S]*?)\[\/b\]/gi, '<b>$1</b>').replace(/\[i\]([\s\S]*?)\[\/i\]/gi, '<i>$1</i>');
  h = h.replace(/\[url=(https?:\/\/[^\]\s]+)\]([\s\S]*?)\[\/url\]/gi, (_, u, l) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${l}</a>`);
  h = h.replace(/\[url\](https?:\/\/[^\]\s]+)\[\/url\]/gi, (_, u) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`);
  h = h.replace(/(^|[^"=>])(https?:\/\/[^\s<]+[^\s<.,;:!?)\]])/g, (_, p, u) => `${p}<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`);
  return h;
}

// Files shared through this server: https://<this host>/f/<32 hex>/<name>. Another host's link stays a plain link.
const FILE_LINK = /https?:\/\/([^/\s\]]+)\/f\/([0-9a-f]{32})\/([^/\s?#\]]+)/g;
const IMAGE = /\.(png|jpe?g|gif|webp|bmp)$/i;
export function sharedFiles(text) {
  const out = [];
  for (const m of String(text || '').matchAll(FILE_LINK)) {
    if (m[1].toLowerCase() !== location.host.toLowerCase()) continue;
    let name = m[3];
    try { name = decodeURIComponent(m[3]); } catch { /* as it is */ }
    if (/[/\\]/.test(name)) continue;
    out.push({ path: '/f/' + m[2] + '/' + encodeURIComponent(name), name, image: IMAGE.test(name) });
  }
  return out.slice(0, 4);
}

// ====== Conversations ======

export function conv(to) {
  if (!S.chat.has(to)) S.chat.set(to, { items: [], more: false, loaded: false });
  return S.chat.get(to);
}
export function loadChat() {
  if (!S.cur) { renderChat(); return; }
  const c = conv(S.cur);
  if (!c.loaded && isOurs(S.cur)) send({ t: 'history', to: S.cur });
  renderChat(true);
}
export function openConv(to) {
  S.cur = to;
  S.unread.delete(to);
  setView('chat');
  loadChat();
  updateUnread();
  emit('app:conv', { to });
  emit('app:dirty');
}
// The middle column shows the chat or another module's view (files): #chat[data-view]
export function setView(v) {
  $('chat').dataset.view = v;
  emit('app:view', { view: v });
}

function history(m) {
  if (!isOurs(m.to)) return;
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
  if (!msg || !isOurs(msg.to)) return;
  const c = conv(msg.to);
  const i = c.items.findIndex((x) => x.id === msg.id || (m.n && x.n === m.n));
  const fresh = i < 0;
  if (i >= 0) c.items[i] = msg; else c.items.push(msg);
  const mine = msg.from === S.uid;
  if (msg.to === S.cur) renderChat(true);
  if (fresh && !mine && !msg.del) {
    if (msg.to !== S.cur || document.hidden) {
      if (msg.to !== S.cur) S.unread.set(msg.to, (S.unread.get(msg.to) || 0) + 1);
      notify(msg.name, msg.text, msg.to);
    }
    updateUnread();
  }
}

export function renderChat(stick) {
  const list = $('msgList');
  const box = $('messages');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
  list.textContent = '';
  const c = S.cur ? conv(S.cur) : { items: [], more: false };
  $('older').classList.toggle('hidden', !c.more);
  if (!c.items.length && c.loaded) list.append(el('div', { class: 'empty muted', text: t('noMessages') }));
  for (const msg of c.items) {
    const text = el('div', { class: 'text' });
    if (msg.del) text.textContent = t('deleted');
    else text.innerHTML = richText(msg.text) + (msg.edit ? `<span class="edit">${esc(t('edited'))}</span>` : '');
    const row = el('div', { class: 'msg' + (msg.del ? ' del' : '') + (msg.pending ? ' pending' : '') + (msg.from === S.uid ? ' own' : ''), dataset: { id: msg.id } },
      el('span', { class: 'who', text: msg.name }),
      el('span', { class: 'when', text: new Date(msg.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) }),
      text);
    if (!msg.del) {
      for (const f of sharedFiles(msg.text)) {
        if (f.image) {
          const img = el('img', { class: 'pic', src: f.path, alt: f.name, loading: 'lazy', decoding: 'async' });
          img.onload = () => { if (stick || atEnd) box.scrollTop = box.scrollHeight; };
          row.append(el('a', { class: 'picbox', href: f.path, target: '_blank', rel: 'noopener noreferrer' }, img));
        } else {
          row.append(el('a', { class: 'filecard', href: f.path, download: f.name, rel: 'noopener noreferrer' }, el('span', { text: '📎' }), el('span', { class: 'fname', text: f.name }), el('span', { class: 'muted small', text: t('download') })));
        }
      }
    }
    list.appendChild(row);
  }
  $('say').disabled = !S.cur;
  if (stick || atEnd) box.scrollTop = box.scrollHeight;
}

// ====== Unread, notifications ======

export function updateUnread() {
  let total = 0;
  for (const [to, n] of S.unread) if (isOurs(to)) total += n;
  const comm = S.unread.get(SERVER_CHAT) || 0;
  const tab = $('tabComm');
  tab.classList.toggle('unread', comm > 0);
  tab.dataset.count = comm > 99 ? '99+' : comm || '';
  const mob = document.querySelector('.mobile-tabs [data-pane="chat"]');
  if (mob) mob.dataset.count = total > 99 ? '99+' : total || '';
  document.title = (total ? '(' + total + ') ' : '') + (S.server || 'Spectre Voice');
  emit('app:unread', { total });
}

export const notifyPrefs = { on: store.get('sv.notify', '') === '1' };
export async function setNotify(want) {
  if (want && 'Notification' in window && Notification.permission !== 'granted') {
    const p = await Notification.requestPermission().catch(() => 'denied');
    if (p !== 'granted') { toast(t('notifyDenied'), true); want = false; }
  }
  notifyPrefs.on = !!want && 'Notification' in window;
  store.set('sv.notify', notifyPrefs.on ? '1' : '');
  return notifyPrefs.on;
}
export function notify(title, body, to) {
  if (!notifyPrefs.on || !document.hidden || !('Notification' in window) || Notification.permission !== 'granted') return;
  try {
    const n = new Notification(title, { body: String(body || '').slice(0, 200), tag: to || 'sv', silent: false });
    n.onclick = () => { window.focus(); if (to) openConv(to); n.close(); };
  } catch { /* some browsers only from a service worker */ }
}
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && S.cur && S.unread.has(S.cur)) { S.unread.delete(S.cur); updateUnread(); }
});

// ====== Sending: text, files and pictures ======

$('older').onclick = () => {
  const c = conv(S.cur);
  if (c.items.length) send({ t: 'history', to: S.cur, before: c.items[0].id });
};
export function say(text) {
  if (!text || !S.cur) return;
  const n = 'w' + Date.now() + Math.random().toString(36).slice(2, 6);
  conv(S.cur).items.push({ id: Number.MAX_SAFE_INTEGER, n, to: S.cur, from: S.uid, name: S.name, text, at: Date.now(), pending: true });
  send({ t: 'say', to: S.cur, text, n });
  renderChat(true);
}
$('sayForm').onsubmit = (ev) => {
  ev.preventDefault();
  const text = $('say').value.trim();
  if (!text) return;
  $('say').value = '';
  say(text);
};

// POST /files (the apps' way): the server keeps it and gives /f/<id>/<name>; the link goes in the chat
export function uploadChatFile(file) {
  const to = S.cur;
  if (!to || !file) return;
  const bar = el('div', { class: 'upload' }, el('span', { class: 'fname', text: file.name }), el('progress', { max: 1, value: 0 }));
  $('uploads').append(bar);
  const xhr = new XMLHttpRequest();
  xhr.open('POST', api('/files', { name: file.name, space: 'main' }));
  xhr.upload.onprogress = (e) => { if (e.lengthComputable) bar.querySelector('progress').value = e.loaded / e.total; };
  xhr.onload = () => {
    bar.remove();
    if (xhr.status === 200) {
      let path = '';
      try { path = JSON.parse(xhr.responseText).url || ''; } catch { /* below */ }
      if (!path) { toast(t('uploadFailed', { why: 'bad answer' }), true); return; }
      const enc = path.split('/').map((p) => { try { return encodeURIComponent(decodeURIComponent(p)); } catch { return encodeURIComponent(p); } }).join('/');
      const cur = S.cur;
      S.cur = to;
      say(location.origin + enc);
      S.cur = cur;
      if (cur !== to) renderChat();
      return;
    }
    const why = { 401: t('errAuth'), 404: t('filesOff'), 413: t('tooBig'), 507: t('spaceFull') }[xhr.status] || ('HTTP ' + xhr.status);
    toast(t('uploadFailed', { why }), true);
  };
  xhr.onerror = () => { bar.remove(); toast(t('uploadFailed', { why: t('errWs') }), true); };
  xhr.send(file);
}
$('attach').onclick = () => $('attachFile').click();
$('attachFile').onchange = () => {
  for (const f of $('attachFile').files) uploadChatFile(f);
  $('attachFile').value = '';
};
$('say').addEventListener('paste', (ev) => {
  const files = [...(ev.clipboardData?.files || [])];
  if (!files.length) return;
  ev.preventDefault();
  for (const f of files) uploadChatFile(f.name && f.name !== 'image.png' ? f : new File([f], 'picture-' + Date.now() + '.' + (f.type.split('/')[1] || 'png'), { type: f.type }));
});

$('tabChannel').onclick = () => { const ch = me()?.ch; const cur = S.cur.startsWith('c:') ? S.cur : (ch ? 'c:' + ch : ''); if (cur) openConv(cur); };
$('tabComm').onclick = () => openConv(SERVER_CHAT);

on('msg', message);
on('history', history);
// A refused line: the ones waiting to be sent come off (the toast says why)
on('error', (m) => {
  if (m.op !== 'say') return;
  const c = S.chat.get(S.cur);
  if (c) { c.items = c.items.filter((x) => !x.pending); renderChat(); }
});
on('app:welcome', () => { S.unread.clear(); updateUnread(); });
on('poke', (m) => notify('👉 ' + m.name, m.text || '', null));
