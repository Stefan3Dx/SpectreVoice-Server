// Streams: watching screen and camera shares (and the server's TV, room "tv:main"), and sharing the screen from here.
//
// Watching, as the apps do:
//   through the server: /streams?token=… {"type":"subscribe","room","dev"} → the server offers, we answer; with a
//   simulcast share {"type":"layer","q":"auto"|"h"|"m"|"l"} picks the size, the server says which it sends.
//   "dev" is this browser's device id: a new watch replaces our own old one instead of being refused.
//   peer to peer (a stream live with "mode":"p2p"): p2p-join to the streamer's session, it offers (p2p-offer), we
//   answer (p2p-answer), candidates both ways (p2p-ice), p2p-bye to leave; "moved" (or the live list) means the
//   stream goes the other way now: we follow it. The badge says how it comes: via the server, P2P or P2P relay.
// Sharing: getDisplayMedia (with the tab's or system's sound when the browser offers it), the "Server + P2P" route:
//   published once to the SFU (room = our uid); when the server can't take it (refused, or no connection in 12 s)
//   it goes peer to peer: p2p-live, then a connection per viewer that joins (at most SHARE_P2P_MAX).

import { $, S, el, send, on, emit, toast, api, wsUrl, rtcConfig, memberByUid, may } from './core.js';
import { t, errText } from './i18n.js';
import { applySink } from './voice.js';

export const TV_ROOM = 'tv:main';
const UP_TIMEOUT = 12000;
const P2P_OFFER_TIMEOUT = 20000;
const NO_PICTURE = 9000;
const SHARE_P2P_MAX = 4;
const SHARE_KBPS = 2500;

// The streams live now by someone (the TV isn't anyone's)
export const liveOf = (uid) => [...S.live.values()].filter((l) => l.uid === uid && !l.room.startsWith('tv:'));
const nameOf = (l) => memberByUid(l.uid)?.name || l.uid?.slice(0, 8) || '?';

// ====== Watching ======

const W = new Map(); // room → watch

export function watching(room) { return W.has(room); }

// watchRoom(room, {name}) starts (or shows) a watch
export function watchRoom(room, opts = {}) {
  if (!S.token) return;
  let w = W.get(room);
  if (w) {
    if (w.ended) restart(w); else w.tile.scrollIntoView({ block: 'nearest' });
    return;
  }
  const l = S.live.get(room);
  w = { room, name: opts.name || (l ? nameOf(l) : room), title: l?.title || '', gen: 0, timers: [], muted: false, volume: 1, layers: [], layer: '', asked: 'auto', via: '', resub: 0 };
  W.set(room, w);
  makeTile(w);
  start(w);
  layoutStage();
  emit('app:watching', { room, on: true });
}

function start(w) {
  w.ended = false;
  w.up = false;
  const l = S.live.get(w.room);
  if (l && l.mode === 'p2p' && w.room !== TV_ROOM) watchP2p(w, l.sid);
  else watchSfu(w);
}
function restart(w) {
  closeConn(w, true);
  w.resub = 0;
  start(w);
}

function later(w, ms, f) {
  const gen = w.gen;
  w.timers.push(setTimeout(() => { if (w.gen === gen && W.get(w.room) === w) f(); }, ms));
}

function gotTrack(w, track) {
  if (!w.stream) w.stream = new MediaStream();
  for (const old of w.stream.getTracks()) if (old.kind === track.kind) w.stream.removeTrack(old);
  w.stream.addTrack(track);
  if (w.video.srcObject !== w.stream) w.video.srcObject = w.stream;
  applySink(w.video);
  applyVolume(w);
  w.video.play().catch(() => { tileMsg(w, t('clickToPlay'), [{ text: '▶', onclick: () => { w.video.play(); tileMsg(w, ''); } }]); });
}

// Codecs the browser can't decode (an old browser and the TV's H.264, say): said, instead of a black tile
function decodable(sdp) {
  const caps = new Set((RTCRtpReceiver.getCapabilities?.('video')?.codecs || []).map((c) => c.mimeType.toLowerCase()));
  if (!caps.size) return true;
  const video = sdp.split(/\r?\nm=/).find((s) => s.startsWith('video'));
  if (!video) return true;
  const names = [...video.matchAll(/a=rtpmap:\d+ ([\w-]+)\/90000/g)].map((m) => m[1].toLowerCase()).filter((n) => !['rtx', 'red', 'ulpfec', 'flexfec-03'].includes(n));
  return !names.length || names.some((n) => caps.has('video/' + n));
}

function watchSfu(w) {
  const gen = ++w.gen;
  w.mode = 'sfu';
  setVia(w, 'server');
  tileMsg(w, t('connecting'));
  const ws = new WebSocket(wsUrl('/streams?token=' + encodeURIComponent(S.token)));
  const pc = new RTCPeerConnection(rtcConfig());
  w.ws = ws;
  w.pc = pc;
  const pending = [];
  const out = (m) => { if (ws.readyState === 1) ws.send(JSON.stringify(m)); };
  w.sendLayer = (q) => out({ type: 'layer', q });
  pc.onicecandidate = (ev) => { if (ev.candidate) out({ type: 'candidate', candidate: ev.candidate.candidate, mid: ev.candidate.sdpMid, mline: ev.candidate.sdpMLineIndex }); };
  pc.ontrack = (ev) => { if (w.gen === gen) gotTrack(w, ev.track); };
  pc.onconnectionstatechange = () => {
    if (w.gen !== gen) return;
    if (pc.connectionState === 'connected') { w.up = true; tileMsg(w, ''); }
    if (pc.connectionState === 'failed') ended(w, gen, t('streamFailed'));
  };
  ws.onopen = () => out({ type: 'subscribe', room: w.room, dev: S.dev });
  ws.onmessage = async (ev) => {
    if (w.gen !== gen) return;
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    try {
      switch (m.type) {
        case 'offer': {
          if (!decodable(m.sdp)) { finish(w, t('codecUnsupported')); return; }
          await pc.setRemoteDescription({ type: 'offer', sdp: m.sdp });
          for (const c of pending.splice(0)) await pc.addIceCandidate(c).catch(() => {});
          await pc.setLocalDescription(await pc.createAnswer());
          out({ type: 'answer', sdp: pc.localDescription.sdp });
          break;
        }
        case 'candidate': {
          const c = { candidate: m.candidate, sdpMid: m.mid ?? null, sdpMLineIndex: m.mline ?? null };
          if (pc.remoteDescription) await pc.addIceCandidate(c).catch(() => {}); else pending.push(c);
          break;
        }
        case 'layer':
          w.layer = m.layer || '';
          w.asked = m.q || 'auto';
          w.layers = Array.isArray(m.layers) ? m.layers : [];
          updateBar(w);
          break;
        case 'ended': ended(w, gen, null); break;
        case 'error': ended(w, gen, m.message || t('streamFailed')); break;
      }
    } catch (e) {
      console.warn('stream', e);
      ended(w, gen, String(e?.message || e));
    }
  };
  ws.onclose = () => { if (w.gen === gen && !w.ended) ended(w, gen, w.up ? null : t('streamNoConn')); };
  later(w, UP_TIMEOUT, () => { if (!w.up && !w.ended) ended(w, gen, t('streamUdp')); });
  // No picture yet (the share's key frame got lost on the way): subscribed again, twice at most
  later(w, NO_PICTURE, () => {
    if (w.ended || w.video.videoWidth > 0 || w.resub >= 2) return;
    w.resub++;
    closeConn(w, false);
    watchSfu(w);
  });
}

function ended(w, gen, why) {
  if (w.gen !== gen || w.ended) return;
  // The streamer went over to P2P (or came back on a new session): follow it
  const l = S.live.get(w.room);
  if (why === null && l && w.room !== TV_ROOM && (l.mode === 'p2p') !== (w.mode === 'p2p')) { follow(w); return; }
  if (why === null && l && w.mode === 'p2p' && l.mode === 'p2p' && l.sid !== w.sid) { follow(w); return; }
  finish(w, why ?? (w.room === TV_ROOM ? t('tvStopped') : t('streamEnded')));
}

function follow(w) {
  const l = S.live.get(w.room);
  if (!l) { finish(w, t('streamEnded')); return; }
  closeConn(w, false);
  start(w);
}

function watchP2p(w, sid) {
  const gen = ++w.gen;
  w.mode = 'p2p';
  w.sid = sid;
  w.offered = false;
  w.layers = [];
  setVia(w, 'p2p');
  tileMsg(w, t('connectingP2p'));
  const pc = new RTCPeerConnection(rtcConfig());
  w.pc = pc;
  w.pending = [];
  pc.onicecandidate = (ev) => {
    if (ev.candidate && w.gen === gen) send({ t: 'p2p-ice', sid, room: w.room, candidate: ev.candidate.candidate, mid: ev.candidate.sdpMid, mline: ev.candidate.sdpMLineIndex });
  };
  pc.ontrack = (ev) => { if (w.gen === gen) gotTrack(w, ev.track); };
  pc.onconnectionstatechange = async () => {
    if (w.gen !== gen) return;
    if (pc.connectionState === 'connected') {
      w.up = true;
      tileMsg(w, '');
      setVia(w, await viaOf(pc));
    }
    if (pc.connectionState === 'failed') finish(w, t('p2pFailed'));
  };
  send({ t: 'p2p-join', sid, room: w.room });
  later(w, P2P_OFFER_TIMEOUT, () => { if (!w.offered && !w.ended) finish(w, t('p2pNoAnswer')); });
}

// How a connected peer connection carries its media: "relay" through a TURN server, else "p2p"
async function viaOf(pc) {
  try {
    const r = await pc.getStats();
    let pair = null;
    r.forEach((s) => { if (s.type === 'transport' && s.selectedCandidatePairId) pair = r.get(s.selectedCandidatePairId); });
    if (!pair) r.forEach((s) => { if (!pair && s.type === 'candidate-pair' && s.nominated && s.state === 'succeeded') pair = s; });
    if (!pair) return 'p2p';
    return [pair.localCandidateId, pair.remoteCandidateId].some((id) => r.get(id)?.candidateType === 'relay') ? 'relay' : 'p2p';
  } catch { return 'p2p'; }
}

// A p2p-* message from a streamer we watch
async function viewerSignal(m) {
  const w = [...W.values()].find((x) => x.mode === 'p2p' && x.room === m.room && x.sid === m.from && !x.ended);
  if (!w) return;
  const pc = w.pc;
  const gen = w.gen;
  try {
    switch (m.t) {
      case 'p2p-offer':
        w.offered = true;
        if (!decodable(m.sdp)) { finish(w, t('codecUnsupported')); return; }
        await pc.setRemoteDescription({ type: 'offer', sdp: m.sdp });
        for (const c of w.pending.splice(0)) await pc.addIceCandidate(c).catch(() => {});
        await pc.setLocalDescription(await pc.createAnswer());
        if (w.gen === gen) send({ t: 'p2p-answer', sid: w.sid, room: w.room, sdp: pc.localDescription.sdp });
        break;
      case 'p2p-ice': {
        const c = { candidate: m.candidate, sdpMid: m.mid ?? null, sdpMLineIndex: m.mline ?? null };
        if (pc.remoteDescription) await pc.addIceCandidate(c).catch(() => {}); else w.pending.push(c);
        break;
      }
      case 'p2p-bye':
        w.told = true;
        if (m.why === 'full') finish(w, t('p2pFull', { name: w.name }));
        else if (m.why === 'moved') follow(w);
        else if (m.why === 'ended' || m.why === 'left') {
          // The streamer's session ended; it may be live again on a new one (the live list says)
          const l = S.live.get(w.room);
          if (l && (l.mode !== 'p2p' || l.sid !== w.sid)) follow(w); else finish(w, t('streamEnded'));
        } else finish(w, t('p2pStopped'));
        break;
    }
  } catch (e) {
    console.warn('p2p watch', e);
    finish(w, String(e?.message || e));
  }
}

// The connection of a watch goes (the tile stays); [tell]: the P2P streamer is told we left
function closeConn(w, tell) {
  w.gen++;
  for (const x of w.timers.splice(0)) clearTimeout(x);
  if (w.mode === 'p2p' && tell && !w.told && w.sid) send({ t: 'p2p-bye', sid: w.sid, room: w.room, why: 'stop' });
  w.told = false;
  try { w.ws?.close(); } catch { /* gone */ }
  try { w.pc?.close(); } catch { /* gone */ }
  w.ws = null;
  w.pc = null;
  w.up = false;
  w.sendLayer = null;
  if (w.stream) for (const tr of w.stream.getTracks()) w.stream.removeTrack(tr);
}

// A watch that ended: its tile says why, with Watch again and Close
function finish(w, why) {
  closeConn(w, true);
  w.ended = true;
  w.endedAt = Date.now();
  w.video.srcObject = null;
  w.stream = null;
  const again = w.room === TV_ROOM || S.live.has(w.room);
  tileMsg(w, why || t('streamEnded'), [again ? { text: t('watchAgain'), onclick: () => restart(w) } : null, { text: t('close'), onclick: () => stopWatching(w.room) }]);
  w.tile.classList.add('ended');
  emit('app:dirty');
}

// Another name on a watch's tile (the TV's channel changed)
export function renameWatch(room, name) {
  const w = W.get(room);
  if (w) { w.name = name; updateBar(w); }
}
// A watch that ended starts again (the TV came back on)
export function rewatch(room) {
  const w = W.get(room);
  if (w && w.ended) restart(w);
}

export function stopWatching(room) {
  const w = W.get(room);
  if (!w) return;
  closeConn(w, true);
  w.tile.remove();
  W.delete(room);
  layoutStage();
  emit('app:watching', { room, on: false });
  emit('app:dirty');
}

// ====== The tiles ======

const QUALITY = [['auto', 'qAuto'], ['h', 'qHigh'], ['m', 'qMedium'], ['l', 'qLow']];

function makeTile(w) {
  const video = el('video', { autoplay: true, playsInline: true, attrs: { playsinline: true } });
  const msg = el('div', { class: 'tile-msg hidden' });
  const name = el('span', { class: 'tname', text: w.name });
  const via = el('span', { class: 'via' });
  const res = el('span', { class: 'res muted' });
  const q = el('select', { class: 'q hidden', title: t('quality'), attrs: { 'aria-label': t('quality') } });
  q.onchange = () => { w.asked = q.value; w.sendLayer?.(q.value); updateBar(w); };
  const vol = el('input', { type: 'range', class: 'tvol', min: 0, max: 100, value: 100, title: t('volume'), attrs: { 'aria-label': t('volume') } });
  vol.oninput = () => { w.volume = vol.value / 100; w.muted = false; applyVolume(w); };
  const mute = el('button', { class: 'ghost icon tmute', type: 'button', text: '🔊', title: t('mute'), onclick: () => { w.muted = !w.muted; applyVolume(w); } });
  const full = el('button', { class: 'ghost icon', type: 'button', text: '⛶', title: t('fullscreen'), onclick: () => { if (document.fullscreenElement) document.exitFullscreen(); else w.tile.requestFullscreen?.().catch(() => {}); } });
  const close = el('button', { class: 'ghost icon tclose', type: 'button', text: '✕', title: t('stopWatching'), onclick: () => stopWatching(w.room) });
  const bar = el('div', { class: 'tile-bar' }, el('div', { class: 'tinfo' }, name, via, res), el('div', { class: 'tctl' }, q, mute, vol, full, close));
  w.tile = el('div', { class: 'tile', dataset: { room: w.room } }, video, msg, bar);
  w.video = video;
  w.els = { msg, name, via, res, q, vol, mute };
  video.addEventListener('resize', () => updateBar(w));
  video.addEventListener('loadeddata', () => { tileMsg(w, ''); updateBar(w); });
  video.ondblclick = () => full.onclick();
  $('tiles').append(w.tile);
  updateBar(w);
}

function tileMsg(w, text, buttons) {
  const m = w.els.msg;
  m.textContent = '';
  m.classList.toggle('hidden', !text);
  w.tile.classList.remove('ended');
  if (!text) return;
  m.append(el('div', { text }));
  const row = el('div', { class: 'row' });
  for (const b of buttons || []) if (b) row.append(el('button', { type: 'button', class: 'small', text: b.text, onclick: b.onclick }));
  if (row.childElementCount) m.append(row);
}

function setVia(w, via) {
  w.via = via;
  updateBar(w);
}

function updateBar(w) {
  const { via, res, q, name } = w.els;
  name.textContent = w.name + (w.title ? ' · ' + w.title : '');
  via.textContent = { server: t('viaServer'), p2p: t('viaP2p'), relay: t('viaRelay') }[w.via] || '';
  via.className = 'via via-' + (w.via || 'none');
  via.dataset.via = w.via || '';
  res.textContent = w.video.videoWidth ? w.video.videoWidth + '×' + w.video.videoHeight : '';
  // The quality picker: only for a simulcast share through the server (it has the layers)
  const layered = w.mode === 'sfu' && w.layers.length > 1;
  q.classList.toggle('hidden', !layered);
  if (layered) {
    q.textContent = '';
    for (const [v, k] of QUALITY) {
      if (v !== 'auto' && !w.layers.includes(v)) continue;
      let label = t(k);
      if (v === 'auto' && w.layer) label += ' (' + t({ h: 'qHigh', m: 'qMedium', l: 'qLow' }[w.layer] || w.layer) + ')';
      q.append(el('option', { value: v, text: label, selected: v === w.asked }));
    }
  }
  w.tile.dataset.layer = w.layer || '';
}

function applyVolume(w) {
  w.video.volume = Math.max(0, Math.min(1, w.volume));
  w.video.muted = !!w.muted || S.snd || !!w.self;
  if (w.els) {
    w.els.mute.textContent = w.video.muted ? '🔇' : '🔊';
    w.els.vol.value = Math.round(w.volume * 100);
  }
}

function layoutStage() {
  const n = W.size + (SH.preview ? 1 : 0);
  $('stage').classList.toggle('hidden', n === 0);
  $('tiles').dataset.n = String(Math.min(n, 4));
  const mob = document.querySelector('.mobile-tabs [data-pane="stage"]');
  if (mob) mob.dataset.count = n || '';
  document.body.classList.toggle('has-stage', n > 0);
}

// ====== The live list (sidebar) ======

function renderLive() {
  const box = $('liveList');
  box.textContent = '';
  const items = [...S.live.values()].filter((l) => !l.room.startsWith('tv:')).sort((a, b) => a.since - b.since);
  $('liveBox').classList.toggle('hidden', !items.length);
  for (const l of items) {
    const mine = l.uid === S.uid;
    const w = W.get(l.room);
    box.append(el('div', { class: 'liveitem' + (w ? ' on' : ''), dataset: { room: l.room } },
      el('span', { class: 'livedot' }),
      el('div', { class: 'livetext' }, el('div', { class: 'lname', text: nameOf(l) + (mine ? ' (' + t('you') + ')' : '') }), el('div', { class: 'muted small', text: (l.title || (l.room.endsWith(':camera') ? t('camera') : t('screen'))) + (l.mode === 'p2p' ? ' · P2P' : '') })),
      mine ? null : el('button', { type: 'button', class: w && !w.ended ? 'small' : 'small primary', text: w && !w.ended ? t('stopWatching') : t('watch'), onclick: () => { if (w && !w.ended) stopWatching(l.room); else { watchRoom(l.room); emit('app:showPane', 'stage'); } } })));
  }
}

// ====== Sharing the screen ======

const SH = { on: false, stream: null, ws: null, pc: null, mode: '', up: false, gen: 0, timers: [], viewers: new Map(), poll: null, count: 0, preview: null };

export async function shareStart() {
  if (SH.on) return;
  if (!navigator.mediaDevices?.getDisplayMedia) { toast(t('shareUnsupported'), true); return; }
  if (!may('stream')) { toast(t('errRole'), true); return; }
  let stream;
  try {
    stream = await navigator.mediaDevices.getDisplayMedia({
      video: { frameRate: { ideal: 30, max: 30 }, width: { max: 1920 }, height: { max: 1080 } },
      audio: true, systemAudio: 'include', surfaceSwitching: 'include', selfBrowserSurface: 'exclude',
    });
  } catch (e) {
    if (e?.name !== 'NotAllowedError' && e?.name !== 'AbortError') toast(t('shareFailed', { why: e?.message || e }), true);
    return;
  }
  const video = stream.getVideoTracks()[0];
  if (!video) { stream.getTracks().forEach((x) => x.stop()); return; }
  try { video.contentHint = 'detail'; } catch { /* old browser */ }
  video.onended = () => shareStop();
  Object.assign(SH, { on: true, stream, mode: '', up: false, count: 0 });
  SH.room = S.uid;
  showPreview();
  sharePublish();
  updateShareUi();
}

function sharePublish() {
  const gen = ++SH.gen;
  SH.mode = 'sfu';
  SH.up = false;
  const ws = new WebSocket(wsUrl('/streams?token=' + encodeURIComponent(S.token)));
  const pc = new RTCPeerConnection(rtcConfig());
  SH.ws = ws;
  SH.pc = pc;
  const out = (m) => { if (ws.readyState === 1) ws.send(JSON.stringify(m)); };
  const pending = [];
  pc.onicecandidate = (ev) => { if (ev.candidate) out({ type: 'candidate', candidate: ev.candidate.candidate, mid: ev.candidate.sdpMid, mline: ev.candidate.sdpMLineIndex }); };
  pc.onconnectionstatechange = () => {
    if (SH.gen !== gen) return;
    if (pc.connectionState === 'connected') { SH.up = true; updateShareUi(); pollViewers(); }
    if (pc.connectionState === 'failed') toP2p(gen, 'failed');
  };
  for (const tr of SH.stream.getTracks()) {
    const init = { direction: 'sendonly', streams: [SH.stream] };
    if (tr.kind === 'video') init.sendEncodings = [{ maxBitrate: SHARE_KBPS * 1000, maxFramerate: 30 }];
    pc.addTransceiver(tr, init);
  }
  ws.onopen = async () => {
    try {
      await pc.setLocalDescription(await pc.createOffer());
      if (SH.gen !== gen) return;
      out({ type: 'publish', room: SH.room, sdp: pc.localDescription.sdp, title: '' });
    } catch (e) { toP2p(gen, String(e?.message || e)); }
  };
  ws.onmessage = async (ev) => {
    if (SH.gen !== gen) return;
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    try {
      if (m.type === 'answer') {
        await pc.setRemoteDescription({ type: 'answer', sdp: m.sdp });
        for (const c of pending.splice(0)) await pc.addIceCandidate(c).catch(() => {});
      } else if (m.type === 'candidate') {
        const c = { candidate: m.candidate, sdpMid: m.mid ?? null, sdpMLineIndex: m.mline ?? null };
        if (pc.remoteDescription) await pc.addIceCandidate(c).catch(() => {}); else pending.push(c);
      } else if (m.type === 'error') {
        const msg = m.message || '';
        // Not ours to stream: no other way helps
        if (/not allowed|only stream as yourself|two streams|already live/.test(msg)) { toast(t('shareRefused', { why: msg }), true); shareStop(); return; }
        toP2p(gen, msg);
      }
    } catch (e) { toP2p(gen, String(e?.message || e)); }
  };
  ws.onclose = () => { if (SH.gen === gen && SH.on && SH.mode === 'sfu') toP2p(gen, 'closed'); };
  SH.timers.push(setTimeout(() => { if (SH.gen === gen && SH.on && !SH.up) toP2p(gen, 'timeout'); }, UP_TIMEOUT));
}

// The server can't carry it: peer to peer from here (announced first, so viewers on the server follow)
function toP2p(gen, why) {
  if (SH.gen !== gen || !SH.on || SH.mode === 'p2p') return;
  console.info('share: peer to peer (' + why + ')');
  SH.gen++;
  SH.mode = 'p2p';
  send({ t: 'p2p-live', room: SH.room, on: true, title: '' });
  closeShareSfu();
  toast(t('shareP2p'));
  updateShareUi();
}

function closeShareSfu() {
  clearInterval(SH.poll);
  SH.poll = null;
  try { SH.ws?.close(); } catch { /* gone */ }
  try { SH.pc?.close(); } catch { /* gone */ }
  SH.ws = null;
  SH.pc = null;
  SH.up = false;
}

function pollViewers() {
  clearInterval(SH.poll);
  const tick = async () => {
    if (!SH.on || SH.mode !== 'sfu') return;
    try {
      const r = await fetch(api('/streams/status', { room: SH.room }));
      if (r.ok) { const st = await r.json(); SH.count = st.viewers || 0; updateShareUi(); }
    } catch { /* next time */ }
  };
  tick();
  SH.poll = setInterval(tick, 5000);
}

// A p2p-* message from one of our viewers
async function shareSignal(m) {
  const from = m.from;
  try {
    switch (m.t) {
      case 'p2p-join': {
        dropViewer(from, false);
        if (SH.viewers.size >= SHARE_P2P_MAX) { send({ t: 'p2p-bye', sid: from, room: SH.room, why: 'full' }); return; }
        const pc = new RTCPeerConnection(rtcConfig());
        const v = { pc, pending: [], answered: false };
        SH.viewers.set(from, v);
        pc.onicecandidate = (ev) => { if (ev.candidate && SH.viewers.get(from) === v) send({ t: 'p2p-ice', sid: from, room: SH.room, candidate: ev.candidate.candidate, mid: ev.candidate.sdpMid, mline: ev.candidate.sdpMLineIndex }); };
        pc.onconnectionstatechange = () => { if (pc.connectionState === 'failed') setTimeout(() => { if (SH.viewers.get(from) === v && pc.connectionState === 'failed') dropViewer(from, true); }, 15000); };
        const each = Math.max(500, Math.round(SHARE_KBPS / Math.max(1, SH.viewers.size)));
        for (const tr of SH.stream.getTracks()) {
          const init = { direction: 'sendonly', streams: [SH.stream] };
          if (tr.kind === 'video') init.sendEncodings = [{ maxBitrate: each * 1000, maxFramerate: 30 }];
          pc.addTransceiver(tr, init);
        }
        await pc.setLocalDescription(await pc.createOffer());
        if (SH.viewers.get(from) === v) send({ t: 'p2p-offer', sid: from, room: SH.room, sdp: pc.localDescription.sdp });
        SH.count = SH.viewers.size;
        updateShareUi();
        break;
      }
      case 'p2p-answer': {
        const v = SH.viewers.get(from);
        if (!v) return;
        await v.pc.setRemoteDescription({ type: 'answer', sdp: m.sdp });
        v.answered = true;
        for (const c of v.pending.splice(0)) await v.pc.addIceCandidate(c).catch(() => {});
        break;
      }
      case 'p2p-ice': {
        const v = SH.viewers.get(from);
        if (!v) return;
        const c = { candidate: m.candidate, sdpMid: m.mid ?? null, sdpMLineIndex: m.mline ?? null };
        if (v.answered) await v.pc.addIceCandidate(c).catch(() => {}); else v.pending.push(c);
        break;
      }
      case 'p2p-bye': dropViewer(from, false); break;
    }
  } catch (e) {
    console.warn('p2p share', e);
    dropViewer(from, true);
  }
}

function dropViewer(sid, tell) {
  const v = SH.viewers.get(sid);
  if (!v) return;
  SH.viewers.delete(sid);
  if (tell) send({ t: 'p2p-bye', sid, room: SH.room, why: 'stop' });
  try { v.pc.close(); } catch { /* gone */ }
  SH.count = SH.viewers.size;
  updateShareUi();
}

export function shareStop() {
  if (!SH.on) return;
  SH.on = false;
  SH.gen++;
  for (const x of SH.timers.splice(0)) clearTimeout(x);
  if (SH.mode === 'p2p') send({ t: 'p2p-live', room: SH.room, on: false });
  closeShareSfu();
  for (const sid of [...SH.viewers.keys()]) dropViewer(sid, false);
  SH.stream?.getTracks().forEach((x) => x.stop());
  SH.stream = null;
  SH.mode = '';
  SH.count = 0;
  if (SH.preview) { SH.preview.tile.remove(); SH.preview = null; }
  layoutStage();
  updateShareUi();
}

function showPreview() {
  const video = el('video', { autoplay: true, muted: true, playsInline: true, attrs: { playsinline: true, muted: true } });
  video.srcObject = SH.stream;
  const info = el('span', { class: 'via via-self' });
  const tile = el('div', { class: 'tile self', dataset: { room: 'self' } }, video,
    el('div', { class: 'tile-bar' }, el('div', { class: 'tinfo' }, el('span', { class: 'tname', text: t('yourScreen') }), info),
      el('div', { class: 'tctl' }, el('button', { type: 'button', class: 'small danger', text: t('stopSharing'), onclick: shareStop }))));
  SH.preview = { tile, info };
  $('tiles').prepend(tile);
  video.play().catch(() => {});
  layoutStage();
}

function updateShareUi() {
  const b = $('shareBtn');
  b.classList.toggle('hidden', !navigator.mediaDevices?.getDisplayMedia || (!SH.on && !may('stream')));
  b.classList.toggle('live', SH.on);
  b.textContent = SH.on ? t('stopSharing') : t('shareScreen');
  const info = SH.on ? (SH.mode === 'p2p' ? 'P2P' : SH.up ? t('viaServer') : t('connecting')) + ' · ' + t('viewers', { n: SH.count }) : '';
  $('shareInfo').textContent = info;
  $('shareInfo').classList.toggle('hidden', !SH.on);
  if (SH.preview) SH.preview.info.textContent = info;
}
$('shareBtn').onclick = () => (SH.on ? shareStop() : shareStart());

// ====== Wiring ======

on('app:render', renderLive);
on('app:watching', renderLive);
on('app:perms', updateShareUi);
on('app:welcome', () => {
  updateShareUi();
  // A new session: watches peer to peer start again from it; our P2P share is announced again
  for (const w of W.values()) if (!w.ended && w.mode === 'p2p') follow(w);
  if (SH.on && SH.mode === 'p2p') {
    for (const sid of [...SH.viewers.keys()]) dropViewer(sid, false);
    send({ t: 'p2p-live', room: SH.room, on: true, title: '' });
  }
});
on('app:down', ({ retry }) => {
  if (retry) return;
  // Left the server: everything stops
  for (const room of [...W.keys()]) stopWatching(room);
  shareStop();
});
on('app:volumes', () => { for (const w of W.values()) applyVolume(w); });
on('app:sink', () => { for (const w of W.values()) applySink(w.video); });
on('app:lang', () => { for (const w of W.values()) updateBar(w); updateShareUi(); renderLive(); });
on('live', (m) => {
  const room = m.s?.room;
  const w = W.get(room);
  if (!w) return;
  // Ended a moment ago because it went another way (the stream's own connection was faster than the list)
  if (w.ended) { if (m.on && Date.now() - w.endedAt < 5000) restart(w); return; }
  if (m.on) {
    const l = m.s;
    w.title = l.title || '';
    const stale = l.mode === 'p2p' ? (w.mode !== 'p2p' || w.sid !== l.sid) : w.mode !== 'sfu';
    if (stale) follow(w); else updateBar(w);
  } else {
    // Off the list: the stream ended (the server says so on the stream's own connection too)
    later(w, 1500, () => { if (!S.live.has(room) && !w.ended) finish(w, t('streamEnded')); });
  }
});
for (const k of ['p2p-join', 'p2p-offer', 'p2p-answer', 'p2p-ice', 'p2p-bye']) {
  on(k, (m) => {
    if (SH.on && SH.mode === 'p2p' && m.room === SH.room && m.t !== 'p2p-offer') shareSignal(m);
    else viewerSignal(m);
  });
}
on('error', (m) => {
  if (!String(m.op || '').startsWith('p2p-')) return;
  m.handled = true;
  if (m.op === 'p2p-live' && SH.on) { toast(t('shareRefused', { why: errText(m) }), true); shareStop(); }
  else if (m.op === 'p2p-join') {
    for (const w of W.values()) if (w.mode === 'p2p' && !w.ended) finish(w, errText(m));
  }
});
