// Chat: a channel's chat ("c:<id>") and the server chat ("g:1") as the PC app shows them (MainScreen ChatPanel,
// ChatLine; ChatViews, ChatExtras): history from the server, lines grouped by person with their ghost, day
// separators, BBCode-light, pictures shared on this server in the line (only from this server: no other host learns
// who reads), files and pictures sent from here (/files), unread counters, notifications while the tab is hidden.
//
// The same messages as the apps, so web and app users see each other's:
//   reactions   bus {"t":"mreact","k":<name>|<hash of the text>,"e":emoji,"on":bool,"name"}  (Presence.kt MessageReactions)
//   replies     a first line "↪ Name: what they said" (ChatExtras.kt Replies)
//   edit/delete hub {"t":"edit","id","text"} / {"t":"unsay","id"}: own lines, delete also with delete_messages
//   typing      bus {"t":"typing","k":"c<id>"|"g1","name"}                                       (ChatExtras.kt Typing)
//   mentions    @Name, completed from the people here; a line naming you is lit (ChatViews.kt mentions)
// Other conversations (private ones) are another module's: messages whose "to" isn't c:/g: are left alone here.

import { $, S, el, esc, send, toast, on, emit, store, api, me, may, SERVER_CHAT, openDialog, chLabel } from './core.js';
import { t, getLang } from './i18n.js';
import { busSend } from './bus.js';
import { avatar, javaHash } from './avatar.js';
import { icon, setIcon } from './icons.js';

export const isOurs = (to) => typeof to === 'string' && (to.startsWith('c:') || to.startsWith('g:'));

// BBCode-light: [b], [i], [url=…]…[/url], [url]…[/url] and bare links; everything else is text.
export function richText(s) {
  let h = esc(s);
  h = h.replace(/\[b\]([\s\S]*?)\[\/b\]/gi, '<b>$1</b>').replace(/\[i\]([\s\S]*?)\[\/i\]/gi, '<i>$1</i>');
  h = h.replace(/\[url=(https?:\/\/[^\]\s]+)\]([\s\S]*?)\[\/url\]/gi, (_, u, l) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${l}</a>`);
  h = h.replace(/\[url\](https?:\/\/[^\]\s]+)\[\/url\]/gi, (_, u) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`);
  // Bare links only outside the links made above (inside one they'd break its href); no quote ever in a link
  h = h.split(/(<a\b[^>]*>[\s\S]*?<\/a>)/).map((part, i) => i % 2 ? part
    : part.replace(/(https?:\/\/[^\s<"']+[^\s<"'.,;:!?)\]])/g, (u) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`)).join('');
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
    out.push({ path: '/f/' + m[2] + '/' + encodeURIComponent(name), name, image: IMAGE.test(name), link: m[0] });
  }
  return out.slice(0, 4);
}

// ====== The PC's chat extras ======

export const QUICK = ['👍', '😂', '❤️', '🔥', '😮', '💀'];
export const EMOJI = [
  '😀', '😂', '🤣', '😅', '😊', '😍', '😎', '🤔', '😏', '😴', '😭', '😡', '🤯', '🥳', '😱', '🙄',
  '👍', '👎', '👌', '🙏', '👏', '💪', '🤝', '✌️', '🫡', '👀', '🔥', '💯', '❤️', '💀', '🎉', '✅',
  '❌', '⚡', '⭐', '🎮', '🕹️', '🏆', '🎯', '💣', '🍺', '🍕', '☕', '🎵', '📌', '🚀', '🐐', '🤡',
];

// A message's key for reactions: who wrote it and its text's hash (Kotlin's String.hashCode, unsigned, base 36)
export const reactKey = (name, text) => name + '|' + (javaHash(String(text)) >>> 0).toString(36);

// key → emoji → uids (by the uid the server stamped, never the name a message carries)
const reacts = new Map();
const sentNames = new Map();
function applyReact(k, e, uid, onv) {
  let forMsg = reacts.get(k);
  if (!forMsg) { if (!onv) return; forMsg = new Map(); reacts.set(k, forMsg); }
  // At most 20 different emoji on one message (a peer can't fill it with chips)
  if (onv && !forMsg.has(e) && forMsg.size >= 20) return;
  const who = forMsg.get(e) || new Set();
  if (onv) who.add(uid); else who.delete(uid);
  if (who.size) forMsg.set(e, who); else forMsg.delete(e);
  if (!forMsg.size) reacts.delete(k);
  if (reacts.size > 500) for (const old of [...reacts.keys()].slice(0, 100)) reacts.delete(old);
}
function nameOf(uid) {
  if (uid === S.uid) return me()?.name || S.name;
  for (const m of S.members.values()) if (m.uid === uid) return m.name;
  return sentNames.get(uid) || '?';
}
export function toggleReact(k, e) {
  if (!S.uid) return;
  const onv = !reacts.get(k)?.get(e)?.has(S.uid);
  if (!busSend({ t: 'mreact', k, e, on: onv, name: me()?.name || S.name })) { toast(t('busDown'), true); return; }
  applyReact(k, e, S.uid, onv);
  renderSoon();
}
on('bus:mreact', (m) => {
  const from = typeof m.from === 'string' ? m.from : '';
  if (!from || typeof m.k !== 'string' || typeof m.e !== 'string') return;
  if (typeof m.name === 'string' && sentNames.size < 2000) sentNames.set(from, m.name.slice(0, 40));
  applyReact(m.k.slice(0, 80), m.e.slice(0, 8), from, m.on === true);
  renderSoon();
});

// Replies: the first line quotes who and what (every client shows it, messages are plain text)
let replyTo = null; // {name, text}
export function wrapReply(text) {
  const r = replyTo;
  if (!r) return text;
  setReply(null);
  const flat = r.text.replace(/\n/g, ' ');
  const snippet = flat.length > 80 ? flat.slice(0, 80) + '…' : flat;
  return '↪ ' + r.name + ': ' + snippet + '\n' + text;
}
export function splitReply(text) {
  if (!text.startsWith('↪ ')) return null;
  const end = text.indexOf('\n');
  if (end < 0) return null;
  const q = text.substring(2, end);
  const cut = q.indexOf(': ');
  if (cut <= 0) return null;
  return { who: q.slice(0, cut), what: q.slice(cut + 2), rest: text.substring(end + 1).replace(/^[\n ]+/, '') };
}
function setReply(r) {
  replyTo = r;
  const bar = $('replyBar');
  bar.textContent = '';
  bar.classList.toggle('hidden', !r);
  if (!r) return;
  bar.append(icon('reply'), el('span', { class: 'rwho', text: t('replyingTo', { name: r.name }) }), el('span', { class: 'rwhat', text: r.text }),
    el('button', { class: 'iconbtn sm ghost', type: 'button', title: t('cancel'), attrs: { 'aria-label': t('cancel') }, onclick: () => setReply(null) }, icon('close')));
  $('say').focus();
}

// @mentions
const reEsc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
export const mentions = (text, name) => !!name && new RegExp('(^|[^\\p{L}\\p{N}_])@' + reEsc(name) + '([^\\p{L}\\p{N}_]|$)', 'iu').test(text);

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
  if (S.cur !== to) { setReply(null); editing = 0; editDraft = null; }
  S.cur = to;
  S.unread.delete(to);
  setView('chat');
  loadChat();
  updateUnread();
  renderTyping();
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
  if (fresh) spoke(msg.to, msg.from);
  if (msg.to === S.cur) renderChat(fresh && mine);
  if (fresh && !mine && !msg.del) {
    if (msg.to !== S.cur || document.hidden) {
      if (msg.to !== S.cur) S.unread.set(msg.to, (S.unread.get(msg.to) || 0) + 1);
      notify(msg.name, msg.text, msg.to);
    }
    updateUnread();
  }
}

// ====== Drawing the chat ======

const timeOf = (at) => new Date(at).toLocaleTimeString(getLang() || [], { hour: '2-digit', minute: '2-digit' });
const dayOf = (at) => { const d = new Date(at); return d.getFullYear() * 10000 + d.getMonth() * 100 + d.getDate(); };
function dayLabel(at) {
  const now = new Date();
  const today = dayOf(now);
  const y = new Date(now); y.setDate(now.getDate() - 1);
  const d = dayOf(at);
  if (d === today) return t('today');
  if (d === dayOf(y)) return t('yesterday');
  const s = new Date(at).toLocaleDateString(getLang() || [], { weekday: 'long', day: 'numeric', month: 'long' });
  return s.charAt(0).toUpperCase() + s.slice(1);
}
// Grouped: the same person within a few minutes, the same day
const follows = (prev, e) => prev && prev.from === e.from && e.at - prev.at < 5 * 60000 && dayOf(prev.at) === dayOf(e.at);

let editing = 0; // the id of the line being edited here
let editDraft = null; // what's written in it so far (the chat may be drawn again meanwhile)
let queued = false;
function renderSoon() {
  if (queued) return;
  queued = true;
  requestAnimationFrame(() => { queued = false; renderChat(); });
}

export function renderChat(stick) {
  const list = $('msgList');
  const box = $('messages');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
  const keep = box.scrollTop;
  const sel = list.querySelector('.msg.sel')?.dataset.id;
  list.textContent = '';
  const c = S.cur ? conv(S.cur) : { items: [], more: false };
  $('older').classList.toggle('hidden', !c.more);
  const items = c.items.filter((x) => !x.del);
  if (!items.length && c.loaded) {
    list.append(el('div', { class: 'empty' }, el('div', { class: 'emptyghost' }, avatar('', 'spectre', 56)),
      el('div', { class: 'emptyt', text: t('noMessages') }), el('div', { class: 'muted small', text: t('noMessagesHint') })));
  }
  const myName = me()?.name || S.name;
  let prev = null;
  for (const msg of items) {
    if (!prev || dayOf(prev.at) !== dayOf(msg.at)) list.append(el('div', { class: 'day' }, el('span', { text: dayLabel(msg.at) })));
    list.append(line(msg, follows(prev, msg), myName, box, stick || atEnd));
    prev = msg;
  }
  if (sel) list.querySelector(`.msg[data-id="${sel}"]`)?.classList.add('sel');
  $('say').disabled = !S.cur;
  const where = S.cur.startsWith('c:') ? chLabel(S.channels.get(+S.cur.slice(2))) : t('serverChat');
  $('say').placeholder = t('messageTo', { where });
  if (stick || atEnd) box.scrollTop = box.scrollHeight; else box.scrollTop = keep;
}

function line(msg, grouped, myName, box, follow) {
  const own = msg.from === S.uid;
  const reply = splitReply(msg.text || '');
  const body = reply ? reply.rest : (msg.text || '');
  const files = sharedFiles(body);
  const onlyFile = files.length === 1 && body.trim().replace(files[0].link, '').replace(/[📎🎤\s]/gu, '') === '';
  const k = reactKey(msg.name, msg.text || '');
  const row = el('div', {
    class: 'msg' + (own ? ' own' : '') + (msg.pending ? ' pending' : '') + (grouped ? ' grouped' : '') + (!own && mentions(body, myName) ? ' mention' : ''),
    dataset: { id: msg.id, k },
  });
  const gut = el('div', { class: 'mgut' });
  if (grouped) gut.append(el('span', { class: 'gtime', text: timeOf(msg.at) }));
  else gut.append(avatar(msg.name, msg.from, 30));
  const main = el('div', { class: 'mbody' });
  if (!grouped) {
    main.append(el('div', { class: 'mhead' },
      el('span', { class: 'who', text: msg.name }),
      el('span', { class: 'when', text: timeOf(msg.at) }),
      msg.edit ? el('span', { class: 'edit', text: t('edited') }) : null));
  } else if (msg.edit) {
    main.append(el('span', { class: 'edit floatedit', text: t('edited') }));
  }
  if (reply) main.append(el('div', { class: 'quote' }, el('span', { class: 'qwho', text: '↪ ' + reply.who }), el('span', { class: 'qwhat', text: reply.what })));
  if (editing === msg.id) {
    main.append(editBox(msg, reply));
  } else {
    const text = el('div', { class: 'text' + (onlyFile ? ' onlyfile' : '') });
    if (!onlyFile) text.innerHTML = richText(body);
    main.append(text);
  }
  for (const f of files) {
    if (f.image) {
      const img = el('img', { class: 'pic', src: f.path, alt: f.name, loading: 'lazy', decoding: 'async' });
      img.onload = () => { if (follow) box.scrollTop = box.scrollHeight; };
      main.append(el('div', { class: 'picwrap' },
        el('a', { class: 'picbox', href: f.path, target: '_blank', rel: 'noopener noreferrer' }, img),
        el('div', { class: 'picname' }, el('span', { class: 'fname', text: f.name }),
          el('a', { class: 'picdl', href: f.path, download: f.name, rel: 'noopener noreferrer', title: t('download'), attrs: { 'aria-label': t('download') } }, icon('download')))));
    } else {
      main.append(el('a', { class: 'filecard', href: f.path, download: f.name, rel: 'noopener noreferrer' },
        el('span', { class: 'ficon' }, icon('file')),
        el('span', { class: 'fmeta' }, el('span', { class: 'fname', text: f.name }), el('span', { class: 'fsub', text: t('sharedFile') })),
        el('span', { class: 'fdl', title: t('download') }, icon('download'))));
    }
  }
  const rs = reacts.get(k);
  if (rs?.size) {
    const chips = el('div', { class: 'reacts' });
    for (const [e, who] of rs) {
      const mine = who.has(S.uid);
      chips.append(el('button', {
        class: 'react' + (mine ? ' mine' : ''), type: 'button', title: [...who].map(nameOf).join(', '),
        attrs: { 'aria-pressed': mine ? 'true' : 'false' }, onclick: (ev) => { ev.stopPropagation(); toggleReact(k, e); },
      }, el('span', { class: 'remo', text: e }), el('span', { class: 'rn', text: String(who.size) })));
    }
    main.append(chips);
  }
  row.append(gut, main);
  if (!msg.pending) row.append(tools(msg, own, k, reply, body));
  // A tap (phones: no pointing) shows the line's tools
  row.onclick = (ev) => {
    if (ev.target.closest('a, button, input, .mtools')) return;
    const was = row.classList.contains('sel');
    for (const o of document.querySelectorAll('#msgList .msg.sel')) o.classList.remove('sel');
    if (!was) row.classList.add('sel');
  };
  return row;
}

// Pointing at a line: quick reactions, more emoji, reply; edit and delete one's own (delete also for moderators)
function tools(msg, own, k, reply, body) {
  const bar = el('div', { class: 'mtools' });
  const here = reacts.get(k);
  for (const e of QUICK) {
    if (here?.has(e)) continue;
    bar.append(el('button', { class: 'tq', type: 'button', text: e, title: t('react'), onclick: (ev) => { ev.stopPropagation(); toggleReact(k, e); } }));
  }
  const tb = (name, title, fn, cls) => el('button', { class: 'tb' + (cls ? ' ' + cls : ''), type: 'button', title, attrs: { 'aria-label': title }, onclick: (ev) => { ev.stopPropagation(); fn(ev); } }, icon(name));
  bar.append(tb('emoji', t('react'), (ev) => emojiPop(ev.currentTarget, (e) => toggleReact(k, e))));
  bar.append(tb('reply', t('reply'), () => setReply({ name: msg.name, text: body.replace(/\n/g, ' ') }), 'treply'));
  if (own) bar.append(tb('edit', t('editMsg'), () => { editing = msg.id; editDraft = null; renderChat(); }, 'tedit'));
  if (own || may('delete_messages')) {
    bar.append(tb('trash', t('deleteMsg'), () => {
      if (own) { send({ t: 'unsay', id: msg.id }); return; }
      openDialog({
        title: t('deleteMsg'), body: el('p', { text: t('deleteOther', { name: msg.name }) }),
        actions: [{ text: t('cancel') }, { text: t('deleteMsg'), danger: true, id: 'delGo', onclick: () => { send({ t: 'unsay', id: msg.id }); } }],
      });
    }, 'tdel'));
  }
  return bar;
}

// Editing in place: Enter saves, Escape leaves it; a reply keeps its quote
function editBox(msg, reply) {
  const head = reply ? msg.text.slice(0, msg.text.indexOf('\n') + 1) : '';
  const input = el('input', { class: 'editbox', maxLength: 4000, value: editDraft ?? (reply ? reply.rest : msg.text) });
  input.oninput = () => { editDraft = input.value; };
  const done = (save) => {
    const v = input.value.trim();
    editing = 0;
    editDraft = null;
    if (save && v && head + v !== msg.text) send({ t: 'edit', id: msg.id, text: head + v });
    renderChat();
    $('say').focus();
  };
  input.onkeydown = (ev) => {
    if (ev.key === 'Enter') { ev.preventDefault(); done(true); }
    if (ev.key === 'Escape') { ev.preventDefault(); done(false); }
  };
  setTimeout(() => { if (document.activeElement?.classList.contains('editbox') && document.activeElement !== input) return; input.focus(); input.setSelectionRange(input.value.length, input.value.length); }, 0);
  return el('div', { class: 'editwrap' }, input,
    el('div', { class: 'edithint' }, el('span', { text: t('editHint') }),
      el('button', { class: 'small', type: 'button', text: t('cancel'), onclick: () => done(false) }),
      el('button', { class: 'small primary', type: 'button', text: t('save'), onclick: () => done(true) })));
}

// ====== The emoji picker (writing, and reacting) ======

let popFor = null;
const pop = el('div', { id: 'emojiPop', class: 'emojipop hidden', attrs: { role: 'dialog', 'aria-label': 'Emoji' } },
  EMOJI.map((e) => el('button', { type: 'button', class: 'emo', text: e, onclick: (ev) => { ev.stopPropagation(); const f = popFor; closeEmoji(); if (f) f(e); } })));
document.body.append(pop);
export function emojiPop(anchor, pick) {
  if (!pop.classList.contains('hidden') && popFor === pick) { closeEmoji(); return; }
  popFor = pick;
  pop.classList.remove('hidden');
  const r = anchor.getBoundingClientRect();
  const w = pop.offsetWidth;
  const h = pop.offsetHeight;
  pop.style.left = Math.max(8, Math.min(r.left, innerWidth - w - 8)) + 'px';
  pop.style.top = (r.top - h - 8 > 8 ? r.top - h - 8 : Math.min(r.bottom + 8, innerHeight - h - 8)) + 'px';
}
function closeEmoji() { pop.classList.add('hidden'); popFor = null; }
document.addEventListener('click', (ev) => { if (!pop.contains(ev.target)) closeEmoji(); });
document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape' && !pop.classList.contains('hidden')) closeEmoji(); });
function insertAtCaret(input, s) {
  const a = input.selectionStart ?? input.value.length;
  const b = input.selectionEnd ?? a;
  input.value = input.value.slice(0, a) + s + input.value.slice(b);
  input.focus();
  input.setSelectionRange(a + s.length, a + s.length);
}

// ====== "… is typing" ======

const typingNow = new Map(); // "c5" / "g1" → uid → when
const typingKey = (to) => (to || '').replace(':', '');
let lastTyped = 0;
let lastTypedKey = '';
function typed() {
  if (!S.cur || !isOurs(S.cur)) return;
  const k = typingKey(S.cur);
  const now = Date.now();
  if (now - lastTyped < 3000 && k === lastTypedKey) return;
  lastTyped = now;
  lastTypedKey = k;
  busSend({ t: 'typing', k, name: me()?.name || S.name });
}
function spoke(to, uid) {
  const m = typingNow.get(typingKey(to));
  if (m?.delete(uid) && to === S.cur) renderTyping();
}
on('bus:typing', (m) => {
  if (m.to !== undefined || typeof m.k !== 'string' || !/^[cg]\d{1,12}$/.test(m.k) || typeof m.from !== 'string' || m.from === S.uid) return;
  // By the uid the server stamped (the name a message carries could be anyone's); its name from the members
  if (typeof m.name === 'string' && sentNames.size < 2000) sentNames.set(m.from, m.name.slice(0, 40));
  if (!typingNow.has(m.k)) typingNow.set(m.k, new Map());
  const who = typingNow.get(m.k);
  if (who.size >= 20 && !who.has(m.from)) return;
  who.set(m.from, Date.now());
  renderTyping();
});
let typingTimer = 0;
function renderTyping() {
  const box = $('typing');
  const m = typingNow.get(typingKey(S.cur));
  const now = Date.now();
  if (m) for (const [n, at] of m) if (now - at > 6000) m.delete(n);
  const who = m ? [...m.keys()].map(nameOf) : [];
  box.textContent = '';
  if (who.length) {
    box.append(el('span', { class: 'tdots' }, el('i'), el('i'), el('i')),
      el('span', { text: who.length === 1 ? t('typing1', { name: who[0] }) : who.length === 2 ? t('typing2', { a: who[0], b: who[1] }) : t('typingMany') }));
  }
  clearTimeout(typingTimer);
  if (who.length) typingTimer = setTimeout(renderTyping, 1000);
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
    // to: a conversation to open, or what to do on a click (a private chat: dm.js)
    const n = new Notification(title, { body: String(body || '').slice(0, 200), tag: typeof to === 'string' ? to : 'sv-dm', silent: false });
    n.onclick = () => { window.focus(); if (typeof to === 'function') to(); else if (to) openConv(to); n.close(); };
  } catch { /* some browsers only from a service worker */ }
}
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && S.cur && S.unread.has(S.cur)) { S.unread.delete(S.cur); updateUnread(); }
});

// ====== Writing: text, @mentions, emoji, files and pictures ======

$('older').onclick = () => {
  const c = conv(S.cur);
  if (c.items.length) send({ t: 'history', to: S.cur, before: c.items[0].id });
};
export function say(text) {
  if (!text || !S.cur) return;
  const n = 'w' + Date.now() + Math.random().toString(36).slice(2, 6);
  conv(S.cur).items.push({ id: Number.MAX_SAFE_INTEGER, n, to: S.cur, from: S.uid, name: me()?.name || S.name, text, at: Date.now(), pending: true });
  send({ t: 'say', to: S.cur, text, n });
  renderChat(true);
}
$('sayForm').onsubmit = (ev) => {
  ev.preventDefault();
  const text = $('say').value.trim();
  if (!text) return;
  $('say').value = '';
  showMentions();
  lastTyped = 0; // what's written next is told again (the others stopped showing us typing when this came)
  say(wrapReply(text));
};

// The word being written, when it starts with @: the people here whose name starts with it
let names = [];
function showMentions() {
  const input = $('say');
  const before = input.value.slice(0, input.selectionStart ?? input.value.length);
  const word = before.slice(before.lastIndexOf(' ') + 1);
  names = [];
  if (word.startsWith('@') && word.length >= 2) {
    const want = word.slice(1).toLowerCase();
    const seen = new Set();
    for (const m of S.members.values()) {
      if (m.sid === S.sid || seen.has(m.name) || !m.name.toLowerCase().startsWith(want)) continue;
      seen.add(m.name);
      names.push(m.name);
      if (names.length >= 6) break;
    }
  }
  const box = $('mentions');
  box.textContent = '';
  box.classList.toggle('hidden', !names.length);
  for (const n of names) box.append(el('button', { class: 'mchip', type: 'button', text: '@' + n, onclick: () => complete(n) }));
}
function complete(name) {
  const input = $('say');
  const at = input.selectionStart ?? input.value.length;
  const before = input.value.slice(0, at);
  const cut = before.lastIndexOf(' ') + 1;
  input.value = before.slice(0, cut) + '@' + name + ' ' + input.value.slice(at);
  const pos = cut + name.length + 2;
  input.focus();
  input.setSelectionRange(pos, pos);
  showMentions();
}
$('say').addEventListener('input', () => { if ($('say').value) typed(); showMentions(); });
$('say').addEventListener('keydown', (ev) => {
  if (ev.key === 'Tab' && names.length) { ev.preventDefault(); complete(names[0]); return; }
  if (ev.key === 'Escape' && replyTo) { ev.preventDefault(); setReply(null); return; }
  // Up in an empty line: edit your last one here
  if (ev.key === 'ArrowUp' && !$('say').value && S.cur) {
    const last = [...conv(S.cur).items].reverse().find((x) => x.from === S.uid && !x.del && !x.pending);
    if (last) { ev.preventDefault(); editing = last.id; editDraft = null; renderChat(); }
  }
});
$('emojiBtn').onclick = (ev) => { ev.stopPropagation(); emojiPop($('emojiBtn'), (e) => insertAtCaret($('say'), e)); };
setIcon($('emojiBtn'), 'emoji');
setIcon($('attach'), 'attach');
setIcon($('sendBtn'), 'send');

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
// Files dropped on the chat are shared in it
$('chat').addEventListener('dragover', (ev) => { if (ev.dataTransfer?.types?.includes('Files') && ($('chat').dataset.view || 'chat') === 'chat') { ev.preventDefault(); $('chat').classList.add('drop'); } });
$('chat').addEventListener('dragleave', (ev) => { if (!$('chat').contains(ev.relatedTarget)) $('chat').classList.remove('drop'); });
$('chat').addEventListener('drop', (ev) => {
  $('chat').classList.remove('drop');
  if (($('chat').dataset.view || 'chat') !== 'chat' || !ev.dataTransfer?.files?.length) return;
  ev.preventDefault();
  for (const f of ev.dataTransfer.files) uploadChatFile(f);
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
// A colour or a picture changed, someone came or went (names on reactions): the lines again
on('bus:kept', ({ kind }) => { if (!kind || kind === 'profiles') renderSoon(); });
