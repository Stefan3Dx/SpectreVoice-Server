// Identity: a P-256 key in IndexedDB (the private key is not extractable and never leaves the browser). The server
// knows us by the key's uid; the v2 challenge is signed for the page's own host.

import { b64 } from './core.js';

function idb() {
  return new Promise((ok, no) => {
    const req = indexedDB.open('spectrevoice', 1);
    req.onupgradeneeded = () => req.result.createObjectStore('keys');
    req.onsuccess = () => ok(req.result);
    req.onerror = () => no(req.error);
  });
}
function idbGet(db, k) {
  return new Promise((ok, no) => {
    const r = db.transaction('keys').objectStore('keys').get(k);
    r.onsuccess = () => ok(r.result);
    r.onerror = () => no(r.error);
  });
}
function idbPut(db, k, v) {
  return new Promise((ok, no) => {
    const tx = db.transaction('keys', 'readwrite');
    tx.objectStore('keys').put(v, k);
    tx.oncomplete = ok;
    tx.onerror = () => no(tx.error);
  });
}

export async function loadIdentity() {
  const db = await idb();
  let pair = await idbGet(db, 'id');
  if (!pair) {
    pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
    await idbPut(db, 'id', pair);
  }
  const spki = new Uint8Array(await crypto.subtle.exportKey('spki', pair.publicKey));
  const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', spki));
  return { priv: pair.privateKey, pub: b64.enc(spki), uid: b64.url(hash.slice(0, 15)) };
}

// WebCrypto gives r‖s; the server wants ASN.1 DER.
function derSig(raw) {
  const int = (b) => {
    let i = 0;
    while (i < b.length - 1 && b[i] === 0) i++;
    b = b.slice(i);
    if (b[0] & 0x80) b = new Uint8Array([0, ...b]);
    return [0x02, b.length, ...b];
  };
  const body = [...int(raw.slice(0, 32)), ...int(raw.slice(32))];
  return new Uint8Array([0x30, body.length, ...body]);
}

export async function signChallenge(id, nonceB64) {
  const host = location.hostname.replace(/^\[|\]$/g, '').toLowerCase();
  const prefix = new TextEncoder().encode('spectrevoice-auth-v2:' + host + '\n');
  const nonce = b64.dec(nonceB64);
  const data = new Uint8Array(prefix.length + nonce.length);
  data.set(prefix);
  data.set(nonce, prefix.length);
  const raw = new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, id.priv, data));
  return b64.enc(derSig(raw));
}
