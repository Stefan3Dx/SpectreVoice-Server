// Voice: one WebRTC connection to the server's SFU over /v1/ws ("rtc"/"ice"/"rtc-reset"); the server offers again
// whenever who we hear changes. Push-to-talk (a key, Space by default, or the button), open mic, mute, deafen,
// per-person volume and mute (kept per uid in this browser), the microphone and speakers chosen in the settings, and
// the browser's own noise suppression, and Max volume (each voice through a limiter).

import { $, S, send, store, toast, emit, on, rtcConfig } from './core.js';
import { t } from './i18n.js';

export const V = { pc: null, stream: null, track: null, audios: new Map(), early: [], gen: 0 };

// Choices kept in this browser
export const prefs = {
  inDev: store.get('sv.inDev', ''),
  outDev: store.get('sv.outDev', ''),
  ns: store.get('sv.ns', '1') !== '0',
  pttKey: store.get('sv.pttKey', 'Space'),
  // Max volume: the loudest a voice may come out (dBFS, -24 .. -1; 0 = off), as in the apps
  maxDb: Math.max(-24, Math.min(0, Math.round(+store.get('sv.maxDb', '-9')) || 0)),
};

// ====== Max volume: each voice through a limiter (Web Audio), so a cough or a shout never comes out above it ======

export const A = { ctx: null };

// The limiter is heard only while the context runs: suspended (no click yet on Safari, or the browser held it back)
// or interrupted, each voice plays through its own element as before, at its volume
const ctxLive = () => !!A.ctx && A.ctx.state === 'running';

function audioCtx() {
  if (A.ctx) return A.ctx;
  const C = window.AudioContext || window.webkitAudioContext;
  if (!C) return null;
  try { A.ctx = new C({ latencyHint: 'interactive' }); } catch { return null; }
  if (prefs.outDev && typeof A.ctx.setSinkId === 'function') A.ctx.setSinkId(prefs.outDev).catch(() => {});
  // Running, suspended or interrupted: the voices follow at once (element or limiter)
  A.ctx.onstatechange = () => applyVolumes();
  // Started by the next click or key if the browser held it back
  const resume = () => { if (A.ctx && A.ctx.state === 'suspended') A.ctx.resume().catch(() => {}); };
  document.addEventListener('pointerdown', resume, true);
  document.addEventListener('keydown', resume, true);
  resume();
  return A.ctx;
}

// A limiter at [db] dBFS: in → out nodes. The compressor adds its own makeup gain (the spec's 0.6 power of what it
// takes off at full scale); the gain after it takes that back, so under the ceiling the voice is as it was.
export function limiterChain(ctx, db) {
  const c = ctx.createDynamicsCompressor();
  const ratio = 20; // the most Web Audio allows: the threshold a little lower, so full scale comes out at [db]
  const thr = Math.max(-100, (db * ratio) / (ratio - 1));
  c.threshold.value = thr;
  c.knee.value = 0;
  c.ratio.value = ratio;
  c.attack.value = 0.001;
  c.release.value = 0.4;
  const back = ctx.createGain();
  back.gain.value = Math.pow(10, (0.6 * thr * (1 - 1 / ratio)) / 20);
  c.connect(back);
  return { input: c, output: back };
}

// One voice's way out: the element alone (no limiter, or no Web Audio), or element (muted, it keeps Chrome pulling
// the remote track) → source → volume → limiter → speakers
function wire(entry) {
  unwire(entry);
  if (prefs.maxDb >= 0 || !entry.el.srcObject) return;
  const ctx = audioCtx();
  if (!ctx) return;
  // An output chosen and no way to send the context there (Firefox, Safari): the elements keep playing on it
  if (prefs.outDev && typeof ctx.setSinkId !== 'function') return;
  try {
    const src = ctx.createMediaStreamSource(entry.el.srcObject);
    const gain = ctx.createGain();
    const lim = limiterChain(ctx, prefs.maxDb);
    src.connect(gain);
    gain.connect(lim.input);
    lim.output.connect(ctx.destination);
    entry.graph = { src, gain, out: lim.output };
  } catch (e) {
    console.warn('limiter', e);
    entry.graph = null;
  }
}

// Tests (web/e2e/run.js): one voice from [stream], wired as a received one would be
export function _testVoice(stream) {
  const el = new Audio();
  el.autoplay = true;
  el.srcObject = stream;
  const entry = { el, sid: -1, graph: null };
  const key = { test: true };
  V.audios.set(key, entry);
  wire(entry);
  applyVolumes();
  entry.done = () => { unwire(entry); el.pause(); el.srcObject = null; V.audios.delete(key); };
  return entry;
}

export function setMaxDb(db) {
  prefs.maxDb = Math.max(-24, Math.min(0, Math.round(+db) || 0));
  store.set('sv.maxDb', String(prefs.maxDb));
  for (const a of V.audios.values()) wire(a);
  applyVolumes();
}

function micConstraints() {
  return {
    audio: {
      deviceId: prefs.inDev ? { ideal: prefs.inDev } : undefined,
      echoCancellation: true, noiseSuppression: prefs.ns, autoGainControl: true,
    },
  };
}

async function openMic() {
  try {
    const stream = await navigator.mediaDevices.getUserMedia(micConstraints());
    const track = stream.getAudioTracks()[0];
    track.enabled = false;
    return { stream, track };
  } catch (e) {
    toast(t(e?.name === 'NotAllowedError' ? 'micDenied' : 'noMic'), true);
    return { stream: null, track: null };
  }
}

export async function voiceStart() {
  voiceStop();
  const gen = ++V.gen;
  if (!V.stream) {
    const m = await openMic();
    V.stream = m.stream;
    V.track = m.track;
  }
  if (gen !== V.gen || !S.ws) return;
  const pc = new RTCPeerConnection(rtcConfig());
  V.pc = pc;
  V.early = [];
  pc.onicecandidate = (ev) => {
    if (!ev.candidate || V.pc !== pc) return;
    send({ t: 'ice', c: ev.candidate.candidate, mid: ev.candidate.sdpMid, mline: ev.candidate.sdpMLineIndex });
  };
  pc.ontrack = (ev) => {
    if (ev.track.kind !== 'audio') return;
    // One stream per voice, named "s<sid>"
    const sid = +((ev.streams[0]?.id || '').replace(/^s/, ''));
    const old = V.audios.get(ev.transceiver);
    if (old) { unwire(old); old.el.pause(); old.el.srcObject = null; }
    const a = new Audio();
    a.autoplay = true;
    a.srcObject = ev.streams[0] || new MediaStream([ev.track]);
    const entry = { el: a, sid, graph: null };
    V.audios.set(ev.transceiver, entry);
    wire(entry);
    applySink(a);
    applyVolumes();
    a.play().catch(() => { /* a user gesture came first (the connect button) */ });
  };
  pc.onconnectionstatechange = () => {
    if (V.pc !== pc) return;
    document.body.dataset.rtc = pc.connectionState;
    emit('app:rtc');
    if (pc.connectionState === 'connected') toast(t('voiceOn') + (V.track ? '' : ' (' + t('listenOnly') + ')'));
    if (pc.connectionState === 'failed') { toast(t('voiceOff'), true); voiceStart(); }
  };
  if (V.track) pc.addTrack(V.track, V.stream);
  else pc.addTransceiver('audio', { direction: 'recvonly' });
  const offer = await pc.createOffer();
  if (V.pc !== pc) return;
  await pc.setLocalDescription(offer);
  send({ t: 'rtc', type: 'offer', sdp: pc.localDescription.sdp });
  updatePTT();
  emit('app:voice');
}

export function voiceStop() {
  if (V.pc) { V.pc.onconnectionstatechange = null; V.pc.close(); }
  V.pc = null;
  for (const a of V.audios.values()) { unwire(a); a.el.pause(); a.el.srcObject = null; }
  V.audios.clear();
  document.body.dataset.rtc = 'new';
  emit('app:rtc');
}

export async function voiceSignal(m) {
  const pc = V.pc;
  if (m.t === 'rtc-reset') { voiceStart(); return; }
  if (!pc) return;
  try {
    if (m.t === 'ice') {
      const c = { candidate: m.c, sdpMid: m.mid ?? null, sdpMLineIndex: m.mline ?? null };
      if (!pc.remoteDescription) V.early.push(c); else await pc.addIceCandidate(c);
      return;
    }
    if (m.type === 'answer') {
      if (pc.signalingState !== 'have-local-offer') return;
      await pc.setRemoteDescription({ type: 'answer', sdp: m.sdp });
    } else if (m.type === 'offer') {
      // Ours is out and unanswered: the server rolls back and offers again after its answer
      if (pc.signalingState !== 'stable') return;
      await pc.setRemoteDescription({ type: 'offer', sdp: m.sdp });
      const ans = await pc.createAnswer();
      await pc.setLocalDescription(ans);
      send({ t: 'rtc', type: 'answer', sdp: pc.localDescription.sdp });
    }
    for (const c of V.early.splice(0)) await pc.addIceCandidate(c).catch(() => {});
    pruneAudios();
  } catch (e) {
    console.warn('rtc', e);
  }
}

// Voices the server stopped sending: their players go
function pruneAudios() {
  for (const [tr, a] of V.audios) {
    if (tr.stopped || !tr.currentDirection || !tr.currentDirection.includes('recv')) {
      unwire(a);
      a.el.pause();
      a.el.srcObject = null;
      V.audios.delete(tr);
    }
  }
}

function unwire(entry) {
  if (!entry.graph) return;
  try { entry.graph.src.disconnect(); entry.graph.out.disconnect(); } catch { /* gone */ }
  entry.graph = null;
}

export function applyVolumes() {
  for (const { el: a, sid, graph } of V.audios.values()) {
    const uid = S.members.get(sid)?.uid;
    const vol = Math.max(0, Math.min(1, S.volumes[uid] ?? 1));
    const off = S.snd || !!S.muted[uid];
    if (graph && ctxLive()) {
      // Through the limiter: the element stays muted (it only keeps the track flowing)
      graph.gain.gain.value = off ? 0 : vol;
      a.muted = true;
    } else {
      // Not through the limiter (none, or the context isn't running): the element plays, the graph stays silent
      if (graph) graph.gain.gain.value = 0;
      a.volume = vol;
      a.muted = off;
    }
  }
  emit('app:volumes');
}

// The speakers chosen in the settings, for any media element of the page (voices, streams)
export function applySink(media) {
  if (prefs.outDev && typeof media.setSinkId === 'function') media.setSinkId(prefs.outDev).catch(() => {});
}

export function updatePTT() {
  const live = !!V.track && !S.mic && (S.open || S.pressing);
  if (V.track) V.track.enabled = live;
  $('ptt').classList.toggle('live', live);
  $('ptt').classList.toggle('off', S.mic || !V.track);
}

// ====== Settings that touch the microphone ======

// A new microphone (another device, or noise suppression switched): the track is swapped on the live connection
async function reopenMic() {
  if (!S.ws || S.phase !== 'on') { if (V.stream) { V.stream.getTracks().forEach((x) => x.stop()); V.stream = null; V.track = null; } return; }
  const had = !!V.track;
  const m = await openMic();
  if (!m.track) return;
  const old = V.stream;
  V.stream = m.stream;
  V.track = m.track;
  const sender = V.pc?.getSenders().find((x) => x.track?.kind === 'audio' || x.track === null);
  if (had && sender) {
    await sender.replaceTrack(m.track).catch(() => {});
    old?.getTracks().forEach((x) => x.stop());
    updatePTT();
  } else {
    old?.getTracks().forEach((x) => x.stop());
    voiceStart(); // listening only until now: a connection with a microphone
  }
}

export async function setInputDevice(id) {
  prefs.inDev = id;
  store.set('sv.inDev', id);
  await reopenMic();
}
export function setOutputDevice(id) {
  prefs.outDev = id;
  store.set('sv.outDev', id);
  for (const { el: a } of V.audios.values()) {
    if (typeof a.setSinkId === 'function') a.setSinkId(id).catch(() => {});
  }
  if (A.ctx && typeof A.ctx.setSinkId === 'function') A.ctx.setSinkId(id || '').catch(() => {});
  // An output the context can't follow (no setSinkId): the voices back on their elements, or on the limiter again
  for (const a of V.audios.values()) wire(a);
  applyVolumes();
  emit('app:sink');
}
export async function setNoiseSuppression(on) {
  prefs.ns = on;
  store.set('sv.ns', on ? '1' : '0');
  if (V.track && typeof V.track.applyConstraints === 'function') {
    try { await V.track.applyConstraints({ noiseSuppression: on, echoCancellation: true, autoGainControl: true }); return; } catch { /* reopened below */ }
  }
  await reopenMic();
}
export function setPttKey(code) {
  prefs.pttKey = code || 'Space';
  store.set('sv.pttKey', prefs.pttKey);
  emit('app:ptt');
}
// "Space", "KeyT", "F2"… as people read them
export function keyName(code) {
  if (!code) return '';
  return code.replace(/^Key/, '').replace(/^Digit/, '').replace(/^Numpad/, 'Num ').replace(/(Left|Right)$/, ' $1').trim();
}

// ====== Push-to-talk, mute, deafen ======

const ptt = $('ptt');
const press = (onp) => (ev) => { if (ev.type.startsWith('pointer')) ev.preventDefault(); S.pressing = onp; updatePTT(); };
ptt.onpointerdown = press(true);
ptt.onpointerup = press(false);
ptt.onpointerleave = press(false);
ptt.onpointercancel = press(false);
ptt.oncontextmenu = (ev) => ev.preventDefault();
const typing = () => /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName) || $('dlg').open;
document.addEventListener('keydown', (ev) => {
  if (ev.code !== prefs.pttKey || ev.repeat || S.phase !== 'on' || typing()) return;
  ev.preventDefault();
  S.pressing = true;
  updatePTT();
});
document.addEventListener('keyup', (ev) => {
  if (ev.code !== prefs.pttKey || !S.pressing) return;
  S.pressing = false;
  updatePTT();
});
window.addEventListener('blur', () => { S.pressing = false; updatePTT(); });
$('openMic').onchange = () => { S.open = $('openMic').checked; store.set('sv.open', S.open ? '1' : ''); updatePTT(); };
// Like the apps: deafening mutes the mic too, undeafening gives it back as it was (still off if you'd muted it
// first), and mute while deafened undeafens and turns the mic on
let micBeforeDeaf = false;
function setSelf(mic, snd) {
  S.mic = mic; S.snd = snd;
  send({ t: 'state', mic, snd });
  updatePTT(); applyVolumes(); emit('app:selfstate');
}
$('mute').onclick = () => setSelf(S.snd ? false : !S.mic, false);
$('deafen').onclick = () => { if (S.snd) setSelf(micBeforeDeaf, false); else { micBeforeDeaf = S.mic; setSelf(true, true); } };

on('rtc', voiceSignal);
on('ice', voiceSignal);
on('rtc-reset', voiceSignal);
on('app:down', voiceStop);
on('app:ptt', () => { $('pttKey').textContent = keyName(prefs.pttKey); });
