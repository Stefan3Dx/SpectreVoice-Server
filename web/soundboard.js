// The server's soundboard (soundboard.go), as the apps have it: the server's sounds as tiles (emoji, name, length); a
// click plays one in one's voice channel for everyone there. A play isn't mixed into anyone's microphone: the server
// tells everyone in the channel ({"t":"sb-play"} with the sound's hash) and each app plays its own copy, fetched once
// from /sb/f/<hash>.opus (Ogg Opus, which the browser decodes itself) and kept by its hash. Here: WebAudio, at the
// volume chosen in this browser, unless the server's sounds are muted here or one is deafened.
//
// "soundboard" plays (without it a click only previews the sound here), "soundboard_manage" adds (POST /sb/upload),
// renames and removes. The list comes in the welcome ("sb") and again whenever it changes.

import { S, el, send, on, toast, api, may, me, openDialog, store } from './core.js';
import { t } from './i18n.js';
import { prefs } from './voice.js';

const EMOJI = ['🔊', '📯', '🥁', '👏', '😂', '💀', '🔥', '🎉', '😱', '🐐', '🚨', '🎺', '🤡', '👻', '💥', '🍿'];
const MAX_SECONDS = 10;
const HASH = /^[0-9a-f]{64}$/;

export const SB = {
  known: false, items: [], max: 0, coolUntil: 0, playing: null, error: '', busy: false,
  muted: store.get('sv.sbMuted', '') === '1',
  volume: Math.max(0, Math.min(1, +store.get('sv.sbVolume', '0.7') || 0)),
  played: 0,
};
const buffers = new Map(); // hash -> Promise<AudioBuffer>
let ctx = null;
let gain = null;
let view = null; // the open dialog's body
let editing = null; // a sound (or 'new')
let source = null;

// ====== Playing ======

function audio() {
  if (!ctx) {
    const AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) return null;
    ctx = new AC({ latencyHint: 'interactive' });
    gain = ctx.createGain();
    gain.connect(ctx.destination);
    sink();
  }
  if (ctx.state === 'suspended') ctx.resume().catch(() => {});
  gain.gain.value = SB.volume;
  return ctx;
}
function sink() {
  if (ctx && typeof ctx.setSinkId === 'function') ctx.setSinkId(prefs.outDev || '').catch(() => {});
}
// A gesture lets the page's sound start (autoplay rules): the first one makes the context
for (const ev of ['pointerdown', 'keydown']) document.addEventListener(ev, () => { if (S.phase === 'on') audio(); }, { capture: true, passive: true });

async function sha256(buf) {
  const d = new Uint8Array(await crypto.subtle.digest('SHA-256', buf));
  return Array.from(d, (b) => b.toString(16).padStart(2, '0')).join('');
}

// The sound decoded: fetched once (checked against its hash, like the apps), kept while the page is open
function bufferOf(hash) {
  if (buffers.has(hash)) return buffers.get(hash);
  const p = (async () => {
    const r = await fetch(api('/sb/f/' + hash + '.opus'));
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const data = await r.arrayBuffer();
    if (data.byteLength > 1 << 20 || (await sha256(data)) !== hash) throw new Error('bad sound');
    const c = audio();
    if (!c) throw new Error('no WebAudio');
    return c.decodeAudioData(data);
  })();
  buffers.set(hash, p);
  p.catch(() => buffers.delete(hash));
  if (buffers.size > 48) buffers.delete(buffers.keys().next().value);
  return p;
}

async function playHere(m) {
  try {
    const buf = await bufferOf(m.hash);
    const c = audio();
    if (!c) return;
    if (source) { try { source.stop(); } catch { /* ended */ } }
    const src = c.createBufferSource();
    src.buffer = buf;
    src.connect(gain);
    source = src;
    const p = { id: m.id, emoji: m.emoji || '🔊', name: m.name || '', byName: m.byName || '' };
    SB.playing = p;
    src.onended = () => { if (SB.playing === p) { SB.playing = null; render(); } if (source === src) source = null; };
    src.start();
    SB.played++;
    // What came out, for the page's own checks (e2e): which sound, how long, how loud
    const ch0 = buf.getChannelData(0);
    let peak = 0;
    for (let i = 0; i < ch0.length; i += 16) peak = Math.max(peak, Math.abs(ch0[i]));
    Object.assign(document.body.dataset, { sbPlayed: String(SB.played), sbLast: m.hash, sbSecs: buf.duration.toFixed(2), sbPeak: peak.toFixed(3) });
    render();
  } catch (e) {
    console.warn('soundboard', m.hash, e);
  }
}

// ====== Asking the server ======

function playOnServer(s) {
  const left = SB.coolUntil - Date.now();
  if (left > 0) { showError(errText('cooldown', left)); return; }
  SB.error = '';
  send({ t: 'sb', op: 'play', id: s.id });
}

function errText(why, wait, msg) {
  const k = { role: 'sbErrRole', off: 'sbErrOff', channel: 'sbErrChannel', mute: 'sbErrMute', cooldown: 'sbErrCooldown', rate: 'sbErrRate', busy: 'sbErrBusy',
    gone: 'sbErrGone', full: 'sbErrFull', format: 'sbErrFormat', big: 'sbErrBig', ffmpeg: 'sbErrFfmpeg' }[why];
  return k ? t(k, { s: ((wait || 0) / 1000).toFixed(1) }) : (msg || t('sbErrBad'));
}
function showError(text) {
  SB.error = text;
  if (view?.isConnected) render(); else toast(text, true);
}

async function upload(file, name, emoji) {
  if (file.size > 8 << 20) { showError(errText('big')); return; }
  SB.busy = true;
  SB.error = '';
  render();
  try {
    const r = await fetch(api('/sb/upload', { name: name.trim(), emoji: emoji.trim() }), { method: 'POST', body: file });
    let m = {};
    try { m = await r.json(); } catch { /* not JSON */ }
    if (r.ok) {
      editing = null;
      toast(m.cut ? t('sbCut', { n: MAX_SECONDS }) : t('sbAdded'));
    } else showError(errText(m.why, 0, m.error || 'HTTP ' + r.status));
  } catch (e) {
    showError(errText('', 0, e?.message || String(e)));
  } finally {
    SB.busy = false;
    render();
  }
}

// ====== The dialog ======

const inVoice = () => { const c = S.channels.get(me()?.ch); return !!c && !c.kind; };

function tile(s, mayPlay, manage, cooling) {
  const on = SB.playing?.id === s.id;
  const b = el('button', {
    type: 'button', class: 'sbtile' + (on ? ' on' : '') + (cooling && !on ? ' cool' : ''), dataset: { id: s.id }, title: s.name + (s.byName ? ' · ' + s.byName : ''),
    disabled: !mayPlay && !manage,
    onclick: () => { if (mayPlay) playOnServer(s); else playHere(s); },
    oncontextmenu: (ev) => { if (manage) { ev.preventDefault(); editing = s; render(); } },
  },
  el('span', { class: 'sbemoji', text: s.emoji || '🔊' }),
  el('span', { class: 'sbname', text: s.name }),
  el('span', { class: 'sbmeta' }, el('span', { text: (s.ms / 1000).toFixed(1) + ' s' }),
    manage ? el('span', { class: 'sbedit', text: '✎', title: t('sbEdit'), attrs: { role: 'button', 'aria-label': t('sbEdit') }, onclick: (ev) => { ev.stopPropagation(); editing = s; render(); } }) : null));
  return b;
}

function editor(s) {
  const isNew = s === 'new';
  let file = null;
  let emoji = isNew ? '🔊' : (s.emoji || '🔊');
  let sure = false;
  const name = el('input', { id: 'sbName', maxLength: 32, value: isNew ? '' : s.name, placeholder: t('sbNameLabel') });
  const fileInput = el('input', { type: 'file', id: 'sbFile', class: 'hidden', attrs: { accept: '.ogg,.opus,.mp3,.wav,audio/ogg,audio/mpeg,audio/wav' } });
  const fileNote = el('span', { class: 'muted small grow', text: t('sbFileNote', { n: MAX_SECONDS }) });
  fileInput.onchange = () => {
    file = fileInput.files[0] || null;
    if (!file) return;
    if (!name.value.trim()) name.value = file.name.replace(/\.[^.]+$/, '').slice(0, 32);
    fileNote.textContent = file.name + ' · ' + Math.ceil(file.size / 1024) + ' KB';
    save.disabled = false;
  };
  const other = el('input', { class: 'sbother', maxLength: 8, placeholder: t('sbOther'), value: EMOJI.includes(emoji) ? '' : emoji });
  const picks = el('div', { class: 'sbemojis' });
  const drawPicks = () => {
    picks.textContent = '';
    for (const e of EMOJI) picks.append(el('button', { type: 'button', class: 'sbpick' + (e === emoji ? ' on' : ''), text: e, onclick: () => { emoji = e; other.value = ''; drawPicks(); } }));
    picks.append(other);
  };
  other.oninput = () => { const v = other.value.trim(); if (v) { emoji = v; drawPicks(); other.focus(); } };
  drawPicks();
  const save = el('button', {
    type: 'button', class: 'primary small', id: 'sbSave', text: isNew ? t('sbAddIt') : t('save'), disabled: isNew,
    onclick: () => {
      if (isNew) { if (file) upload(file, name.value, emoji); return; }
      send({ t: 'sb', op: 'edit', id: s.id, name: name.value.trim(), emoji });
      editing = null;
      render();
    },
  });
  const del = isNew ? null : el('button', {
    type: 'button', class: 'danger small', id: 'sbDel', text: t('delete'),
    onclick: () => {
      if (!sure) { sure = true; del.textContent = t('sbSure'); setTimeout(() => { sure = false; if (del.isConnected) del.textContent = t('delete'); }, 4000); return; }
      send({ t: 'sb', op: 'del', id: s.id });
      editing = null;
      render();
    },
  });
  return el('div', { class: 'sbeditor' },
    el('div', { class: 'sbedtitle', text: isNew ? t('sbNew') : t('sbChange', { name: s.name }) }),
    isNew ? el('div', { class: 'row' }, el('button', { type: 'button', class: 'small', id: 'sbPick', text: t('sbChoose'), onclick: () => fileInput.click() }), fileInput, fileNote) : null,
    name, picks,
    el('div', { class: 'row' }, del, el('span', { class: 'grow' }), el('button', { type: 'button', class: 'small', text: t('cancel'), onclick: () => { editing = null; render(); } }), save));
}

function render() {
  if (!view?.isConnected) return;
  const keep = view.querySelector('.sbeditor');
  const mayPlay = may('soundboard');
  const manage = may('soundboard_manage');
  const now = Date.now();
  const cooling = SB.coolUntil > now;
  const head = el('div', { class: 'row sbhead' },
    el('span', { class: 'sbtitle', text: t('sbServer') + (SB.known ? ` — ${SB.items.length}/${SB.max}` : '') }),
    el('span', { class: 'grow' }),
    cooling ? el('span', { class: 'sbbadge', text: t('sbNext', { s: ((SB.coolUntil - now) / 1000).toFixed(1) }) }) : null,
    manage && SB.known && !editing ? el('button', { type: 'button', class: 'primary small', id: 'sbAdd', text: t('sbAdd'), disabled: SB.busy || SB.items.length >= SB.max, onclick: () => { editing = 'new'; render(); } }) : null);
  const out = [el('p', { class: 'sbsub', text: t('sbSubtitle') }), head];
  if (!SB.known) {
    out.push(el('p', { class: 'sbnote warn', text: t('sbNone') }));
  } else {
    if (SB.error) out.push(el('div', { class: 'sberr' }, el('span', { class: 'grow', text: SB.error }), el('button', { type: 'button', class: 'ghost icon', text: '✕', onclick: () => { SB.error = ''; render(); } })));
    if (SB.busy) out.push(el('p', { class: 'sbnote', text: t('sbConverting') }));
    // An editor being typed in stays as it is while the list around it changes
    if (editing) {
      const key = editing === 'new' ? 'new' : String(editing.id);
      let ed = keep && keep.dataset.for === key ? keep : null;
      if (!ed) { ed = editor(editing); ed.dataset.for = key; }
      out.push(ed);
    }
    if (!mayPlay) out.push(el('p', { class: 'sbnote warn', text: t('sbNoRole') }));
    else if (!inVoice()) out.push(el('p', { class: 'sbnote', text: t('sbJoin') }));
    if (!SB.items.length) {
      out.push(el('div', { class: 'sbempty' }, el('div', { text: t('sbEmpty') }), el('div', { class: 'muted small', text: manage ? t('sbEmptyManage', { n: MAX_SECONDS }) : t('sbEmptyAdmins') })));
    }
    out.push(el('div', { class: 'sbgrid', id: 'sbGrid' }, SB.items.map((s) => tile(s, mayPlay, manage, cooling))));
    if (SB.playing) out.push(el('p', { class: 'sbnote accent', id: 'sbPlaying', text: '▶ ' + SB.playing.emoji + ' ' + SB.playing.name + (SB.playing.byName ? ' · ' + t('sbBy', { name: SB.playing.byName }) : '') }));
  }
  const mute = el('input', { type: 'checkbox', id: 'sbMute', checked: SB.muted });
  mute.onchange = () => { SB.muted = mute.checked; store.set('sv.sbMuted', SB.muted ? '1' : ''); if (SB.muted && source) { try { source.stop(); } catch { /* ended */ } } render(); };
  out.push(el('label', { class: 'sbtoggle' }, mute, el('span', {}, el('span', { text: t('sbMute') }), el('span', { class: 'muted small', text: t('sbMuteSub') }))));
  if (!SB.muted) {
    const label = el('span', { class: 'muted small', text: Math.round(SB.volume * 100) + '%' });
    const vol = el('input', { type: 'range', id: 'sbVol', min: 0, max: 100, value: Math.round(SB.volume * 100) });
    vol.oninput = () => { SB.volume = vol.value / 100; label.textContent = vol.value + '%'; if (gain) gain.gain.value = SB.volume; };
    vol.onchange = () => store.set('sv.sbVolume', String(SB.volume));
    out.push(el('div', { class: 'sbslider' }, el('div', { class: 'row' }, el('span', { class: 'small grow', text: t('sbVolume') }), label), vol));
  }
  view.replaceChildren(...out);
}

let coolTimer = null;
function tickCool() {
  clearInterval(coolTimer);
  coolTimer = setInterval(() => {
    if (!view?.isConnected || Date.now() >= SB.coolUntil) { clearInterval(coolTimer); }
    render();
  }, 200);
}

export function openSoundboard() {
  view = el('div', { class: 'sb', id: 'sbView' });
  editing = null;
  SB.error = '';
  audio();
  openDialog({ title: 'Soundboard', body: view, wide: true, cls: 'sbdlg', actions: [{ text: t('close'), primary: true }], onclose: () => { view = null; } });
  render();
  if (SB.coolUntil > Date.now()) tickCool();
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
    button = el('button', { id: 'sbOpen', type: 'button', class: 'ghost icon sbbtn', text: '🔊', title: 'Soundboard', attrs: { 'aria-label': 'Soundboard' }, onclick: openSoundboard });
    toolsBox().append(button);
  }
  button.classList.toggle('hidden', !on);
  button.classList.toggle('playing', !!SB.playing);
}

// ====== From the server ======

function setList(m) {
  if (!m || !Array.isArray(m.items)) { SB.known = false; SB.items = []; SB.max = 0; return; }
  SB.known = true;
  SB.items = m.items.filter((x) => x && HASH.test(x.hash || '') && typeof x.id === 'number').slice(0, 500);
  SB.max = m.max || 0;
}

on('app:welcome', (m) => {
  setList(m.sb);
  SB.coolUntil = 0;
  SB.playing = null;
  editing = null;
  showButton(SB.known);
  render();
});
on('sb', (m) => {
  setList(m);
  if (editing && editing !== 'new' && !SB.items.some((x) => x.id === editing.id)) editing = null;
  showButton(SB.known);
  render();
});
on('sb-play', (m) => {
  if (!HASH.test(m.hash || '')) return;
  if (m.by === S.uid) { SB.coolUntil = Date.now() + 3000; tickCool(); }
  if (SB.muted || S.snd) { render(); return; }
  playHere(m);
});
on('error', (m) => {
  if (m.op !== 'sb') return;
  m.handled = true;
  SB.busy = false;
  if (m.why === 'cooldown' && m.wait > 0) { SB.coolUntil = Date.now() + m.wait; tickCool(); }
  showError(errText(m.why, m.wait, m.message));
});
on('done', (m) => { if (m.op === 'sb') { SB.busy = false; SB.error = ''; render(); } });
on('app:down', (d) => { if (!d?.retry) { SB.known = false; showButton(false); } if (source) { try { source.stop(); } catch { /* ended */ } } });
on('app:perms', render);
on('app:render', () => { if (view?.isConnected && !view.querySelector('.sbeditor')) render(); });
on('app:lang', render);
on('app:sink', sink);
on('app:selfstate', () => { if (S.snd && source) { try { source.stop(); } catch { /* ended */ } } });
