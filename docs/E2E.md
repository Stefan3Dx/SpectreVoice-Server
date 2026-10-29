# End-to-end encrypted private messages

(The same text as docs/E2E.md in the apps repo; the browser side is web/e2e.js and web/dm.js, the server side e2e.go.)

Private messages (DMs) between two people are end-to-end encrypted: the apps encrypt them and the server keeps and
passes on only ciphertext. The server never sees the text or what an attachment holds. Channel chat and the server
chat are not end-to-end encrypted (many readers, moderation, server-side search), and this document doesn't change
that.

What this protects against: someone who runs or takes over the server reading or quietly changing private
messages: the operator, a hosting company, a stolen database or backup. What it doesn't protect against: a
compromised device (malware, someone with the unlocked phone or PC), the metadata the server needs to deliver
(who writes to whom, when, how big), and screenshots.

The same scheme on every client: shared Kotlin (`shared/.../core/E2eCrypto.kt`, with the JDK and Android's own
crypto: `KeyPairGenerator`, `KeyAgreement`, `Cipher`, `Mac`, `Signature`) for the PC and Android, and WebCrypto in the
browser (`web/e2e.js` in the server repo). No primitive is written by hand: ECDH and ECDSA on P-256, HKDF-SHA256
(RFC 5869, built from the platform's HMAC), AES-256-GCM, SHA-256/512.

## 1. Keys

**Identity key** (exists already): one P-256 key per person, made on the device (`core/Identity.kt`; in the browser, a
non-extractable WebCrypto key in IndexedDB). Its uid is `base64url(SHA-256(SPKI))[:20 chars]`, so a uid *is* a
commitment to the identity key: nobody, the server included, can show another identity key for the same uid.

**Device key** (new): every install makes its own P-256 ECDH key pair the first time it connects.
- PC: kept in `device.properties`, sealed with the app's secret store (Windows DPAPI, else AES-256-GCM with the
  0600 `secret.key`). The `device` file is not in backups: another PC is another device.
- Android: the same file, sealed with the Android keystore key; `data_extraction_rules.xml` keeps it off backups and
  device-to-device transfers.
- Browser: a non-extractable `CryptoKey` in IndexedDB (`keys` store, `e2e-dk`).

Each device publishes a **device record**, signed by the identity key:

```
{"v":1, "uid":U, "dev":D, "ik":<identity SPKI, base64>, "dk":<device SPKI, base64>, "ts":<ms>, "sig":<base64 DER>}
signed = "spectrevoice-e2e-device-v1\n" + U + "\n" + D + "\n" + ik + "\n" + dk + "\n" + ts      (UTF-8)
```

`D` is the per-install id the apps already send in the hello (`dev`). A record is accepted (by the server, and again
by every app that uses it) only when `uidOf(ik) == U`, the signature checks with `ik`, `dk` is a P-256 key and the
sizes are sane. The server keeps at most 10 devices a person (the one seen longest ago goes), refuses a record older
than the one it has for that device, and drops devices not seen for 180 days.

The signing key is the same identity key the sign-in uses; the prefixes (`spectrevoice-auth-v2:`,
`spectrevoice-e2e-device-v1`, `spectrevoice-e2e-msg-v1`) keep the three uses apart, so a signature for one can never
pass as another.

## 2. A message

Sending to person R from person S (on device Ds):

1. The app asks the server for R's devices and S's own (`e2e-keys`), and checks every record as above. Devices
   whose record doesn't check are left out (and the screen says the server sent bad keys).
2. A fresh random 32-byte content key `CK`, a random 16-byte message id `mid`, the time `ts`.
3. `AAD = "sv-e2e-msg-v1\n" + S + "\n" + Ds + "\n" + R + "\n" + ts + "\n" + mid` (UTF-8).
4. The body: `{"text":…, "att":{…}?, "p":"<padding>"}` as UTF-8 JSON, padded with spaces to a multiple of 128
   bytes (the length leaks less), encrypted with AES-256-GCM under `CK`, a random 12-byte IV, and `AAD`.
5. For every device of R **and every device of S** (so S's other devices, and this one, can read the history):
   a fresh ephemeral P-256 key pair `E` (ECIES: one per recipient device, per message), `Z = ECDH(E, dk)`,
   `KEK = HKDF-SHA256(ikm = Z, salt = raw(E.pub) ‖ raw(dk), info = "sv-e2e-wrap-v1\n" + uid + "\n" + dev + "\n" + AAD, 32)`,
   and `CK` wrapped with AES-256-GCM under `KEK`, a random IV, and `AAD`.
6. The envelope, signed with S's identity key (ECDSA P-256, SHA-256, DER):

```
{"v":1, "from":S, "fdev":Ds, "to":R, "ts":ts, "mid":mid, "ik":<S's identity SPKI>,
 "iv":…, "ct":…, "keys":[{"to":uid, "dev":D, "epk":<raw point>, "iv":…, "wk":…}, …], "sig":…}
signed = "spectrevoice-e2e-msg-v1\n" + AAD + "\n" + iv + "\n" + ct + "\n" + (for each key, in order: to + "," + dev + "," + epk + "," + iv + "," + wk + "\n")
```

All binary values are standard base64; `mid` is base64url. The server takes the envelope as an opaque string
(`e2e-say`), checks only what it knows anyway (`from` is the sender, `to` the recipient, the size), keeps it
in the `messages` row's new `enc` column with an empty `text`, and hands it out as `"e2e"` with `msg`, `history` and
`dms`.

Receiving (every app, for every encrypted line, live or from the history):

- `from`/`to` must be the two people of the conversation the server filed it under, and `from` the sender the
  server names; `uidOf(ik) == from`; the signature must check with `ik`. So the server can't change a message, move
  it to another conversation, or make one up "from" someone.
- The key for this uid and this device is unwrapped and the body decrypted; GCM checks it together with `AAD`.
- **Replays**: an `mid` seen before in the conversation (the server sending the same envelope again under a new
  id) isn't shown a second time; the copy is marked. The apps remember the ids of what they have loaded (the
  history shown, and what came while connected), not of everything ever.
- **Time**: `ts` is the sender's clock; when it differs from the server's time by more than an hour the line
  says so (a message kept back and delivered later, or a clock that's wrong).

A line that doesn't pass any of these is shown as "Couldn't decrypt this message" with the reason (not for this
device, changed, not from the person it says), never as text.

**Sender authentication** is the identity signature; a recipient knows the message came from the person whose
uid it is (and the safety number ties that uid to the person).

### Forward secrecy: what this scheme gives and what it doesn't

Every message has its own content key and its own ephemeral keys, but each is encrypted to the recipients'
long-lived **device keys**. There is no Double Ratchet. The trade-off, plainly:

- Someone who records the ciphertext (the server, or anyone with a copy of its database) and **later** steals a
  device's private key can decrypt every message that was encrypted to that device, past and future, until the
  device key is replaced. With a ratchet only the messages around the theft would be exposed.
- Not having a ratchet is what makes the rest simple and robust: several devices a person, a history that every
  device of the person can read again from the server (nothing to sync between devices), messages that can be
  read in any order, no session state that a crash, a restore or a second tab can corrupt.
- What limits the damage: device keys never leave the device, are sealed by the operating system's secret store,
  and aren't in backups. A device key can be replaced (**Settings › Privacy › New encryption key**): the
  old private key is deleted, so what was encrypted only to it can no longer be read by anyone who steals the device
  afterwards (it also can't be read on that device any more).
- A Double Ratchet (or MLS) can be added later as `"v":2` without changing the server: the server stores opaque
  envelopes either way.

## 3. Attachments

A file in a private chat is encrypted on the device before it's uploaded: a random 32-byte file key and a random
7-byte nonce prefix; the file in 64 KiB pieces, each sealed with AES-256-GCM under the file key with the nonce
`prefix ‖ uint32_be(piece number) ‖ (1 for the last piece, else 0)` — the STREAM construction (Hoang, Reyhanitabar,
Rogaway, Vizár 2015, as in age and Tink). Pieces can't be reordered, dropped or cut off at the end without the
download failing. The upload goes to the ordinary `/files` endpoint under the name `file.sve`, so the server
learns only the size. The link, the key, the prefix, the real name, the type and the size go **inside** the
encrypted message (`att`). Downloading decrypts into the Downloads folder (PC, phone) or into a blob (browser).

## 4. Safety numbers and key changes

**Safety number** ("Verify this person", from the lock in the private chat): for each of the two people,
`h = "sv-safety-v1\n" + uid + "\n" ‖ SPKI`, then 5200 times `h = SHA-512(h ‖ SPKI)`; the first 30 bytes as six
5-digit groups (each 5 bytes as a big-endian number mod 100000). The two halves, the smaller uid first, make 60
digits in 12 groups: the same on both phones. Read them out to each other, in person or on a call you trust; when
they match, mark the person **verified** (kept on this device).

Because the uid is the hash of the identity key, a person's identity key can't change "under" their uid. The
warnings the apps show:

- **New or changed device keys** (a new device, a reinstall): the apps remember the devices they saw for each
  person (`e2e.properties`: device id and SHA-256 of its key). When the server's list has a device not seen before,
  or a known device with another key, the private chat shows "X's encryption keys changed" with *Verify* and *OK*.
  Messages still go (new devices are signed by X's identity key); a person you verified stays verified, since the
  safety number didn't change, but the warning stays until you dismiss it.
- **Another identity under a known name**: the apps remember the name each person had; a private chat with
  someone whose name is that of another person you have talked to (or verified) says so: "This isn't the X you
  talked to before: a different identity". This is the case a safety number exists for (someone, or the server,
  showing a new key as an old friend).
- **Keys the server sent that don't check** (a record whose signature or uid doesn't match): left out, and the
  chat says the server sent keys that aren't X's.

## 5. Migration and old apps

- Private messages written before this (plaintext on the server) stay readable and are marked
  **"Not encrypted (older)"**. A plaintext private line that arrives now (from an old app, or made up by a server)
  carries the same mark: no plaintext line is ever shown as encrypted.
- New apps never send private messages in plaintext. If the other person has no device key yet (an old app), the
  chat says **"This person's app doesn't support encrypted messages yet"** and the message box is off. There is no
  silent fall back to plaintext. The server refuses a plaintext private `say` from a session that published a
  device key (an app that can encrypt must).
- An old app gets the new encrypted lines with an empty `text` and the `e2e` field it doesn't know: it shows empty
  lines (old apps don't know better); the people on old apps see the "doesn't support" note on the other side.
- Bots don't publish device keys: a bot can't be written to privately by the new apps.

## 6. The server

- `e2e-publish {"rec":…}` → `e2e-published`; the people online are told `e2e-changed {"uid"}`.
- `e2e-keys {"uid"}` → `e2e-keys {"uid","devices":[rec…]}`, only for oneself and people of this server.
  12 a second a session (bursts of 30).
- `e2e-say {"to":"u:<uid>","e":"<envelope>","n"}` → `msg` with `"e2e"` (and `text` ""); 48 KB at most, 5 a second a
  session (bursts of 20), only to people of the server, refused while muted.
- `unsay` takes back an encrypted line (the envelope is deleted); `edit` is refused for them (an edit would need a new
  envelope; take it back and write again).
- History, `dms`, catch-up after a reconnect: the same as before, with `e2e` in the items.
- **Search**: the server no longer searches private chats at all (`to: "u:…"` is refused; `"*"` covers channels and
  the server chat). The apps search the private chats themselves, over what they have loaded and decrypted.
- **Push**: a private message wakes the phone with no text. Without `SV_PUSH_CONTENT`: `{"kind":"dm"}` only. With it:
  the sender's name as the title and an empty body (the app shows "New message from X"), never the words.
- Erasing a person (`wipe`) deletes their device records too.

## 7. What the apps keep on the device

Nothing decrypted is written to disk. Decrypted private messages live in memory while the app runs; the history
is fetched (encrypted) from the server and decrypted again when a chat is opened. This keeps a single copy of the
history (the server's ciphertext), nothing to clean up on a shared PC, and nothing a backup could leak.
Search over private chats is over what is loaded in memory. On disk: the device private key (sealed, §1), and the
per-person pins and verified marks (`e2e.properties`: no secrets; tampering with it would only make warnings come
or go on that device).

A consequence: a new device (a new phone, a reinstall, another browser) can't read private messages sent before
it existed, which were encrypted only to the devices of that time. They show as "Sent before this device had its
key".

## 8. Limits, plainly

- Metadata: the server sees who writes to whom, when, how often, the size (rounded to 128 bytes), and which devices
  a person has. The sender's name is in the server's row, as before.
- The server can withhold, delay or drop messages, and leave a device out of the list it gives (then that device
  can't read what's sent meanwhile). It can't read, change, reorder unnoticed, or forge them.
- No ratchet (§2): a stolen device key opens what was sent to that device.
- Trust on first use: until you compare safety numbers, you trust that the uid in front of you is the person.
- The browser client's code comes from the server it runs on: a hostile server could serve a page that leaks keys.
  For people who don't trust the server, the PC and phone apps are the safe choice; the web client's DMs protect
  against a database copy, not against the operator changing the page.
