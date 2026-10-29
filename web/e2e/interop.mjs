// Interop of the end-to-end encryption between the apps (Kotlin, core/E2eCrypto.kt) and the browser (web/e2e.js):
//
//	node web/e2e/interop.mjs <in.json> <out.json>
//
// in.json is written by the apps' E2eInteropTest: keys made in Kotlin (exported), an envelope Ana sealed for Bob, Ana's
// device record, an encrypted file, the safety number Kotlin worked out, and tampered copies. This opens and checks
// all of it with WebCrypto, then answers as Bob: a reply sealed here for Ana's device, Bob's device record signed
// here, a file encrypted here. The test opens those in Kotlin. Exit 0 when everything here checked.
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const E = await import(join(dirname(fileURLToPath(import.meta.url)), '..', 'e2e.js'));
const subtle = globalThis.crypto.subtle;
const [inPath, outPath] = process.argv.slice(2);
const v = JSON.parse(readFileSync(inPath, 'utf8'));
const fail = (m) => { console.error('interop: ' + m); process.exit(1); };

const bobId = {
  uid: v.bob.uid, pub: v.bob.ik,
  priv: await subtle.importKey('pkcs8', E.b64.dec(v.bob.pkcs8), { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']),
};
const bobDev = {
  priv: await subtle.importKey('pkcs8', E.b64.dec(v.bobDev.pkcs8), { name: 'ECDH', namedCurve: 'P-256' }, false, ['deriveBits']),
  raw: E.rawOfSpki(E.b64.dec(v.bobDev.spki)),
};
if (await E.uidOf(E.b64.dec(v.bob.ik)) !== v.bob.uid) fail('uid of a Kotlin identity differs');

const out = {};
const o = await E.open(v.envelope, v.bob.uid, v.bobDev.dev, bobDev, v.ana.uid, v.ana.uid);
if (!o.ok) fail('Kotlin envelope not opened: ' + o.why);
if (o.text !== v.text) fail('text differs: ' + JSON.stringify(o.text));
if (JSON.stringify(o.att) !== JSON.stringify(v.att)) fail('attachment differs: ' + JSON.stringify(o.att));
out.opened = o.text;
out.tampered = [];
for (const t of v.tampered) out.tampered.push((await E.open(t, v.bob.uid, v.bobDev.dev, bobDev, v.ana.uid, v.ana.uid)).why || 'OPENED');
const rec = await E.verifyRecord(v.anaRecord);
if (!rec || rec.dev !== v.anaDev.dev || rec.dk !== v.anaRecord.dk || E.b64.enc(E.b64.dec(v.anaRecord.dk)) !== v.anaDev.spki) fail('Kotlin device record refused');
if (await E.verifyRecord({ ...v.anaRecord, ts: v.anaRecord.ts + 1 })) fail('a changed device record passed');
const plain = await E.decryptFile(E.b64.dec(v.file.data), v.file.k, v.file.np);
if (E.b64.enc(plain) !== v.file.plain) fail('file differs');
const sn = await E.safetyNumber(v.ana.uid, v.ana.ik, v.bob.uid, v.bob.ik);
if (sn !== v.safety) fail('safety number differs: ' + sn);
out.kek = E.b64.enc(await E.kek(E.b64.dec(v.kek.z), E.b64.dec(v.kek.epk), E.b64.dec(v.kek.dk), 'BOB', 'devB', v.kek.aad));

// Bob answers from the browser's side
const anaDev = await E.importPublic(v.anaDev.spki, 'ecdh');
out.replyText = 'Salut Ana — din browser ✓ ' + 'ș'.repeat(50);
out.replyAtt = { url: '/f/0123456789abcdef0123456789abcdef/file.sve', k: E.b64.enc(E.random(32)), np: E.b64.enc(E.random(7)), size: 9, name: 'note.txt', mime: 'text/plain' };
const bobDevPub = await E.importPublic(v.bobDev.spki, 'ecdh');
out.reply = await E.seal(bobId, v.bobDev.dev, v.ana.uid, { text: out.replyText, att: out.replyAtt }, [
  { uid: v.ana.uid, dev: v.anaDev.dev, key: anaDev.key, raw: anaDev.raw },
  { uid: v.bob.uid, dev: v.bobDev.dev, key: bobDevPub.key, raw: bobDevPub.raw },
]);
// And it opens here too (the sender's own device reads its history)
if (!(await E.open(out.reply, v.bob.uid, v.bobDev.dev, bobDev, v.bob.uid, v.ana.uid)).ok) fail('own reply not opened');
out.bobRecord = await E.signRecord(bobId, v.bobDev.dev, v.bobDev.spki, 1700000000123);
const f2 = new Uint8Array(200000).map((_, i) => (i * 7) & 255);
const ef = await E.encryptFile(f2);
out.file = { data: E.b64.enc(ef.data), k: ef.k, np: ef.np, plain: E.b64.enc(f2) };
writeFileSync(outPath, JSON.stringify(out));
console.log('OK');
