// Voice: one WebRTC connection to the server's SFU over /v1/ws ("rtc"/"ice"/"rtc-reset"); the server offers again
// whenever who we hear changes. Push-to-talk (a key, Space by default, or the button), open mic, mute, deafen,
// per-person volume and mute (kept per uid in this browser), the microphone and speakers chosen in the settings, and
// the browser's own noise suppression.

import { $, S, send, store, toast, emit, on, rtcConfig } from './core.js';
import { t } from './i18n.js';

export const V = { pc: null, stream: null, track: null, audios: new Map(), early: [], gen: 0 };

// Choices kept in this browser
export const prefs = {
  inDev: store.get('sv.inDev', ''),
  outDev: store.get('sv.outDev', ''),
  ns: store.get('sv.ns', '1') !== '0',
  pttKey: store.get('sv.pttKey', 'Space'),
};

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
    if (old) { old.el.pause(); old.el.srcObject = null; }
    const a = new Audio();
    a.autoplay = true;
    a.srcObject = ev.streams[0] || new MediaStream([ev.track]);
    V.audios.set(ev.transceiver, { el: a, sid });
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
  for (const { el } of V.audios.values()) { el.pause(); el.srcObject = null; }
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
      a.el.pause();
      a.el.srcObject = null;
      V.audios.delete(tr);
    }
  }
}

export function applyVolumes() {
  for (const { el: a, sid } of V.audios.values()) {
    const uid = S.members.get(sid)?.uid;
    a.volume = Math.max(0, Math.min(1, S.volumes[uid] ?? 1));
    a.muted = S.snd || !!S.muted[uid];
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
$('mute').onclick = () => { S.mic = !S.mic; send({ t: 'state', mic: S.mic }); updatePTT(); emit('app:selfstate'); };
$('deafen').onclick = () => { S.snd = !S.snd; send({ t: 'state', snd: S.snd }); applyVolumes(); emit('app:selfstate'); };

on('rtc', voiceSignal);
on('ice', voiceSignal);
on('rtc-reset', voiceSignal);
on('app:down', voiceStop);
on('app:ptt', () => { $('pttKey').textContent = keyName(prefs.pttKey); });
