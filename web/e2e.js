// End-to-end encryption of private messages, with WebCrypto only (docs/E2E.md in the apps repo; the same scheme as
// the apps' core/E2eCrypto.kt): ECDH and ECDSA on P-256, HKDF-SHA256, AES-256-GCM, SHA-256/512. Nothing here keeps
// or logs anything; dm.js holds the keys. Works in the browser and in node (globalThis.crypto) for the interop tests.

const subtle = globalThis.crypto.subtle;
const te = new TextEncoder();
const td = new TextDecoder('utf-8', { fatal: true });

export const DEVICE_PREFIX = 'spectrevoice-e2e-device-v1\n';
export const MSG_PREFIX = 'spectrevoice-e2e-msg-v1\n';
const AAD_PREFIX = 'sv-e2e-msg-v1\n';
const WRAP_INFO = 'sv-e2e-wrap-v1\n';
const SAFETY_PREFIX = 'sv-safety-v1\n';
export const CHUNK = 64 * 1024;
export const MAX_TEXT = 4000;

export const b64 = {
  enc(bytes) {
    const b = new Uint8Array(bytes);
    let s = '';
    for (let i = 0; i < b.length; i += 0x8000) s += String.fromCharCode(...b.subarray(i, i + 0x8000));
    return btoa(s);
  },
  dec: (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0)),
  url: (bytes) => b64.enc(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''),
};
const cat = (...parts) => {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let o = 0;
  for (const p of parts) { out.set(p, o); o += p.length; }
  return out;
};
export const random = (n) => globalThis.crypto.getRandomValues(new Uint8Array(n));

// Constant-time equality (no early exit) for what's compared against something secret-derived.
export function same(a, b) {
  if (typeof a === 'string') a = te.encode(a);
  if (typeof b === 'string') b = te.encode(b);
  let d = a.length ^ b.length;
  for (let i = 0; i < Math.max(a.length, b.length); i++) d |= (a[i] ?? 0) ^ (b[i] ?? 0);
  return d === 0;
}

// ====== Keys ======

const SPKI_P256 = Uint8Array.from([0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00]);

export async function uidOf(spki) {
  const h = new Uint8Array(await subtle.digest('SHA-256', spki));
  return b64.url(h.slice(0, 15));
}

// The 65-byte point of a P-256 SPKI (or null when it isn't one).
export function rawOfSpki(spki) {
  if (spki.length !== SPKI_P256.length + 65 || !same(spki.slice(0, SPKI_P256.length), SPKI_P256)) return null;
  return spki.slice(SPKI_P256.length);
}

// A P-256 public key for ECDH or ECDSA from base64 SPKI; WebCrypto checks the point is on the curve. null: not one.
export async function importPublic(spkiB64, use) {
  try {
    if (spkiB64.length > 300) return null;
    const spki = b64.dec(spkiB64);
    const raw = rawOfSpki(spki);
    if (!raw) return null;
    const alg = use === 'ecdh' ? { name: 'ECDH', namedCurve: 'P-256' } : { name: 'ECDSA', namedCurve: 'P-256' };
    const key = await subtle.importKey('spki', spki, alg, true, use === 'ecdh' ? [] : ['verify']);
    return { key, raw, spki };
  } catch {
    return null;
  }
}

export async function newDeviceKey(extractable = false) {
  const pair = await subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, extractable, ['deriveBits']);
  const spki = new Uint8Array(await subtle.exportKey('spki', pair.publicKey));
  return { priv: pair.privateKey, pub: pair.publicKey, spki: b64.enc(spki), raw: rawOfSpki(spki) };
}

// WebCrypto signs r‖s; the apps and the server use ASN.1 DER.
export function derOfRaw(raw) {
  const int = (b) => {
    let i = 0;
    while (i < b.length - 1 && b[i] === 0) i++;
    b = b.slice(i);
    if (b[0] & 0x80) b = cat(Uint8Array.of(0), b);
    return cat(Uint8Array.of(0x02, b.length), b);
  };
  const body = cat(int(raw.slice(0, 32)), int(raw.slice(32)));
  return cat(Uint8Array.of(0x30, body.length), body);
}
export function rawOfDer(der) {
  // SEQUENCE { INTEGER r, INTEGER s }, short lengths only (a P-256 signature is < 128 bytes)
  if (der.length < 8 || der[0] !== 0x30 || der[1] !== der.length - 2) return null;
  let o = 2;
  const out = new Uint8Array(64);
  for (let k = 0; k < 2; k++) {
    if (der[o] !== 0x02) return null;
    const n = der[o + 1];
    let v = der.slice(o + 2, o + 2 + n);
    if (v.length !== n || n === 0 || n > 33) return null;
    if (v.length === 33) { if (v[0] !== 0) return null; v = v.slice(1); }
    out.set(v, k * 32 + (32 - v.length));
    o += 2 + n;
  }
  return o === der.length ? out : null;
}

async function sign(identity, prefix, data) {
  const raw = new Uint8Array(await subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, identity.priv, cat(te.encode(prefix), data)));
  return b64.enc(derOfRaw(raw));
}
async function verify(key, data, sigB64) {
  try {
    const raw = rawOfDer(b64.dec(sigB64));
    return !!raw && (await subtle.verify({ name: 'ECDSA', hash: 'SHA-256' }, key, raw, data));
  } catch {
    return false;
  }
}

async function hkdf(ikm, salt, info, bytes) {
  const k = await subtle.importKey('raw', ikm, 'HKDF', false, ['deriveBits']);
  return new Uint8Array(await subtle.deriveBits({ name: 'HKDF', hash: 'SHA-256', salt, info }, k, bytes * 8));
}
async function gcm(op, key, iv, data, aad) {
  const k = await subtle.importKey('raw', key, 'AES-GCM', false, [op]);
  const p = { name: 'AES-GCM', iv, tagLength: 128 };
  if (aad) p.additionalData = aad;
  return new Uint8Array(await (op === 'encrypt' ? subtle.encrypt(p, k, data) : subtle.decrypt(p, k, data)));
}
async function ecdh(priv, pub) {
  return new Uint8Array(await subtle.deriveBits({ name: 'ECDH', public: pub }, priv, 256));
}

// ====== Device records ======

const recordSigned = (r) => te.encode(r.uid + '\n' + r.dev + '\n' + r.ik + '\n' + r.dk + '\n' + r.ts);

// identity: { priv: ECDSA CryptoKey, pub: base64 SPKI, uid }
export async function signRecord(identity, dev, dkSpki, ts = Date.now()) {
  const r = { v: 1, uid: identity.uid, dev, ik: identity.pub, dk: dkSpki, ts };
  r.sig = await sign(identity, DEVICE_PREFIX, recordSigned(r));
  return r;
}

// The device's ECDH key when the record is genuine (its identity is the uid's and signed it), else null.
export async function verifyRecord(r) {
  if (!r || r.v !== 1 || typeof r.uid !== 'string' || typeof r.dev !== 'string' || !r.dev || r.dev.length > 64 ||
      typeof r.ik !== 'string' || typeof r.dk !== 'string' || typeof r.sig !== 'string' || !Number.isSafeInteger(r.ts) || r.ts <= 0) return null;
  const ik = await importPublic(r.ik, 'ecdsa');
  if (!ik || !same(await uidOf(ik.spki), r.uid)) return null;
  if (!(await verify(ik.key, cat(te.encode(DEVICE_PREFIX), recordSigned(r)), r.sig))) return null;
  const dk = await importPublic(r.dk, 'ecdh');
  return dk ? { uid: r.uid, dev: r.dev, key: dk.key, raw: dk.raw, ik: r.ik } : null;
}

// ====== Messages ======

const aadOf = (from, fdev, to, ts, mid) => AAD_PREFIX + from + '\n' + fdev + '\n' + to + '\n' + ts + '\n' + mid;
const keyLine = (k) => `${k.to},${k.dev},${k.epk},${k.iv},${k.wk}\n`;
const signedEnvelope = (aad, iv, ct, keys) => te.encode(aad + '\n' + iv + '\n' + ct + '\n' + keys.map(keyLine).join(''));
export const kek = (z, epk, dk, uid, dev, aad) => hkdf(z, cat(epk, dk), te.encode(WRAP_INFO + uid + '\n' + dev + '\n' + aad), 32);

function body(payload) {
  const m = { text: payload.text };
  if (payload.att) m.att = payload.att;
  m.p = '';
  const n = te.encode(JSON.stringify(m)).length;
  m.p = ' '.repeat((128 - (n % 128)) % 128);
  return te.encode(JSON.stringify(m));
}

export const newMid = () => b64.url(random(16));

// Seals payload ({text, att?}) from identity's device fromDev to uid `to` for every recipient ({uid, dev, key, raw}),
// signed: the envelope JSON the server keeps.
export async function seal(identity, fromDev, to, payload, recipients, ts = Date.now(), mid = newMid()) {
  if (!recipients.length || recipients.length > 32) throw new Error('1 to 32 devices');
  if (payload.text.length > MAX_TEXT) throw new Error('too long');
  const aad = aadOf(identity.uid, fromDev, to, ts, mid);
  const aadBytes = te.encode(aad);
  const ck = random(32);
  const iv = random(12);
  const ct = await gcm('encrypt', ck, iv, body(payload), aadBytes);
  const keys = [];
  for (const r of recipients) {
    const e = await subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']);
    const epk = new Uint8Array(await subtle.exportKey('raw', e.publicKey));
    const z = await ecdh(e.privateKey, r.key);
    const k = await kek(z, epk, r.raw, r.uid, r.dev, aad);
    const kiv = random(12);
    keys.push({ to: r.uid, dev: r.dev, epk: b64.enc(epk), iv: b64.enc(kiv), wk: b64.enc(await gcm('encrypt', k, kiv, ck, aadBytes)) });
  }
  const ivs = b64.enc(iv);
  const cts = b64.enc(ct);
  const sig = await sign(identity, MSG_PREFIX, signedEnvelope(aad, ivs, cts, keys));
  return JSON.stringify({ v: 1, from: identity.uid, fdev: fromDev, to, ts, mid, ik: identity.pub, iv: ivs, ct: cts, keys, sig });
}

// Opens an envelope for this device (me, myDev, device = {priv, raw}): from `from` (the sender the server names) in the
// conversation of me and peer. {ok:true, text, att, from, to, ts, mid, ik} or {ok:false, why, mid}.
export async function open(envelope, me, myDev, device, from, peer) {
  let e;
  try { e = JSON.parse(envelope); } catch { return { ok: false, why: 'MALFORMED' }; }
  const mid = typeof e?.mid === 'string' ? e.mid : null;
  const fail = (why) => ({ ok: false, why, mid });
  const str = (k) => typeof e[k] === 'string';
  if (!e || e.v !== 1 || !str('from') || !str('to') || !str('fdev') || !str('ik') || !str('iv') || !str('ct') || !str('sig') ||
      !Number.isSafeInteger(e.ts) || !Array.isArray(e.keys) || !mid || mid.length < 16 || mid.length > 32 || !e.keys.length || e.keys.length > 32 ||
      e.keys.some((k) => !k || typeof k !== 'object')) return fail('MALFORMED');
  if (e.from === e.to || !((e.from === me && e.to === peer) || (e.from === peer && e.to === me))) return fail('WRONG_CONVERSATION');
  if (!same(e.from, from)) return fail('NOT_FROM_SENDER');
  const ik = await importPublic(e.ik, 'ecdsa');
  if (!ik || !same(await uidOf(ik.spki), e.from)) return fail('NOT_FROM_SENDER');
  const aad = aadOf(e.from, e.fdev, e.to, e.ts, mid);
  if (!(await verify(ik.key, cat(te.encode(MSG_PREFIX), signedEnvelope(aad, e.iv, e.ct, e.keys)), e.sig))) return fail('BAD_SIGNATURE');
  const mine = e.keys.find((k) => k.to === me && k.dev === myDev);
  if (!mine) return fail('NOT_FOR_THIS_DEVICE');
  try {
    const aadBytes = te.encode(aad);
    const epk = b64.dec(mine.epk);
    const pub = await subtle.importKey('raw', epk, { name: 'ECDH', namedCurve: 'P-256' }, false, []);
    const z = await ecdh(device.priv, pub);
    const k = await kek(z, epk, device.raw, me, myDev, aad);
    const ck = await gcm('decrypt', k, b64.dec(mine.iv), b64.dec(mine.wk), aadBytes);
    if (ck.length !== 32) return fail('DAMAGED');
    const b = JSON.parse(td.decode(await gcm('decrypt', ck, b64.dec(e.iv), b64.dec(e.ct), aadBytes)));
    const text = typeof b.text === 'string' ? b.text.slice(0, MAX_TEXT) : '';
    return { ok: true, text, att: attachment(b.att), from: e.from, to: e.to, ts: e.ts, mid, ik: e.ik };
  } catch {
    return fail('DAMAGED');
  }
}

function attachment(a) {
  if (!a || typeof a !== 'object' || typeof a.url !== 'string' || typeof a.k !== 'string' || typeof a.np !== 'string') return null;
  try {
    if (b64.dec(a.k).length !== 32 || b64.dec(a.np).length !== 7) return null;
  } catch { return null; }
  return { url: a.url, k: a.k, np: a.np, size: Number.isSafeInteger(a.size) ? a.size : 0, name: String(a.name || 'file').slice(0, 180), mime: String(a.mime || '').slice(0, 100) };
}

// ====== Files (STREAM: AES-256-GCM per 64 KiB piece) ======

function nonce(np, i, last) {
  if (i > 0xffffffff) throw new Error('file too big');
  return cat(np, Uint8Array.of((i >>> 24) & 255, (i >>> 16) & 255, (i >>> 8) & 255, i & 255, last ? 1 : 0));
}

// bytes (Uint8Array) → { data: ciphertext, k, np } (k and np base64, for the message)
export async function encryptFile(bytes) {
  const key = random(32);
  const np = random(7);
  const n = Math.max(1, Math.ceil(bytes.length / CHUNK));
  const parts = [];
  for (let i = 0; i < n; i++) parts.push(await gcm('encrypt', key, nonce(np, i, i === n - 1), bytes.subarray(i * CHUNK, (i + 1) * CHUNK)));
  return { data: cat(...parts), k: b64.enc(key), np: b64.enc(np) };
}

// Throws when a piece was changed, moved, dropped or the end cut off.
export async function decryptFile(data, kB64, npB64) {
  const key = b64.dec(kB64);
  const np = b64.dec(npB64);
  if (key.length !== 32 || np.length !== 7) throw new Error('bad key');
  const piece = CHUNK + 16;
  const n = Math.max(1, Math.ceil(data.length / piece));
  const parts = [];
  for (let i = 0; i < n; i++) {
    const c = data.subarray(i * piece, (i + 1) * piece);
    if (c.length < 16) throw new Error('damaged file');
    try { parts.push(await gcm('decrypt', key, nonce(np, i, i === n - 1), c)); } catch { throw new Error('damaged file'); }
  }
  return cat(...parts);
}

// ====== Safety numbers ======

export async function fingerprint(uid, spkiB64) {
  const spki = b64.dec(spkiB64);
  let h = cat(te.encode(SAFETY_PREFIX + uid + '\n'), spki);
  for (let i = 0; i < 5200; i++) h = new Uint8Array(await subtle.digest('SHA-512', cat(h, spki)));
  let out = '';
  for (let c = 0; c < 6; c++) {
    let v = 0;
    for (let j = 0; j < 5; j++) v = v * 256 + h[c * 5 + j];
    out += String(v % 100000).padStart(5, '0');
  }
  return out;
}

// 60 digits, the smaller uid's half first: the same on both sides.
export async function safetyNumber(uidA, ikA, uidB, ikB) {
  const [a, b] = uidA < uidB ? [[uidA, ikA], [uidB, ikB]] : [[uidB, ikB], [uidA, ikA]];
  return (await fingerprint(a[0], a[1])) + (await fingerprint(b[0], b[1]));
}

export async function sha256b64(s) {
  return b64.enc(new Uint8Array(await subtle.digest('SHA-256', te.encode(s))));
}
