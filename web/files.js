// Channel files (chfiles.go): each channel's folders and files. Who may read the channel sees and downloads them
// (GET /cf/<id>?token=…), anyone there uploads (POST /cf/upload?ch&folder&name, the body is the file) and makes
// folders; one renames and deletes one's own, those with "files" anyone's. Shown in the middle column, the
// "Files" tab next to the chat's, for the channel whose chat is open.

import { $, S, el, send, on, toast, api, may, openDialog, fmtSize, chLabel } from './core.js';
import { t } from './i18n.js';
import { setView } from './chat.js';

const F = { ch: 0, folder: 0, path: [], items: [], used: 0, quota: 0, maxFile: 0, loading: false, uploads: 0 };

const curChannel = () => (S.cur.startsWith('c:') ? +S.cur.slice(2) : 0);

function list(ch, folder) {
  F.ch = ch;
  F.folder = folder;
  F.loading = true;
  send({ t: 'cf-list', ch, folder });
  render();
}

export function openFiles() {
  const ch = curChannel();
  if (!ch) return;
  setView('files');
  if (F.ch !== ch) { F.items = []; F.path = []; }
  list(ch, F.ch === ch ? F.folder : 0);
}

function mineOrBoss(it) { return it.by === S.uid || may('files'); }

function render() {
  const box = $('filesView');
  if ($('chat').dataset.view !== 'files') return;
  box.textContent = '';
  const ch = S.channels.get(F.ch);
  const crumbs = el('div', { class: 'crumbs' }, el('button', { type: 'button', class: 'ghost small', text: chLabel(ch) || '?', onclick: () => list(F.ch, 0) }));
  for (const p of F.path) crumbs.append(el('span', { class: 'muted', text: '›' }), el('button', { type: 'button', class: 'ghost small', text: p.name, onclick: () => list(F.ch, p.id) }));
  const input = el('input', { type: 'file', multiple: true, class: 'hidden', id: 'cfFile' });
  input.onchange = () => { for (const f of input.files) upload(f); input.value = ''; };
  const tools = el('div', { class: 'row wrap filetools' },
    el('button', { type: 'button', class: 'primary small', id: 'cfUpload', text: t('upload'), onclick: () => input.click() }), input,
    el('button', { type: 'button', class: 'small', id: 'cfMkdir', text: t('newFolder'), onclick: mkdir }),
    el('span', { class: 'muted small grow', text: F.quota ? t('spaceUsed', { used: fmtSize(F.used), quota: fmtSize(F.quota) }) : '' }),
    F.uploads ? el('span', { class: 'small', text: t('uploading', { n: F.uploads }) }) : null);
  box.append(crumbs, tools);
  const rows = el('div', { class: 'filelist', id: 'cfList' });
  if (F.folder) rows.append(el('div', { class: 'file dir', onclick: () => list(F.ch, F.path.length > 1 ? F.path[F.path.length - 2].id : 0) }, el('span', { class: 'ficon', text: '↩' }), el('span', { class: 'fname', text: '..' })));
  for (const it of F.items) {
    const name = it.dir
      ? el('button', { type: 'button', class: 'linkish fname', text: it.name, onclick: () => list(F.ch, it.id) })
      : el('a', { class: 'fname', href: api('/cf/' + it.id), download: it.name, text: it.name, title: t('download') });
    const acts = el('span', { class: 'facts' });
    if (mineOrBoss(it)) {
      acts.append(el('button', { type: 'button', class: 'ghost icon', text: '✎', title: t('rename'), onclick: () => rename(it) }),
        el('button', { type: 'button', class: 'ghost icon', text: '🗑', title: t('delete'), onclick: () => del(it) }));
    }
    rows.append(el('div', { class: 'file' + (it.dir ? ' dir' : ''), dataset: { id: it.id } },
      el('span', { class: 'ficon', text: it.dir ? '📁' : icon(it.name) }), name,
      el('span', { class: 'muted small fsize', text: it.dir ? '' : fmtSize(it.size) }),
      el('span', { class: 'muted small fby', text: (it.byName || '') + ' · ' + new Date(it.at).toLocaleDateString() }), acts));
  }
  if (!F.items.length && !F.loading) rows.append(el('p', { class: 'muted empty', text: t('noFiles') }));
  box.append(rows);
}

function icon(n) {
  if (/\.(png|jpe?g|gif|webp|bmp|svg)$/i.test(n)) return '🖼️';
  if (/\.(mp3|ogg|wav|flac|m4a|opus)$/i.test(n)) return '🎵';
  if (/\.(mp4|webm|mkv|mov|avi)$/i.test(n)) return '🎬';
  if (/\.(zip|rar|7z|tar|gz)$/i.test(n)) return '🗜️';
  return '📄';
}

function upload(file) {
  if (F.maxFile && file.size > F.maxFile) { toast(t('uploadFailed', { why: t('tooBig') }), true); return; }
  const ch = F.ch;
  const folder = F.folder;
  const bar = el('div', { class: 'upload' }, el('span', { class: 'fname', text: file.name }), el('progress', { max: 1, value: 0 }));
  $('uploads').append(bar);
  F.uploads++;
  render();
  const xhr = new XMLHttpRequest();
  xhr.open('POST', api('/cf/upload', { ch, folder, name: file.name }));
  xhr.upload.onprogress = (e) => { if (e.lengthComputable) bar.querySelector('progress').value = e.loaded / e.total; };
  const done = () => { bar.remove(); F.uploads--; if (F.ch === ch && F.folder === folder) list(ch, folder); else render(); };
  xhr.onload = () => {
    if (xhr.status !== 200) {
      const why = { 401: t('errAuth'), 404: t('errGone'), 413: t('tooBig'), 507: t('spaceFull') }[xhr.status] || ('HTTP ' + xhr.status);
      toast(t('uploadFailed', { why }), true);
    }
    done();
  };
  xhr.onerror = () => { toast(t('uploadFailed', { why: t('errWs') }), true); done(); };
  xhr.send(file);
}

function ask(title, value, okText, then) {
  const input = el('input', { value: value || '', maxLength: 180 });
  const go = () => { const v = input.value.trim(); if (v) then(v); };
  openDialog({
    title, body: el('form', { class: 'stack', onsubmit: (ev) => { ev.preventDefault(); go(); $('dlg').close(); } }, input),
    actions: [{ text: t('cancel') }, { text: okText, primary: true, onclick: go }],
  });
  setTimeout(() => { input.focus(); input.select(); }, 30);
}
function mkdir() { ask(t('newFolder'), '', t('create'), (name) => send({ t: 'cf-mkdir', ch: F.ch, folder: F.folder, name })); }
function rename(it) { ask(t('rename'), it.name, t('save'), (name) => send({ t: 'cf-rename', id: it.id, name })); }
function del(it) {
  openDialog({
    title: t('delete'), body: el('p', { text: t(it.dir ? 'deleteFolderAsk' : 'deleteFileAsk', { name: it.name }) }),
    actions: [{ text: t('cancel') }, { text: t('delete'), danger: true, onclick: () => send({ t: 'cf-del', id: it.id }) }],
  });
}

function tabs() {
  const show = S.files && !!curChannel();
  $('tabFiles').classList.toggle('hidden', !show);
  const view = $('chat').dataset.view || 'chat';
  $('tabFiles').classList.toggle('on', view === 'files');
  if (view === 'files' && !show) setView('chat');
}

$('tabFiles').onclick = openFiles;
on('cf', (m) => {
  if (m.ch !== F.ch) return;
  Object.assign(F, { folder: m.folder || 0, path: m.path || [], items: m.items || [], used: m.used || 0, quota: m.quota || 0, maxFile: m.maxFile || 0, loading: false });
  render();
});
on('error', (m) => {
  if (!String(m.op || '').startsWith('cf-')) return;
  m.handled = true;
  F.loading = false;
  toast(({ role: t('notYours'), full: t('spaceFull'), bad: t('badName') }[m.why]) || m.message || m.why, true);
  render();
});
on('app:render', tabs);
on('app:view', ({ view }) => { tabs(); if (view === 'files') render(); });
on('app:conv', () => { if ($('chat').dataset.view === 'files') openFiles(); });
on('app:lang', render);
on('app:welcome', () => { F.ch = 0; F.items = []; if ($('chat').dataset.view === 'files') setView('chat'); });
