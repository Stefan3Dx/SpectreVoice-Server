// The member list (people.go): a "Members" tab in the right column next to the channel's view, as Discord's list.
// Online: everyone connected (the sessions this page already has, live); Offline: everyone else who has been on the
// server, asked from the server in pages ("members" with offline: true) as the list is scrolled, searched there too.
// Each section folds; a person's card has "Message" (a private chat, offline people get it when they come back) and
// Block / Unblock. People one blocked aren't listed (the server leaves them out; the online ones here too).

import { $, S, el, send, on, store } from './core.js';
import { t } from './i18n.js';
import { avatar } from './avatar.js';
import { openDm, isBlocked, setBlocked } from './dm.js';

const PAGE = 100;
const M = {
  people: false, seen: false, tab: store.get('sv.rpTab', 'channel'), q: '', fold: {},
  offline: [], total: 0, asked: -1, loading: false, roles: new Map(), timer: 0,
};
try { M.fold = JSON.parse(store.get('sv.memFold', '{}')) || {}; } catch { M.fold = {}; }

const box = document.querySelector('#people .peoplebox');
const tabs = el('div', { class: 'rptabs', attrs: { role: 'tablist' } },
  el('button', { type: 'button', id: 'rpChannel', dataset: { tab: 'channel' } }),
  el('button', { type: 'button', id: 'rpMembers', dataset: { tab: 'members' } }));
const search = el('input', { id: 'memSearch', class: 'memsearch', type: 'search', maxLength: 40, autocomplete: 'off' });
const list = el('div', { id: 'memList', class: 'memlist' });
const view = el('div', { id: 'membersView', class: 'membersview' }, search, list);
box.prepend(tabs);
box.append(view);
for (const b of tabs.children) b.onclick = () => setTab(b.dataset.tab);

function setTab(tab) {
  M.tab = M.people ? tab : 'channel';
  store.set('sv.rpTab', M.tab);
  box.dataset.tab = M.tab;
  for (const b of tabs.children) b.classList.toggle('on', b.dataset.tab === M.tab);
  if (M.tab === 'members') { if (M.asked < 0) ask(true); render(); }
}

// The offline people, a page at a time (fresh: from the start again)
function ask(fresh) {
  if (!M.people || M.loading) return;
  if (!fresh && M.offline.length >= M.total) return;
  M.loading = true;
  M.asked = fresh ? 0 : M.offline.length;
  send({ t: 'members', offline: true, q: M.q, offset: M.asked, limit: PAGE });
}
const askSoon = () => { clearTimeout(M.timer); M.timer = setTimeout(() => { if (M.tab === 'members') ask(true); else M.asked = -1; }, 800); };

on('app:welcome', (m) => {
  M.people = !!m.server?.people;
  M.seen = !!m.server?.membersSeen;
  M.roles = new Map((m.roles || []).map((r) => [r.id, r]));
  M.offline = []; M.total = 0; M.asked = -1; M.loading = false;
  tabs.classList.toggle('hidden', !M.people);
  setTab(M.tab);
});
on('roles', (m) => { M.roles = new Map((m.items || []).map((r) => [r.id, r])); render(); });
on('members', (m) => {
  if (!m.offline || (m.q || '') !== M.q) return;
  M.loading = false;
  M.total = m.total || 0;
  const items = m.items || [];
  if (!m.offset) M.offline = items; else if (m.offset === M.offline.length) M.offline = M.offline.concat(items);
  render();
});
on('error', (m) => { if (m.op === 'members') { m.handled = true; M.loading = false; } });
// Someone comes or goes: online is live; the offline list again a little later
on('member', (m) => { if (M.offline.some((p) => p.uid === m.m?.uid)) { M.offline = M.offline.filter((p) => p.uid !== m.m.uid); M.total = Math.max(0, M.total - 1); } render(); });
on('member-left', () => { render(); askSoon(); });
on('app:render', () => render());
on('app:lang', () => render());
on('app:blocks', () => { render(); askSoon(); });

search.oninput = () => {
  M.q = search.value.trim().slice(0, 40);
  render();
  clearTimeout(M.timer);
  M.timer = setTimeout(() => ask(true), 250);
};
list.onscroll = () => { if (list.scrollTop + list.clientHeight > list.scrollHeight - 200) ask(false); };

const colorOf = (roleIds) => {
  let best = null;
  for (const id of roleIds || []) {
    const r = M.roles.get(id);
    if (r?.color && (!best || r.pos > best.pos)) best = r;
  }
  return best ? '#' + (best.color & 0xffffff).toString(16).padStart(6, '0') : '';
};

function online() {
  const by = new Map();
  for (const m of S.members.values()) {
    if (m.bot && !m.client?.startsWith('bot/')) continue; // the music bot isn't a member
    if (isBlocked(m.uid)) continue;
    const o = by.get(m.uid);
    // Away only when every session is
    if (!o) by.set(m.uid, { uid: m.uid, name: m.name, roles: m.roles, away: !!m.away, awayMsg: m.awayMsg || '', bot: !!m.bot, online: true });
    else if (!m.away) { o.away = false; o.awayMsg = ''; }
  }
  const q = M.q.toLowerCase();
  return [...by.values()].filter((p) => !q || p.name.toLowerCase().includes(q)).sort((a, b) => a.name.localeCompare(b.name));
}

function ago(ms) {
  const m = Math.max(0, Math.round((Date.now() - ms) / 60000));
  if (m < 60) return t('agoMin', { n: m });
  if (m < 48 * 60) return t('agoHours', { n: Math.round(m / 60) });
  return t('agoDays', { n: Math.round(m / 1440) });
}

function row(p) {
  const color = colorOf(p.roles);
  const name = el('span', { class: 'mname', text: p.name + (p.uid === S.uid ? t('youSuffix') : '') });
  if (color) name.style.color = color;
  const sub = p.online ? (p.away ? t('away') + (p.awayMsg ? ': ' + p.awayMsg : '') : '') : (M.seen && p.seen ? t('lastSeen', { when: ago(p.seen) }) : '');
  const av = el('span', { class: 'memav' }, avatar(p.name, p.uid, 32), el('span', { class: 'pres ' + (p.online ? (p.away ? 'away' : 'on') : 'off') }));
  const r = el('div', { class: 'mem big drow' + (p.online ? '' : ' offline'), dataset: { uid: p.uid } }, av,
    el('span', { class: 'mtext' }, name, sub ? el('span', { class: 'msub', text: sub }) : null),
    p.bot ? el('span', { class: 'flags' }, el('span', { class: 'f-emo', title: t('bot'), text: '🤖' })) : null);
  if (p.uid !== S.uid && !p.bot) r.onclick = (ev) => card(p, ev);
  return r;
}

function section(key, title, people, more) {
  const folded = !!M.fold[key];
  const head = el('button', { type: 'button', class: 'memhead' + (folded ? ' folded' : ''), id: 'mem-' + key, attrs: { 'aria-expanded': folded ? 'false' : 'true' } },
    el('span', { class: 'chev', text: '▾' }), el('span', { text: title }));
  head.onclick = () => { M.fold[key] = !folded; store.set('sv.memFold', JSON.stringify(M.fold)); render(); };
  const out = [head];
  if (!folded) {
    for (const p of people) out.push(row(p));
    if (more) out.push(more);
  }
  return out;
}

function render() {
  if (!M.people) return;
  $('rpChannel').textContent = t('rpChannel');
  $('rpMembers').textContent = t('rpMembers');
  search.placeholder = t('membersSearch');
  if (M.tab !== 'members') return;
  const top = list.scrollTop;
  list.textContent = '';
  const on = online();
  const onSet = new Set(on.map((p) => p.uid));
  const off = M.offline.filter((p) => !onSet.has(p.uid) && !isBlocked(p.uid));
  const offN = Math.max(off.length, M.total - (M.offline.length - off.length));
  let more = null;
  if (M.offline.length < M.total) {
    more = el('button', { type: 'button', class: 'ghost small memmore', text: M.loading ? '…' : t('membersMore'), onclick: () => ask(false) });
  }
  list.append(...section('on', t('membersOnline', { n: on.length }), on));
  list.append(...section('off', t('membersOffline', { n: offN }), off, more));
  if (!on.length && !off.length && !M.loading) list.append(el('div', { class: 'empty muted', text: t('membersNone') }));
  list.scrollTop = top;
}

// A person's card from the list: write to them (online or not), block or unblock
function card(p, ev) {
  const pop = $('volPop');
  pop.textContent = '';
  const blocked = isBlocked(p.uid);
  pop.append(el('div', { class: 'popname' }, avatar(p.name, p.uid, 40),
    el('span', {}, el('span', { text: p.name }), el('span', { class: 'muted small', text: ' · ' + (p.online ? (p.away ? t('away') : t('online')) : t('offline')) }))));
  if (!p.online) pop.append(el('p', { class: 'muted small', text: t('dmOfflineNote', { name: p.name }) }));
  pop.append(el('div', { class: 'row' },
    blocked ? null : el('button', { type: 'button', class: 'small primary', id: 'memMessage', text: t('dmMessage'), onclick: () => { pop.classList.add('hidden'); openDm(p.uid, p.name); } }),
    el('button', { type: 'button', class: 'small' + (blocked ? '' : ' danger'), id: 'memBlock', text: blocked ? t('unblock') : t('block'), onclick: () => { pop.classList.add('hidden'); setBlocked(p.uid, !blocked); } })));
  const r = ev.currentTarget.getBoundingClientRect();
  pop.classList.remove('hidden');
  pop.style.left = Math.max(8, Math.min(innerWidth - 280, r.left - 270)) + 'px';
  pop.style.top = Math.max(8, Math.min(innerHeight - 200, r.top)) + 'px';
  ev.stopPropagation();
}
