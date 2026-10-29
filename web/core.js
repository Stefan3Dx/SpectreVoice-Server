// Spectre Voice web client: what every module shares. The state (S), the connection's send, an event bus (server
// messages by their "t", and the page's own "app:*" events), small DOM helpers and the generic dialog.
//
// A module is a file imported by app.js that registers itself with on(…): it never reaches into another module's
// state, only into S and the exports here. Events the page emits:
//   app:welcome (the welcome, after the core took it)  app:render (the tree and chat were drawn again)
//   app:down (the connection went, for good or to retry)  app:lang (the language changed)
//   app:conv (the open conversation changed: {to})  app:perms (one's permissions changed)
// Server messages come as on('<t>', m) after the core handled them (welcome, member, msg, live, tv, cf, p2p-…, error…).

export const $ = (id) => document.getElementById(id);

// el('div', {class: 'x', text: 'y', onclick: f, title: …, dataset: {…}, attrs: {…}}, child, …)
export function el(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') e.className = v;
    else if (k === 'text') e.textContent = v;
    else if (k === 'dataset') Object.assign(e.dataset, v);
    else if (k === 'attrs') for (const [a, av] of Object.entries(v)) { if (av !== undefined && av !== null && av !== false) e.setAttribute(a, av === true ? '' : av); }
    else if (k.startsWith('on')) e[k] = v;
    else e[k] = v;
  }
  for (const k of kids.flat()) if (k !== null && k !== undefined && k !== false) e.append(k);
  return e;
}

export const b64 = {
  enc: (bytes) => btoa(String.fromCharCode(...new Uint8Array(bytes))),
  dec: (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0)),
  url: (bytes) => b64.enc(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''),
};
export const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
export const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* private window */ } },
};

// A short note at the bottom; a popover, so it shows above an open dialog too
let toastTimer;
export function toast(text, err) {
  const e = $('toast');
  e.textContent = text;
  e.classList.toggle('err', !!err);
  e.classList.remove('hidden');
  if (e.showPopover) { try { if (e.matches(':popover-open')) e.hidePopover(); e.showPopover(); } catch { /* not a popover */ } }
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { e.classList.add('hidden'); try { e.hidePopover?.(); } catch { /* closed */ } }, err ? 6000 : 3500);
}

export function fmtSize(n) {
  if (!(n >= 0)) return '';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(n < 10240 ? 1 : 0) + ' KB';
  if (n < 1024 ** 3) return (n / 1024 / 1024).toFixed(n < 10 * 1024 * 1024 ? 1 : 0) + ' MB';
  return (n / 1024 ** 3).toFixed(1) + ' GB';
}

// ====== State ======

export const S = {
  id: null, ws: null, phase: 'off', sid: 0, uid: '', name: '', pass: '', invite: '', wantCh: 0, token: '',
  channels: new Map(), members: new Map(), chat: new Map(), cur: '', ice: [], retry: 0, timer: null, wasOn: false, lastCh: 0,
  mic: false, snd: false, open: false, pressing: false, dev: '', version: '', server: '', volumes: {}, muted: {},
  perms: new Set(), staff: false, live: new Map(), tv: null, files: false, unread: new Map(),
};
try { S.volumes = JSON.parse(store.get('sv.volumes', '{}')) || {}; } catch { S.volumes = {}; }
try { S.muted = JSON.parse(store.get('sv.mutedPeople', '{}')) || {}; } catch { S.muted = {}; }
S.dev = store.get('sv.dev', '');
if (!S.dev) {
  S.dev = b64.url(crypto.getRandomValues(new Uint8Array(12)));
  store.set('sv.dev', S.dev);
}
export const me = () => S.members.get(S.sid);
export const memberByUid = (uid) => { for (const m of S.members.values()) if (m.uid === uid) return m; return null; };
// The server chat: everyone on the server
export const SERVER_CHAT = 'g:1';
// One's permissions on the server (welcome "perms", then "role" messages); "administrator" is all of them
export const may = (p) => S.staff || S.perms.has('administrator') || S.perms.has(p);

export function send(m) {
  if (S.ws && S.ws.readyState === 1) { S.ws.send(JSON.stringify(m)); return true; }
  return false;
}

// Addresses on this server (the page's own origin), signed in with the session's token
export function api(path, params) {
  const q = new URLSearchParams({ ...(params || {}), token: S.token });
  return path + '?' + q.toString();
}
export function wsUrl(path) {
  return (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + path;
}
export const rtcConfig = () => ({ iceServers: S.ice.map((s) => ({ urls: s.urls, username: s.username, credential: s.credential })) });

// ====== Events ======

const listeners = new Map();
export function on(t, fn) {
  if (!listeners.has(t)) listeners.set(t, []);
  listeners.get(t).push(fn);
}
export function emit(t, m) {
  for (const fn of listeners.get(t) || []) {
    try { fn(m); } catch (e) { console.error(t, e); }
  }
}

// ====== Dialog ======

// openDialog({title, body: node(s), actions: [{text, primary, onclick (return false keeps it open)}], wide, onclose})
// One at a time; the page's <dialog id="dlg">. Returns {close, root}.
let dlgClose = null;
export function openDialog({ title, body, actions, wide, onclose, cls }) {
  const d = $('dlg');
  if (d.open) d.close();
  d.textContent = '';
  d.className = 'dlg' + (wide ? ' wide' : '') + (cls ? ' ' + cls : '');
  const close = () => { if (d.open) d.close(); };
  const head = el('div', { class: 'dlg-head' }, el('h2', { text: title || '' }), el('button', { class: 'ghost icon', type: 'button', text: '✕', attrs: { 'aria-label': 'close' }, onclick: close }));
  const content = el('div', { class: 'dlg-body' }, body);
  const foot = el('div', { class: 'dlg-foot' });
  for (const a of actions || []) {
    foot.append(el('button', {
      class: a.primary ? 'primary' : (a.danger ? 'danger' : ''), type: a.submit ? 'submit' : 'button', text: a.text, id: a.id,
      onclick: async (ev) => { ev.preventDefault(); if (a.onclick && (await a.onclick()) === false) return; close(); },
    }));
  }
  d.append(head, content);
  if ((actions || []).length) d.append(foot);
  dlgClose = onclose || null;
  d.onclose = () => { const f = dlgClose; dlgClose = null; if (f) f(); };
  d.showModal();
  return { close, root: d };
}
export const dialogOpen = () => $('dlg').open;

// A channel's name as headers show it: "# name" for a text channel, the bare name for a voice one (its mic is drawn
// next to it where there's room); empty for none.
export const chLabel = (c) => (!c ? '' : c.kind === 'text' ? '# ' + c.name : c.name);
