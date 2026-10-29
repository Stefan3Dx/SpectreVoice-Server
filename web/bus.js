// The bus (bus.go): app to app through the server, the same /bus the PC and phone apps use (their Bus.kt). One
// connection per page, shared by every module: it opens after the welcome (signed in with its token), comes back
// after a drop, closes when the page leaves the server.
//
//   busSend({t: 'mreact', …})       passed on to the others (the server stamps "from": uid, "relay": true)
//   busKept('profiles')             what the server keeps, by kind → id → value (arrives whole on connecting)
//   on('bus:<t>', m)                every message from the bus by its "t" (relayed ones and the server's own)
//   on('bus:open')                  connected, what's kept is in
//   on('bus:kept', {kind, id})      something kept changed (put/del; id '' after the whole state)
//
// Never trusted: what others send is data, checked by whoever uses it.

import { S, on, emit, wsUrl } from './core.js';

export const bus = { ws: null, online: false, kept: {}, ops: [], admin: false, space: 'main', retry: 0, timer: 0 };

export function busSend(m) {
  if (bus.ws && bus.ws.readyState === 1 && bus.online) { bus.ws.send(JSON.stringify(m)); return true; }
  return false;
}
export const busKept = (kind) => bus.kept[kind] || {};

function open() {
  clearTimeout(bus.timer);
  if (!S.token || S.phase !== 'on') return;
  let ws;
  try { ws = new WebSocket(wsUrl('/bus?' + new URLSearchParams({ token: S.token, space: bus.space }))); } catch { return; }
  bus.ws = ws;
  ws.onmessage = (ev) => {
    if (ws !== bus.ws) return;
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (!m || typeof m.t !== 'string') return;
    switch (m.t) {
      case 'state':
        bus.kept = m.state && typeof m.state === 'object' ? m.state : {};
        bus.ops = Array.isArray(m.ops) ? m.ops : [];
        bus.admin = m.admin === true;
        bus.online = true;
        bus.retry = 0;
        emit('bus:open');
        emit('bus:kept', { kind: '', id: '' });
        break;
      case 'put':
      case 'del':
        if (typeof m.kind !== 'string' || typeof m.id !== 'string') break;
        if (!bus.kept[m.kind]) bus.kept[m.kind] = {};
        if (m.t === 'put') bus.kept[m.kind][m.id] = m.v && typeof m.v === 'object' ? m.v : {};
        else delete bus.kept[m.kind][m.id];
        emit('bus:kept', { kind: m.kind, id: m.id });
        break;
    }
    emit('bus:' + m.t, m);
  };
  ws.onclose = () => {
    if (ws !== bus.ws) return;
    bus.ws = null;
    bus.online = false;
    emit('bus:down');
    if (S.phase !== 'on') return;
    bus.timer = setTimeout(open, Math.min(30000, 1000 * 2 ** Math.min(bus.retry++, 5)));
  };
  ws.onerror = () => { /* onclose follows */ };
}

function close() {
  clearTimeout(bus.timer);
  const ws = bus.ws;
  bus.ws = null;
  bus.online = false;
  if (ws) { try { ws.close(); } catch { /* gone */ } }
}

on('app:welcome', (m) => {
  bus.space = (typeof m?.space === 'string' && m.space) || (typeof m?.server?.space === 'string' && m.server.space) || 'main';
  close();
  bus.retry = 0;
  open();
});
on('app:down', close);
