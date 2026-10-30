// Private messages, end-to-end encrypted (e2e.js; docs/E2E.md): this browser's device key (a
// non-extractable ECDH key in IndexedDB) published signed by the identity, each line sealed for every device of
// both people, opened here; the server keeps only ciphertext. A "Private" tab in the chat column, a "Message" button
// on a person's card, the lock on every line, the safety number to compare ("Verify"), a warning when someone's
// devices change, and no sending at all to someone whose app has no key yet (never plaintext instead).
// Decrypted lines live in memory only; search here is over what this page decrypted.
// With a server that has them (people.go): the Private tab lists the recent chats with their unread counts (the
// server's read markers, so the apps agree) and a preview decrypted here; reading moves the marker (dm-read);
// blocking (block / unblock) stops private messages both ways and hides the person.

import { $, S, el, send, toast, on, emit, store, api, openDialog, fmtSize, memberByUid } from './core.js';
import { t } from './i18n.js';
import { setView, notify } from './chat.js';
import { avatar } from './avatar.js';
import * as E from './e2e.js';


// ====== This browser's device key (IndexedDB, not extractable) ======

function idb() {
  return new Promise((ok, no) => {
    const req = indexedDB.open('spectrevoice', 1);
    req.onupgradeneeded = () => req.result.createObjectStore('keys');
    req.onsuccess = () => ok(req.result);
    req.onerror = () => no(req.error);
  });
}
const idbDo = (db, mode, f) => new Promise((ok, no) => {
  const tx = db.transaction('keys', mode);
  const r = f(tx.objectStore('keys'));
  tx.oncomplete = () => ok(r?.result);
  tx.onerror = () => no(tx.error);
});
async function deviceKey() {
  const db = await idb();
  let k = await idbDo(db, 'readonly', (s) => s.get('e2e-dk'));
  if (!k) {
    const n = await E.newDeviceKey(false);
    k = { priv: n.priv, spki: n.spki };
    await idbDo(db, 'readwrite', (s) => s.put(k, 'e2e-dk'));
  }
  return { priv: k.priv, spki: k.spki, raw: E.rawOfSpki(E.b64.dec(k.spki)), pub: (await E.importPublic(k.spki, 'ecdh')).key };
}

// ====== State ======

let ready = null;
const D = {
  key: null, open: '', convs: new Map(), devices: new Map(), asked: new Map(), queue: [], peers: new Map(), dms: [], filter: '',
  people: false, blocks: new Set(), names: new Map(), preview: new Map(),
};
export const isBlocked = (uid) => D.blocks.has(uid);
export function setBlocked(uid, on) {
  if (!D.people || !uid || uid === S.uid) return;
  send({ t: on ? 'block' : 'unblock', uid });
}
// Pins (no secrets): devices seen per person, their name, the identity key, whether verified
let pins = {};
try { pins = JSON.parse(store.get('sv.e2e.pins', '{}')) || {}; } catch { pins = {}; }
const savePins = () => store.set('sv.e2e.pins', JSON.stringify(pins));
const pin = (uid) => (pins[uid] ||= {});
const FRESH = 60000;

const peerOfConv = (to) => {
  if (typeof to !== 'string' || !to.startsWith('d:')) return null;
  const [a, b] = to.slice(2).split(':');
  return a === S.uid ? b : b === S.uid ? a : null;
};
const nameOf = (uid) => memberByUid(uid)?.name || D.dms.find((d) => d.with === uid)?.name || D.names.get(uid) || pins[uid]?.name || uid.slice(0, 8);
function conv(peer) {
  if (!D.convs.has(peer)) D.convs.set(peer, { items: [], more: false, loaded: false, seen: new Map(), unread: 0 });
  return D.convs.get(peer);
}

// ====== Keys ======

function ask(uid) {
  const now = Date.now();
  if ((D.asked.get(uid) || 0) > now - 5000) return;
  D.asked.set(uid, now);
  send({ t: 'e2e-keys', uid });
}
function want(uid) {
  const c = D.devices.get(uid);
  if (!c || Date.now() - c.at > FRESH) ask(uid);
}

on('app:welcome', async (m) => {
  D.devices.clear(); D.asked.clear();
  ready = deviceKey().then((k) => { D.key = k; }, (e) => { console.warn('e2e', e?.name); D.key = null; });
  await ready;
  if (D.key) send({ t: 'e2e-publish', rec: await E.signRecord(S.id, S.dev, D.key.spki) });
  for (const c of D.convs.values()) { c.items = []; c.loaded = false; c.seen.clear(); }
  D.people = !!m.server?.people;
  if (D.people) send({ t: 'blocks' });
  send({ t: 'dms' });
  if (D.open) { want(D.open); send({ t: 'history', to: 'u:' + D.open }); }
  tabs();
});

on('e2e-keys', async (m) => {
  const uid = m.uid;
  D.asked.delete(uid);
  const good = [];
  let bad = false;
  let ik = null;
  for (const r of (m.devices || []).slice(0, 16)) {
    const k = r?.uid === uid ? await E.verifyRecord(r) : null;
    if (!k || (ik && ik !== k.ik)) { bad = true; continue; }
    ik = k.ik;
    good.push(k);
  }
  D.devices.set(uid, { list: good, ik, at: Date.now() });
  if (uid !== S.uid) {
    const p = pin(uid);
    // The verified record's own key (not the first one the server listed under that device id)
    const now = await Promise.all(good.map(async (g) => g.dev + '=' + (await E.sha256b64(g.dk))));
    now.sort();
    if (good.length) {
      if (!p.devs) p.devs = now;
      else if (now.some((x) => !p.devs.includes(x))) p.changed = true;
      else if (!p.changed) p.devs = now;
    }
    if (ik) p.ik = ik;
    const n = memberByUid(uid)?.name;
    if (n && good.length) p.name = n;
    savePins();
    D.peers.set(uid, { supported: good.length > 0, bad, ik });
  }
  await flush();
  if (D.open === uid) render();
});
on('e2e-changed', (m) => { D.devices.delete(m.uid); D.asked.delete(m.uid); if (D.open === m.uid) ask(m.uid); });
on('error', (m) => {
  if (m.op === 'e2e-publish') { m.handled = true; console.warn('e2e: device key not published:', m.why); }
  if (m.op === 'e2e-keys') {
    m.handled = true;
    for (const uid of D.asked.keys()) { D.queue = D.queue.filter((o) => { if (o.peer === uid) dropPending(o); return o.peer !== uid; }); }
    D.asked.clear();
  }
  if (m.op === 'dm-read' || m.op === 'blocks' || ((m.op === 'block' || m.op === 'unblock') && m.why !== 'full')) m.handled = true;
  if ((m.op === 'e2e-say' || m.op === 'poke') && m.why === 'blocked') { m.handled = true; toast(t('dmBlocked'), true); }
  if (m.op === 'e2e-say' || (m.op === 'say' && m.why === 'e2e')) {
    for (const c of D.convs.values()) c.items = c.items.filter((x) => !x.pending);
    render();
  }
});

function dropPending(o, why) {
  const c = conv(o.peer);
  c.items = c.items.filter((x) => x.n !== o.n);
  toast(why || t('dmCantSend'), true);
  if (D.open === o.peer) render();
}

async function flush() {
  const now = Date.now();
  const keep = [];
  for (const o of D.queue) {
    const theirs = D.devices.get(o.peer);
    const mine = D.devices.get(S.uid);
    if (!theirs || !mine || now - theirs.at > FRESH + 5000 || now - mine.at > FRESH + 5000) {
      if (now - o.at > 15000) dropPending(o); else keep.push(o);
      continue;
    }
    if (!theirs.list.length) { dropPending(o, t('dmUnsupported')); continue; }
    const seen = new Set();
    const rec = [...theirs.list, ...mine.list.filter((r) => r.dev !== S.dev), { uid: S.uid, dev: S.dev, key: D.key.pub, raw: D.key.raw }]
      .filter((r) => { const k = r.uid + '\n' + r.dev; if (seen.has(k)) return false; seen.add(k); return true; }).slice(0, 32);
    try {
      const env = await E.seal(S.id, S.dev, o.peer, o.payload, rec);
      if (!send({ t: 'e2e-say', to: 'u:' + o.peer, e: env, n: o.n })) dropPending(o);
    } catch (e) { console.warn('e2e seal', e?.name); dropPending(o); }
  }
  D.queue = keep;
}
setInterval(() => { if (D.queue.length) flush(); }, 5000);

function sealAndSend(peer, payload) {
  if (!D.key) { toast(t('dmCantSend'), true); return; }
  const p = D.peers.get(peer);
  if (p && p.supported === false && D.devices.get(peer) && Date.now() - D.devices.get(peer).at < FRESH) { toast(t('dmUnsupported'), true); return; }
  const n = 'e' + Date.now() + Math.random().toString(36).slice(2, 6);
  conv(peer).items.push({ id: Number.MAX_SAFE_INTEGER, n, from: S.uid, name: S.name, text: payload.text, att: payload.att, at: Date.now(), pending: true, lock: 'sealed' });
  D.queue.push({ peer, n, payload, at: Date.now() });
  want(peer); want(S.uid);
  flush();
  render();
}

// ====== Receiving ======

async function decode(raw, peer) {
  const c = conv(peer);
  if (raw.del) return { ...raw, lock: 'none', text: '' };
  if (ready) await ready;
  if (typeof raw.e2e !== 'string') return { ...raw, lock: 'plain' };
  if (!D.key) return { ...raw, lock: 'before', text: '', note: 'NOT_FOR_THIS_DEVICE' };
  const o = await E.open(raw.e2e, S.uid, S.dev, D.key, raw.from, peer);
  // Sent before this browser had its key: not an error, the other devices read it
  if (!o.ok && o.why === 'NOT_FOR_THIS_DEVICE') return { ...raw, lock: 'before', text: '', note: o.why, mid: o.mid };
  if (!o.ok) return { ...raw, lock: 'broken', text: '', note: o.why, mid: o.mid };
  const first = c.seen.get(o.mid) ?? raw.id;
  if (!c.seen.has(o.mid)) c.seen.set(o.mid, raw.id);
  const note = first !== raw.id ? 'replayed' : Math.abs(o.ts - raw.at) > 3600000 ? 'late' : '';
  return { ...raw, e2e: undefined, lock: 'sealed', text: o.text, att: o.att, note, mid: o.mid };
}

on('msg', async (m) => {
  const peer = peerOfConv(m.m?.to);
  if (!peer) return;
  const x = await decode(m.m, peer);
  if (x.note === 'replayed') return;
  const c = conv(peer);
  const i = c.items.findIndex((y) => y.id === x.id || (m.n && y.n === m.n));
  if (i >= 0) c.items[i] = x; else c.items.push(x);
  c.items.sort((a, b) => (a.pending ? 1 : 0) - (b.pending ? 1 : 0) || a.id - b.id);
  const looking = D.open === peer && $('chat').dataset.view === 'dm' && !document.hidden;
  if (i < 0 && x.from !== S.uid && !x.del && !looking) c.unread++;
  if (i < 0 && x.from !== S.uid && !x.del && document.hidden) notify(t('dmFrom', { name: x.name }), x.text || (x.att ? '📎 ' + x.att.name : t('dmNewMsg')), () => openDm(peer));
  const d = D.dms.find((y) => y.with === peer);
  if (d) D.dms = [d, ...D.dms.filter((y) => y !== d)];
  else D.dms.unshift({ with: peer, name: x.from === peer ? x.name : nameOf(peer) });
  if (!x.pending) D.preview.set(peer, x);
  if (looking && x.from !== S.uid) markRead(peer);
  tabs();
  if (D.open === peer) render(true); else if ($('chat').dataset.view === 'dm' && !D.open) render();
});
document.addEventListener('visibilitychange', () => { if (!document.hidden && D.open && $('chat').dataset.view === 'dm') { conv(D.open).unread = 0; markRead(D.open); tabs(); } });

// The server's read marker for this chat: the last line shown (the other devices are told)
function markRead(peer) {
  if (!D.people || !peer) return;
  const c = conv(peer);
  const last = c.items.filter((x) => !x.pending && x.id < Number.MAX_SAFE_INTEGER).reduce((n, x) => Math.max(n, x.id), 0);
  if (last > (c.read || 0)) { c.read = last; send({ t: 'dm-read', with: peer, id: last }); }
}
on('history', async (m) => {
  const peer = peerOfConv(m.to);
  if (!peer) return;
  const c = conv(peer);
  c.loaded = true;
  c.more = !!m.more;
  const byID = new Map(c.items.filter((x) => !x.pending).map((x) => [x.id, x]));
  for (const raw of m.items || []) {
    const x = await decode(raw, peer);
    if (x.note !== 'replayed') byID.set(x.id, x);
  }
  c.items = [...byID.values()].sort((a, b) => a.id - b.id).slice(-2000).concat(c.items.filter((x) => x.pending));
  if (D.open === peer && $('chat').dataset.view === 'dm') markRead(peer);
  if (D.open === peer) render(!m.before);
});
on('dms', async (m) => {
  D.dms = (m.items || []).map((d) => ({ with: d.with, name: d.name, blocked: !!d.blocked }));
  for (const d of m.items || []) {
    const c = conv(d.with);
    if (typeof d.unread === 'number' && !(D.open === d.with && $('chat').dataset.view === 'dm')) c.unread = d.unread;
    if (typeof d.read === 'number') c.read = Math.max(c.read || 0, d.read);
  }
  tabs();
  // Each chat's last line, decrypted here for the list's preview
  for (const d of m.items || []) if (d.last) D.preview.set(d.with, await decode(d.last, d.with));
  if ($('chat').dataset.view === 'dm' && !D.open) render();
});
// Read on another device of ours
on('dm-read', (m) => {
  const c = conv(m.with);
  c.read = Math.max(c.read || 0, m.id || 0);
  if (!c.items.some((x) => x.id > c.read && x.from !== S.uid && !x.pending)) c.unread = 0;
  tabs();
  if ($('chat').dataset.view === 'dm') render();
});
on('blocks', (m) => { D.blocks = new Set(m.items || []); tabs(); emit('app:blocks'); if ($('chat').dataset.view === 'dm') render(); });

// ====== Files ======

async function sendFile(peer, file) {
  if (!file) return;
  const bar = el('div', { class: 'upload' }, el('span', { class: 'fname', text: '🔒 ' + file.name }), el('progress', { max: 1, value: 0 }));
  $('dmUploads').append(bar);
  try {
    const sealed = await E.encryptFile(new Uint8Array(await file.arrayBuffer()));
    const xhr = new XMLHttpRequest();
    xhr.open('POST', api('/files', { name: 'file.sve', space: 'main' }));
    xhr.upload.onprogress = (e) => { if (e.lengthComputable) bar.querySelector('progress').value = e.loaded / e.total; };
    xhr.onload = () => {
      bar.remove();
      let path = '';
      try { path = JSON.parse(xhr.responseText).url || ''; } catch { /* below */ }
      if (xhr.status !== 200 || !path) { toast(t('uploadFailed', { why: 'HTTP ' + xhr.status }), true); return; }
      sealAndSend(peer, { text: '', att: { url: path, k: sealed.k, np: sealed.np, size: file.size, name: file.name.slice(0, 180), mime: (file.type || '').slice(0, 100) } });
    };
    xhr.onerror = () => { bar.remove(); toast(t('uploadFailed', { why: t('errWs') }), true); };
    xhr.send(new Blob([sealed.data]));
  } catch (e) { bar.remove(); toast(t('dmCantSend'), true); }
}
const blobs = new Map();
async function openFile(att) {
  if (!/^\/f\/[0-9a-f]{32}\/[A-Za-z0-9._-]{1,120}$/.test(att.url)) return null;
  if (blobs.has(att.url)) return blobs.get(att.url);
  const r = await fetch(att.url);
  if (!r.ok) throw new Error('HTTP ' + r.status);
  const plain = await E.decryptFile(new Uint8Array(await r.arrayBuffer()), att.k, att.np);
  const u = URL.createObjectURL(new Blob([plain], { type: /^image\/(png|jpeg|gif|webp)$/.test(att.mime) ? att.mime : 'application/octet-stream' }));
  blobs.set(att.url, u);
  return u;
}
function fileCard(att, box) {
  const card = el('a', { class: 'filecard', href: '#', rel: 'noopener' }, el('span', { text: '🔒📎' }), el('span', { class: 'fname', text: att.name }), el('span', { class: 'muted small', text: fmtSize(att.size) + ' · ' + t('download') }));
  card.onclick = async (ev) => {
    ev.preventDefault();
    try {
      const u = await openFile(att);
      if (!u) return;
      const a = el('a', { href: u, download: att.name });
      document.body.append(a); a.click(); a.remove();
    } catch { toast(t('dmFileBad'), true); }
  };
  if (/^image\/(png|jpeg|gif|webp)$/.test(att.mime) && att.size < 15 * 1024 * 1024) {
    if (blobs.has(att.url)) box.append(el('div', { class: 'picbox' }, el('img', { class: 'pic', src: blobs.get(att.url), alt: att.name })));
    else openFile(att).then((u) => { if (u) { box.append(el('div', { class: 'picbox' }, el('img', { class: 'pic', src: u, alt: att.name }))); } }).catch(() => {});
  }
  return card;
}

// ====== The view ======

const view = el('div', { id: 'dmView', class: 'dmview' },
  el('div', { id: 'dmHead', class: 'dmhead' }),
  el('div', { id: 'dmBanner', class: 'dmbanners' }),
  el('div', { id: 'dmList', class: 'messages dmlist' }),
  el('div', { id: 'dmUploads', class: 'uploads' }),
  el('form', { id: 'dmForm', class: 'say' },
    el('button', { id: 'dmAttach', class: 'ghost icon', type: 'button', text: '📎' }),
    el('input', { id: 'dmFile', type: 'file', class: 'hidden', multiple: true }),
    el('input', { id: 'dmSay', maxLength: 4000, autocomplete: 'off' }),
    el('button', { class: 'primary', type: 'submit', id: 'dmSend' })));
$('chat').append(view);
const tab = el('button', { id: 'tabDm', type: 'button' });
document.querySelector('#chat .chat-tabs').append(tab);
tab.onclick = () => openDm(D.open || '');

export function openDm(peer, name) {
  D.open = peer;
  D.filter = '';
  if (peer && name) D.names.set(peer, name);
  if (peer) {
    conv(peer).unread = 0;
    markRead(peer);
    want(peer); want(S.uid);
    if (!conv(peer).loaded) send({ t: 'history', to: 'u:' + peer });
  }
  setView('dm');
  emit('app:showPane', 'chat');
  render(true);
  tabs();
}

function tabs() {
  const unread = [...D.convs.entries()].reduce((n, [u, c]) => n + (D.blocks.has(u) ? 0 : c.unread), 0);
  tab.textContent = t('dmTab');
  tab.dataset.count = unread > 99 ? '99+' : unread || '';
  tab.classList.toggle('on', $('chat').dataset.view === 'dm');
}
on('app:view', ({ view: v }) => { tabs(); if (v === 'dm') render(); });
on('app:lang', () => { tabs(); render(); });
on('app:down', () => { D.queue = []; });

function lockLabel(x) {
  return { sealed: '🔒 ' + t('dmE2e'), plain: '⚠ ' + t('dmPlain'), broken: '⛔ ' + t('dmBroken'), before: '🔒' }[x.lock] || '';
}
function noteText(x) {
  return { replayed: t('dmReplay'), late: t('dmLate'), NOT_FOR_THIS_DEVICE: t('dmNotForDevice'), NOT_FROM_SENDER: t('dmForged'), WRONG_CONVERSATION: t('dmForged'),
    BAD_SIGNATURE: t('dmChanged2'), DAMAGED: t('dmChanged2'), MALFORMED: t('dmBroken') }[x.note] || '';
}

function render(stick) {
  if ($('chat').dataset.view !== 'dm') return;
  const head = $('dmHead');
  const banner = $('dmBanner');
  const list = $('dmList');
  head.textContent = ''; banner.textContent = ''; list.textContent = '';
  $('dmSend').textContent = t('send');
  $('dmSay').placeholder = t('write');
  // Who: the open chat, and the others to switch to
  const others = [...new Set([...D.dms.map((d) => d.with), ...D.convs.keys()])].filter((u) => u && u !== S.uid && !D.blocks.has(u));
  if (D.open) head.append(el('button', { type: 'button', class: 'small', id: 'dmAll', text: '← ' + t('dmAll'), onclick: () => openDm('') }));
  for (const u of others.slice(0, 30)) {
    const c = D.convs.get(u);
    head.append(el('button', { type: 'button', class: 'small' + (u === D.open ? ' on' : ''), text: '@' + nameOf(u), dataset: { count: c?.unread ? String(c.unread) : '' }, onclick: () => openDm(u) }));
  }
  if (!D.open) {
    // The recent private chats: who, the last line (decrypted here), when, how many unread
    $('dmForm').classList.add('hidden');
    if (!others.length) { list.append(el('div', { class: 'empty muted', text: t('dmNone') })); return; }
    list.append(el('div', { class: 'section dmsection', text: t('dmRecent') }));
    for (const u of others.slice(0, 100)) {
      const c = D.convs.get(u);
      const last = D.preview.get(u) || c?.items.filter((x) => !x.pending).at(-1);
      let text = '';
      if (last) {
        if (last.del) text = t('deleted');
        else if (last.lock === 'before') text = '🔒 ' + t('dmEncrypted');
        else if (last.lock === 'broken') text = '⛔ ' + t('dmBroken');
        else text = (last.from === S.uid ? t('dmYou') : '') + (last.text || (last.att ? '📎 ' + last.att.name : ''));
      }
      const n = c?.unread || 0;
      list.append(el('button', { type: 'button', class: 'dmrow' + (n ? ' unread' : ''), dataset: { uid: u }, onclick: () => openDm(u) },
        avatar(nameOf(u), u, 36),
        el('span', { class: 'dmtext' }, el('span', { class: 'dmname', text: nameOf(u) }), el('span', { class: 'dmprev', text: text })),
        el('span', { class: 'dmmeta' }, last ? el('span', { class: 'when', text: new Date(last.at).toLocaleDateString() === new Date().toLocaleDateString() ? new Date(last.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : new Date(last.at).toLocaleDateString([], { day: 'numeric', month: 'short' }) }) : null,
          n ? el('span', { class: 'dmcount', text: n > 99 ? '99+' : String(n) }) : null)));
    }
    return;
  }
  $('dmForm').classList.remove('hidden');
  const peer = D.open;
  const p = D.peers.get(peer);
  const pp = pins[peer] || {};
  const name = nameOf(peer);
  const verified = !!(p?.ik && pp.verified && E.same(pp.verified, p.ik));
  const bar = el('div', { class: 'dmlock' + (p?.supported === false ? ' warn' : '') },
    el('span', { class: 'lockbadge', text: p?.supported === false ? '⚠ ' + t('dmUnsupported') : p?.supported ? '🔒 ' + t('dmE2e') : t('dmNoKeyYet') }),
    verified ? el('span', { class: 'verified', text: '✔ ' + t('dmVerified') }) : null,
    el('input', { id: 'dmFilter', class: 'dmfilter', placeholder: t('dmSearch'), value: D.filter, oninput: (ev) => { D.filter = ev.target.value; render(); setTimeout(() => { const f = $('dmFilter'); f.focus(); f.setSelectionRange(f.value.length, f.value.length); }); } }),
    p?.ik ? el('button', { type: 'button', class: 'small', id: 'dmVerifyBtn', text: t('dmVerify'), onclick: () => safety(peer) }) : null);
  banner.append(bar);
  if (pp.changed) banner.append(el('div', { class: 'dmwarn' }, el('span', { text: t('dmChanged', { name }) }),
    el('button', { type: 'button', class: 'small', text: t('dmVerify'), onclick: () => safety(peer) }),
    el('button', { type: 'button', class: 'small', text: t('dmOk'), onclick: () => acknowledge(peer) })));
  const same = Object.entries(pins).find(([u, q]) => u !== peer && q.name && q.name.toLowerCase() === name.toLowerCase());
  if (same) banner.append(el('div', { class: 'dmwarn bad', text: t('dmSameName', { name }) }));
  if (p?.bad) banner.append(el('div', { class: 'dmwarn bad', text: t('dmBadKeys', { name }) }));
  const blocked = D.blocks.has(peer);
  if (blocked) banner.append(el('div', { class: 'dmwarn', id: 'dmBlockedNote' }, el('span', { text: t('dmYouBlocked', { name }) }),
    el('button', { type: 'button', class: 'small', text: t('unblock'), onclick: () => setBlocked(peer, false) })));
  if (!memberByUid(peer) && !blocked && p?.supported) banner.append(el('div', { class: 'dmnote muted small', id: 'dmOfflineNote', text: t('dmOfflineNote', { name }) }));
  $('dmSay').disabled = p?.supported === false || blocked;
  $('dmAttach').disabled = p?.supported === false || !S.files || blocked;
  const c = conv(peer);
  if (c.more) list.append(el('button', { class: 'ghost small', type: 'button', text: t('older'), onclick: () => { const first = c.items.find((x) => !x.pending); if (first) send({ t: 'history', to: 'u:' + peer, before: first.id }); } }));
  const q = D.filter.trim().toLowerCase();
  for (const x of c.items) {
    if (q && !(x.text || '').toLowerCase().includes(q) && !(x.att?.name || '').toLowerCase().includes(q)) continue;
    const text = el('div', { class: 'text' });
    if (x.del) text.textContent = t('deleted');
    else if (x.lock === 'broken' || x.lock === 'before') text.textContent = noteText(x);
    else text.textContent = x.text;
    const row = el('div', { class: 'msg lock-' + (x.lock || 'none') + (x.pending ? ' pending' : '') + (x.from === S.uid ? ' own' : ''), dataset: { id: x.id } },
      el('span', { class: 'who', text: x.name }),
      el('span', { class: 'when', text: new Date(x.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) }),
      x.del ? null : el('span', { class: 'lock', title: lockLabel(x), text: lockLabel(x) }),
      text);
    if (x.att && !x.del) row.append(fileCard(x.att, row));
    if (x.note && x.lock !== 'broken' && x.lock !== 'before') row.append(el('div', { class: 'muted small', text: noteText(x) }));
    list.append(row);
  }
  if (stick) list.scrollTop = list.scrollHeight;
}

function acknowledge(uid) {
  const d = D.devices.get(uid);
  const p = pin(uid);
  if (d) p.devs = null;
  delete p.changed;
  savePins();
  if (d) { D.devices.delete(uid); ask(uid); } // the next list becomes the pinned one
  render();
}

async function safety(uid) {
  const p = D.peers.get(uid);
  if (!p?.ik) return;
  const n = await E.safetyNumber(S.id.uid, S.id.pub, uid, p.ik);
  const name = nameOf(uid);
  const groups = n.match(/.{5}/g);
  const verified = !!(pins[uid]?.verified && E.same(pins[uid].verified, p.ik));
  openDialog({
    title: t('dmSafety') + ' · ' + name, cls: 'safety',
    body: [el('p', { class: 'muted', text: t('dmSafetyHow', { name }) }), el('div', { class: 'safetygrid', id: 'safetyNumber' }, groups.map((g) => el('span', { text: g })))],
    actions: [
      verified ? { text: t('dmUnmark'), onclick: () => { delete pin(uid).verified; savePins(); render(); } } : { text: t('cancel') },
      { text: verified ? t('dmOk') : t('dmMark'), primary: true, onclick: () => { if (!verified) { pin(uid).verified = p.ik; delete pin(uid).changed; pin(uid).devs = null; savePins(); D.devices.delete(uid); ask(uid); } render(); } },
    ],
  });
}

$('dmForm').onsubmit = (ev) => {
  ev.preventDefault();
  const text = $('dmSay').value.trim();
  if (!text || !D.open) return;
  $('dmSay').value = '';
  sealAndSend(D.open, { text: text.slice(0, E.MAX_TEXT) });
};
$('dmAttach').onclick = () => $('dmFile').click();
$('dmFile').onchange = () => { for (const f of $('dmFile').files) sendFile(D.open, f); $('dmFile').value = ''; };

// "Message" on a person's card
on('app:personPop', ({ mem, pop }) => {
  if (!D.blocks.has(mem.uid)) pop.append(el('button', { type: 'button', class: 'small', id: 'dmOpen', text: t('dmMessage'), onclick: () => { pop.classList.add('hidden'); openDm(mem.uid, mem.name); } }));
  if (D.people) pop.append(el('button', { type: 'button', class: 'small', id: 'dmBlock', text: D.blocks.has(mem.uid) ? t('unblock') : t('block'), onclick: () => { pop.classList.add('hidden'); setBlocked(mem.uid, !D.blocks.has(mem.uid)); } }));
});
