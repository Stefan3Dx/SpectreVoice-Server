// The server's music bot (bot.go), as the apps have it: what plays, the queue, adding a song (a name or a link) and
// the buttons (pause/resume, skip, to my channel, stop, the volume from the next song). The bot is a member of a
// voice channel like a person: its music comes in as its voice (voice.js), nothing to play here. The same commands
// work in the chat (!play …). The server decides who may steer it ("music", or whoever asked for the song playing,
// or a vote of half the channel to skip).
//
//	-> {"t":"music","cmd":"play"|"skip"|"pause"|"resume"|"stop"|"join"|"vol"|"queue","q":…}
//	<- {"t":"music","sid","ch","now":SONG|null,"queue":[SONG…],"paused","volume"}   ({"t":"music"}: no bot)
//	SONG = {"title","page"?,"by","secs"?}

import { S, el, send, on, toast, me, openDialog } from './core.js';
import { t } from './i18n.js';

export const M = { available: false, st: null, limit: 0 };
let view = null;
let input = null;

export function music(cmd, q) {
  if (!send(q !== undefined ? { t: 'music', cmd, q: String(q) } : { t: 'music', cmd })) toast(t('muOffline'), true);
}

const mmss = (s) => (s > 0 ? Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0') : '');

function add() {
  const q = input.value.trim();
  if (!q) return;
  music('play', q);
  input.value = '';
  addBtn.disabled = true;
  toast(t('muLooking', { q: q.length > 60 ? q.slice(0, 60) + '…' : q }));
}
let addBtn = null;

let status = null;
function render() {
  if (!view?.isConnected) return;
  if (!M.available) {
    view.replaceChildren(el('p', { class: 'sbnote warn', text: t('muNone') }));
    input = null;
    return;
  }
  // The field stays the same element (typing goes on while the status below it changes)
  if (!input || !input.isConnected) {
    input = el('input', { id: 'muQ', maxLength: 300, autocomplete: 'off' });
    input.oninput = () => { addBtn.disabled = !input.value.trim(); };
    addBtn = el('button', { type: 'button', class: 'primary', id: 'muAdd', disabled: true, onclick: add });
    status = el('div', { class: 'mustatus' });
    view.replaceChildren(el('p', { class: 'sbsub', text: t('muSubtitle') }), el('form', { class: 'row muadd', onsubmit: (ev) => { ev.preventDefault(); add(); } }, input, addBtn), status);
  }
  input.placeholder = t('muField');
  addBtn.textContent = t('muAddBtn');
  const out = [];
  const m = M.st;
  out.push(el('div', { class: 'sbtitle', text: t('muNow') }));
  const now = m?.now;
  if (!now) out.push(el('p', { class: 'muted', id: 'muNow', text: t('muNothing') }));
  else {
    const where = S.channels.get(m.ch);
    out.push(el('div', { class: 'munow', id: 'muNow' },
      el('div', { class: 'mutitle', text: now.title || '' }),
      el('div', { class: 'muted small', text: [now.by, mmss(now.secs), m.paused ? t('muPaused') : '', where ? '#' + where.name : ''].filter(Boolean).join(' · ') })));
  }
  if (m) {
    out.push(el('div', { class: 'row wrap mubtns' },
      el('button', { type: 'button', class: 'small', id: 'muPause', text: m.paused ? t('muResume') : t('muPause'), onclick: () => music(m.paused ? 'resume' : 'pause') }),
      el('button', { type: 'button', class: 'small', id: 'muSkip', text: 'Skip', onclick: () => music('skip') }),
      m.ch !== me()?.ch ? el('button', { type: 'button', class: 'small', id: 'muJoin', text: t('muToMine'), onclick: () => music('join') }) : null,
      el('button', { type: 'button', class: 'danger small', id: 'muStop', text: 'Stop', onclick: () => music('stop') })));
    const label = el('span', { class: 'muted small', text: m.volume + '%' });
    const vol = el('input', { type: 'range', id: 'muVol', min: 1, max: 100, value: m.volume });
    vol.oninput = () => { label.textContent = vol.value + '%'; };
    vol.onchange = () => music('vol', vol.value);
    out.push(el('div', { class: 'sbslider' }, el('div', { class: 'row' }, el('span', { class: 'small grow', text: t('muVolume') }), label), vol));
  }
  if (m?.queue?.length) {
    out.push(el('div', { class: 'sbtitle', text: t('muQueue') + ' · ' + m.queue.length + (M.limit ? ' / ' + M.limit : '') }));
    out.push(el('ol', { class: 'muqueue', id: 'muQueue' }, m.queue.map((s) => el('li', {}, el('span', { class: 'grow mutrack', text: s.title || '' }), el('span', { class: 'muted small', text: [s.by, mmss(s.secs)].filter(Boolean).join(' · ') })))));
  }
  status.replaceChildren(...out);
}

export function openMusic() {
  view = el('div', { class: 'mu', id: 'muView' });
  input = null;
  openDialog({ title: t('muTitle'), body: view, cls: 'mudlg', actions: [{ text: t('close'), primary: true }], onclose: () => { view = null; } });
  render();
  if (M.available) music('queue');
  setTimeout(() => input?.focus(), 30);
}

// ====== The button in the top bar ======

function toolsBox() {
  let box = document.getElementById('moduleTools');
  if (!box) {
    const bar = document.querySelector('.top .tools');
    box = el('div', { id: 'moduleTools', class: 'moduletools' });
    if (bar) bar.prepend(box); else document.body.prepend(box);
  }
  return box;
}
let button = null;
function showButton(on) {
  if (!button) {
    button = el('button', { id: 'muOpen', type: 'button', class: 'ghost icon mubtn', text: '🎵', title: t('muTitle'), attrs: { 'aria-label': t('muTitle') }, onclick: openMusic });
    toolsBox().append(button);
  }
  button.title = t('muTitle');
  button.classList.toggle('hidden', !on);
  button.classList.toggle('playing', !!M.st?.now && !M.st?.paused);
}

// ====== From the server ======

const ERR = { here: 'muErrHere', full: 'muErrFull', busy: 'muErrBusy', gone: 'muErrGone', role: 'muErrRole', off: 'muNone' };

on('app:welcome', (m) => {
  M.available = !!m.server?.music;
  M.limit = m.tier?.limits?.musicQueue || 0;
  M.st = null;
  showButton(M.available);
  render();
});
on('music', (m) => {
  M.st = typeof m.sid === 'number' ? m : null;
  if (addBtn && input) addBtn.disabled = !input.value.trim();
  showButton(M.available);
  render();
});
on('slots', (m) => { if (m.limits?.musicQueue) { M.limit = m.limits.musicQueue; render(); } });
on('member-left', (m) => { if (M.st && m.sid === M.st.sid) { M.st = null; showButton(M.available); render(); } });
on('error', (m) => {
  if (m.op !== 'music') return;
  m.handled = true;
  if (addBtn && input) addBtn.disabled = !input.value.trim();
  const k = ERR[m.why];
  toast(k ? t(k) : (m.message || m.why || 'error'), true);
});
on('app:down', (d) => { if (!d?.retry) { M.available = false; M.st = null; showButton(false); } });
on('app:render', render);
on('app:lang', () => { if (button) showButton(M.available); render(); });
