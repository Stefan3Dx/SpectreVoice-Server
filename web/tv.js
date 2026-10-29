// The server's TV (IPTV): one channel for everyone, played by the server into the stream room "tv:main" and watched
// like a share. The list comes from GET /tv/channels (names, groups, whether there's a logo), the logos from
// GET /tv/logo (fetched by the server: the page loads pictures only from its own server). Changing the channel:
// those with "tv", or anyone when it's open. Those with "tv" set the playlist (an address, or an .m3u uploaded to
// POST /tv/playlist), read it again, open it to everyone, turn the TV off or forget the list.

import { $, S, el, send, on, emit, toast, api, may, openDialog } from './core.js';
import { t } from './i18n.js';
import { watchRoom, watching, renameWatch, rewatch, stopWatching, TV_ROOM } from './streams.js';

const TV = { list: null, rev: -1, loading: false, err: '', q: '', group: '', shown: 150, dlg: null };

const canChange = () => !!S.tv && (may('tv') || S.tv.open);
const logo = (i) => api('/tv/logo', { i, r: TV.rev });

async function loadList(force) {
  const st = S.tv;
  if (!st?.set) { TV.list = []; TV.err = ''; return; }
  if (!force && TV.list && TV.rev === st.rev) return;
  TV.loading = true;
  renderDialog();
  try {
    const r = await fetch(api('/tv/channels'));
    const j = await r.json().catch(() => ({}));
    if (r.ok) { TV.list = j.items || []; TV.rev = j.rev ?? st.rev; TV.err = ''; }
    else { TV.list = []; TV.err = j.why === 'none' ? t('tvNoList') : (j.error || ('HTTP ' + r.status)); }
  } catch (e) {
    TV.err = String(e?.message || e);
  }
  TV.loading = false;
  renderDialog();
}

function play(c) {
  if (!canChange()) { toast(t('tvNotAllowed'), true); return; }
  send({ t: 'tv', op: 'play', i: c.i, n: c.n });
}

export function watchTv() {
  const now = S.tv?.now;
  watchRoom(TV_ROOM, { name: '📺 ' + (now?.n || t('tv')) });
}

// ====== The card in the sidebar ======

function renderCard() {
  const card = $('tvCard');
  const st = S.tv;
  card.textContent = '';
  card.classList.toggle('hidden', !st?.on || (!st.set && !may('tv')));
  if (!st?.on) return;
  const now = st.now;
  const head = el('div', { class: 'tvhead' },
    now?.l ? el('img', { class: 'tvlogo', src: logo(now.i), alt: '', loading: 'lazy' }) : el('span', { class: 'tvlogo none', text: '📺' }),
    el('div', { class: 'tvnow' },
      el('div', { class: 'lname', text: t('tv') }),
      el('div', { class: 'muted small', text: now ? now.n + (now.live ? '' : ' · ' + (now.err ? t('tvTrouble') : now.playing ? t('tvStarting') : t('tvIdle'))) : (st.set ? t('tvNothing') : t('tvNoList')) })));
  const row = el('div', { class: 'row' });
  if (now) {
    const w = watching(TV_ROOM);
    row.append(el('button', { type: 'button', id: 'tvWatch', class: w ? 'small' : 'small primary', text: w ? t('stopWatching') : t('watch'), onclick: () => { if (watching(TV_ROOM)) stopWatching(TV_ROOM); else { watchTv(); emit('app:showPane', 'stage'); } renderCard(); } }));
  }
  row.append(el('button', { type: 'button', id: 'tvOpen', class: 'small', text: t('tvChannels'), onclick: openTv }));
  card.append(head, row);
}

// ====== The dialog: channels, and the playlist for those with "tv" ======

export function openTv() {
  TV.shown = 150;
  const body = el('div', { class: 'tv' });
  TV.dlg = { body };
  openDialog({ title: t('tv'), body, wide: true, cls: 'tvdlg', onclose: () => { TV.dlg = null; } });
  renderDialog();
  loadList();
}

function renderDialog() {
  if (!TV.dlg) return;
  const body = TV.dlg.body;
  const st = S.tv || {};
  const keepFocus = document.activeElement?.id === 'tvSearch';
  const caret = keepFocus ? document.activeElement.selectionStart : 0;
  body.textContent = '';
  // Now playing
  const now = st.now;
  body.append(el('div', { class: 'tvbar' },
    el('div', { class: 'tvnow' },
      el('div', { class: 'lname', text: now ? now.n : (st.set ? t('tvNothing') : t('tvNoList')) }),
      now ? el('div', { class: 'muted small', text: t('tvBy', { name: now.byName || '?', time: new Date(now.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) }) + (now.g ? ' · ' + now.g : '') }) : null),
    now ? el('button', { type: 'button', class: 'primary small', text: watching(TV_ROOM) ? t('stopWatching') : t('watch'), onclick: () => { if (watching(TV_ROOM)) stopWatching(TV_ROOM); else { watchTv(); $('dlg').close(); emit('app:showPane', 'stage'); } } }) : null,
    now && canChange() ? el('button', { type: 'button', class: 'small', text: t('tvOff'), onclick: () => send({ t: 'tv', op: 'stop' }) }) : null));
  if (!st.on) { body.append(el('p', { class: 'muted', text: t('tvDisabled') })); return; }
  if (!canChange() && st.set) body.append(el('p', { class: 'muted small', text: t('tvOnlySome') }));

  // The list
  if (st.set) {
    const groups = [...new Set((TV.list || []).map((c) => c.g).filter(Boolean))].sort((a, b) => a.localeCompare(b));
    const search = el('input', { id: 'tvSearch', type: 'search', value: TV.q, placeholder: t('tvSearch'), attrs: { 'aria-label': t('tvSearch') } });
    search.oninput = () => { TV.q = search.value; TV.shown = 150; renderDialog(); };
    const group = el('select', { id: 'tvGroup', attrs: { 'aria-label': t('tvGroup') } }, el('option', { value: '', text: t('tvAllGroups') }), groups.map((g) => el('option', { value: g, text: g, selected: g === TV.group })));
    group.onchange = () => { TV.group = group.value; TV.shown = 150; renderDialog(); };
    body.append(el('div', { class: 'row tvfilter' }, search, groups.length ? group : null));
    if (TV.loading && !TV.list) body.append(el('p', { class: 'muted', text: t('loading') }));
    if (TV.err) body.append(el('p', { class: 'error', text: TV.err }));
    const q = TV.q.trim().toLowerCase();
    const items = (TV.list || []).filter((c) => (!TV.group || c.g === TV.group) && (!q || c.n.toLowerCase().includes(q)));
    const list = el('div', { class: 'tvlist', id: 'tvList' });
    for (const c of items.slice(0, TV.shown)) {
      const cur = now && now.i === c.i;
      list.append(el('button', { type: 'button', class: 'tvch' + (cur ? ' on' : ''), disabled: !canChange() && !cur, dataset: { i: c.i }, title: c.g || '', onclick: () => play(c) },
        c.l ? el('img', { class: 'tvlogo', src: logo(c.i), alt: '', loading: 'lazy', decoding: 'async' }) : el('span', { class: 'tvlogo none', text: '📺' }),
        el('span', { class: 'tvname', text: c.n }),
        c.g ? el('span', { class: 'muted small tvgroup', text: c.g }) : null));
    }
    if (TV.list && !items.length && !TV.loading) list.append(el('p', { class: 'muted', text: t('tvNone') }));
    body.append(list);
    if (items.length > TV.shown) body.append(el('button', { type: 'button', class: 'small', text: t('showMore', { n: items.length - TV.shown }), onclick: () => { TV.shown += 300; renderDialog(); } }));
  }

  // The playlist (those with "tv")
  if (may('tv')) {
    const urlIn = el('input', { id: 'tvUrl', type: 'url', placeholder: 'https://…/playlist.m3u', autocomplete: 'off', attrs: { 'aria-label': t('tvUrl') } });
    const file = el('input', { type: 'file', accept: '.m3u,.m3u8,audio/x-mpegurl,application/vnd.apple.mpegurl,text/plain', class: 'hidden', id: 'tvFile' });
    file.onchange = () => { if (file.files[0]) uploadPlaylist(file.files[0]); file.value = ''; };
    const open = el('input', { type: 'checkbox', id: 'tvOpenAll', checked: !!st.open });
    open.onchange = () => send({ t: 'tv', op: 'open', v: open.checked });
    body.append(el('details', { class: 'tvadmin', open: !st.set },
      el('summary', { text: t('tvPlaylist') }),
      el('p', { class: 'muted small', text: st.set ? t('tvListFrom', { n: st.n || 0, from: st.kind === 'file' ? t('tvFromFile') : (st.host || '?') }) : t('tvNoList') }),
      el('form', { class: 'row', onsubmit: (ev) => { ev.preventDefault(); const u = urlIn.value.trim(); if (u) { send({ t: 'tv', op: 'playlist', url: u }); toast(t('tvReading')); urlIn.value = ''; } } },
        urlIn, el('button', { type: 'submit', class: 'primary small', text: t('tvSet') })),
      el('div', { class: 'row wrap' },
        st.set && st.kind === 'url' ? el('button', { type: 'button', class: 'small', id: 'tvRefresh', text: t('tvRefresh'), onclick: () => { send({ t: 'tv', op: 'refresh' }); toast(t('tvReading')); } }) : null,
        el('button', { type: 'button', class: 'small', text: t('tvUpload'), onclick: () => file.click() }), file,
        st.set ? el('button', { type: 'button', class: 'small danger', text: t('tvClear'), onclick: () => send({ t: 'tv', op: 'clear' }) }) : null),
      el('label', { class: 'chk' }, open, el('span', { text: t('tvOpenAll') }))));
  }
  if (keepFocus) { const s = $('tvSearch'); if (s) { s.focus(); s.setSelectionRange(caret, caret); } }
}

async function uploadPlaylist(f) {
  toast(t('tvReading'));
  try {
    const r = await fetch(api('/tv/playlist'), { method: 'POST', body: f });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) { toast(t('tvError', { why: j.error || ('HTTP ' + r.status) }), true); return; }
    toast(t('tvRead', { n: j.n ?? 0 }));
  } catch (e) { toast(t('tvError', { why: e?.message || e }), true); }
}

// ====== Wiring ======

function changed(st) {
  const before = S.tv;
  S.tv = st || null;
  if (S.tv?.rev !== TV.rev) { TV.list = null; if (TV.dlg) loadList(true); }
  const now = S.tv?.now;
  if (now) renameWatch(TV_ROOM, '📺 ' + now.n);
  if (now?.live && (!before?.now?.live || before.now.i !== now.i)) rewatch(TV_ROOM);
  renderCard();
  renderDialog();
}
on('app:welcome', (m) => { TV.list = null; TV.rev = -1; changed(m.tv); });
on('tv', (m) => changed(m.tv));
on('app:render', renderCard);
on('app:perms', () => { renderCard(); renderDialog(); });
on('app:watching', (m) => { if (m.room === TV_ROOM) { renderCard(); renderDialog(); } });
on('app:lang', () => { renderCard(); renderDialog(); });
on('done', (m) => {
  if (m.op !== 'tv') return;
  toast(t('tvRead', { n: m.n ?? 0 }));
  loadList(true);
});
on('error', (m) => {
  if (m.op !== 'tv') return;
  m.handled = true;
  const k = { role: 'tvNotAllowed', off: 'tvDisabled', ffmpeg: 'tvNoFfmpeg', none: 'tvNoList', busy: 'tvBusy', rate: 'errRate' }[m.why];
  toast(k ? t(k) : t('tvError', { why: m.message || m.why }), true);
});
