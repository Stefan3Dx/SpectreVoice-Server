package main

// End-to-end encrypted private messages. The apps encrypt a private message to every device of both people
// (ECDH P-256 + HKDF-SHA256 + AES-256-GCM, signed with the sender's identity key) and the server keeps and passes on
// only that envelope: it never sees the words or what an attachment holds. The scheme is described in docs/E2E.md
// and implemented in web/e2e.js here and core/E2eCrypto.kt in the apps.
//
//	-> {"t":"e2e-publish","rec":{"v":1,"uid","dev","ik","dk","ts","sig"}}   a device's key, signed by the identity
//	<- {"t":"e2e-published","dev":…}                                       (everyone else: {"t":"e2e-changed","uid"})
//	-> {"t":"e2e-keys","uid":…}                                            oneself or someone of this server
//	<- {"t":"e2e-keys","uid":…,"devices":[rec…]}
//	-> {"t":"e2e-say","to":"u:<uid>","e":"<envelope JSON>","n":…}
//	<- {"t":"msg","m":{"id","to":"d:…","from","name","text":"","at","e2e":"<envelope JSON>"},"n"}
//
// The server checks only what it knows anyway (who sends, to whom, the size) and that device records are signed by
// the key their uid is the hash of. Search never looks into private chats (search.go); a push for one has no text.

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	e2eMaxEnvelope = 48 << 10 // an envelope: 4000 characters, padded, wrapped for up to 20 devices
	e2eMaxRecord   = 2 << 10
	e2eMaxDevices  = 10 // a person's devices kept; the one seen longest ago goes
	e2eMaxKeys     = 32 // wrapped keys in one envelope
)

// Vars so tests can change them.
var (
	e2eDeviceIdle           = 180 * 24 * time.Hour
	e2eSayRate      float64 = 5
	e2eSayBurst     float64 = 20
	e2eKeysRate     float64 = 12
	e2eKeysBurst    float64 = 30
	e2eDevPattern           = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	e2eMidPattern           = regexp.MustCompile(`^[A-Za-z0-9_-]{16,32}$`)
	e2eSignedPrefix         = "spectrevoice-e2e-device-v1\n"
)

// bucket: a token budget (its session's read loop only).
type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) take(rate, burst float64) bool {
	now := time.Now()
	if b.at.IsZero() {
		b.tokens = burst
	} else {
		b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// e2eRecord: a device's published key, as the apps sign it.
type e2eRecord struct {
	V   int    `json:"v"`
	UID string `json:"uid"`
	Dev string `json:"dev"`
	IK  string `json:"ik"`
	DK  string `json:"dk"`
	TS  int64  `json:"ts"`
	Sig string `json:"sig"`
}

// p256 parses a base64 SPKI that must be a P-256 key.
func p256(b64 string) ([]byte, *ecdsa.PublicKey, bool) {
	if len(b64) == 0 || len(b64) > 300 {
		return nil, nil, false
	}
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, nil, false
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, nil, false
	}
	ek, ok := k.(*ecdsa.PublicKey)
	if !ok || ek.Curve.Params().Name != "P-256" {
		return nil, nil, false
	}
	return der, ek, true
}

// check: a well-formed record of uid's device dev, signed by the identity whose hash uid is.
func (r *e2eRecord) check(uid, dev string, now time.Time) string {
	switch {
	case r.V != 1:
		return "version"
	case r.UID != uid:
		return "not yours"
	case !e2eDevPattern.MatchString(r.Dev) || (dev != "" && r.Dev != dev):
		return "device"
	case r.TS <= 0 || r.TS > now.Add(24*time.Hour).UnixMilli():
		return "time"
	}
	ikDER, ik, ok := p256(r.IK)
	if !ok || uidOf(ikDER) != uid {
		return "identity key"
	}
	if _, _, ok := p256(r.DK); !ok {
		return "device key"
	}
	sig, err := base64.StdEncoding.DecodeString(r.Sig)
	if err != nil || len(sig) == 0 || len(sig) > 80 {
		return "signature"
	}
	h := sha256.Sum256([]byte(e2eSignedPrefix + r.UID + "\n" + r.Dev + "\n" + r.IK + "\n" + r.DK + "\n" + strconv.FormatInt(r.TS, 10)))
	if !ecdsa.VerifyASN1(ik, h[:], sig) {
		return "signature"
	}
	return ""
}

// e2eOp: the end-to-end encryption messages; false when t isn't one.
func (h *hub) e2eOp(ss *session, t string, m map[string]any) bool {
	switch t {
	case "e2e-publish":
		h.e2ePublish(ss, m)
	case "e2e-keys":
		if !h.staff(ss.uid) && !ss.e2eKeys.take(e2eKeysRate, e2eKeysBurst) {
			ss.refuse(t, "rate", "slow down")
			return true
		}
		uid := str(m, "uid")
		// Only oneself and people of this server: nobody learns another server's people's devices through this one
		if !uidRe.MatchString(uid) || (uid != ss.uid && !h.s.store.isMember(spaceID, uid)) {
			ss.refuse(t, "gone", "they aren't on this server")
			return true
		}
		devs, err := h.s.store.e2eDevices(uid, time.Now().Add(-e2eDeviceIdle))
		if err != nil {
			ss.refuse(t, "server", "can't read the keys")
			return true
		}
		ss.send(map[string]any{"t": "e2e-keys", "uid": uid, "devices": devs})
	case "e2e-say":
		h.e2eSay(ss, m)
	default:
		return false
	}
	return true
}

func (h *hub) e2ePublish(ss *session, m map[string]any) {
	raw, _ := json.Marshal(m["rec"])
	if len(raw) > e2eMaxRecord {
		ss.refuse("e2e-publish", "bad", "too long")
		return
	}
	var r e2eRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		ss.refuse("e2e-publish", "bad", "not a device record")
		return
	}
	if ss.dev == "" {
		ss.refuse("e2e-publish", "bad", "the hello named no device")
		return
	}
	now := time.Now()
	if why := r.check(ss.uid, ss.dev, now); why != "" {
		ss.refuse("e2e-publish", "bad", "device record: "+why)
		return
	}
	changed, err := h.s.store.e2ePut(&r, raw, now, now.Add(-e2eDeviceIdle))
	if err != nil {
		if err == errE2eOlder {
			ss.refuse("e2e-publish", "old", "a newer key is published for this device")
			return
		}
		log.Printf("e2e: %v", err)
		ss.refuse("e2e-publish", "server", "not saved")
		return
	}
	ss.e2e = true
	ss.send(map[string]any{"t": "e2e-published", "dev": r.Dev})
	if changed {
		// The people here look again before they next write to them (or to their other devices)
		h.mu.Lock()
		h.broadcast(map[string]any{"t": "e2e-changed", "uid": ss.uid}, func(o *session) bool { return o != ss && o.comm == ss.comm })
		h.mu.Unlock()
	}
}

// e2eEnvelope: what the server reads of an envelope (all of it is the sender's; only these are checked).
type e2eEnvelope struct {
	V    int               `json:"v"`
	From string            `json:"from"`
	To   string            `json:"to"`
	Mid  string            `json:"mid"`
	Keys []json.RawMessage `json:"keys"`
}

func (h *hub) e2eSay(ss *session, m map[string]any) {
	const t = "e2e-say"
	if !h.staff(ss.uid) && !ss.e2eSay.take(e2eSayRate, e2eSayBurst) {
		ss.refuse(t, "rate", "slow down")
		return
	}
	to := str(m, "to")
	if !strings.HasPrefix(to, "u:") {
		ss.refuse(t, "bad", "only private messages are end-to-end encrypted")
		return
	}
	conv, _, other, err := h.target(ss, to)
	if err != nil || other == "" {
		ss.refuse(t, "bad", "bad recipient")
		return
	}
	if h.s.mods.muted(ss.uid, spaceID) {
		ss.refuse(t, "muted", "you can't write here for now")
		return
	}
	if !h.s.store.isMember(spaceID, other) {
		ss.refuse(t, "bad", "they aren't on this server")
		return
	}
	env := str(m, "e")
	if env == "" || len(env) > e2eMaxEnvelope {
		ss.refuse(t, "full", "empty or too long")
		return
	}
	var e e2eEnvelope
	if err := json.Unmarshal([]byte(env), &e); err != nil || e.V != 1 || e.From != ss.uid || e.To != other ||
		!e2eMidPattern.MatchString(e.Mid) || len(e.Keys) == 0 || len(e.Keys) > e2eMaxKeys {
		ss.refuse(t, "bad", "not an envelope from you to them")
		return
	}
	msg := &chatMsg{To: conv, From: ss.uid, Name: ss.name, At: time.Now().UnixMilli(), Enc: env}
	if err := h.s.store.addMessage(msg); err != nil {
		log.Printf("chat: %v", err)
		ss.refuse(t, "server", "not saved")
		return
	}
	statMessages.Add(1)
	out := map[string]any{"t": "msg", "m": msg}
	if n := str(m, "n"); n != "" {
		out["n"] = clip(n, 64)
	}
	h.mu.Lock()
	h.deliver(out, 0, ss.uid, other)
	h.mu.Unlock()
	if !ss.api {
		go h.pushFor(ss, msg, 0, other)
	}
}

var errE2eOlder = &e2eError{"older"}

type e2eError struct{ s string }

func (e *e2eError) Error() string { return e.s }

// e2ePut keeps a device's record (a newer one of the same device replaces it; an older one is refused), drops the
// person's devices beyond e2eMaxDevices (seen longest ago first) and everyone's not seen since idle. changed: the
// device or its key is new.
func (st *store) e2ePut(r *e2eRecord, raw []byte, now, idle time.Time) (changed bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var oldTS int64
	var oldRec string
	err = st.db.QueryRow(`SELECT ts, rec FROM e2e_devices WHERE uid=? AND dev=?`, r.UID, r.Dev).Scan(&oldTS, &oldRec)
	switch {
	case err == nil && oldTS > r.TS:
		return false, errE2eOlder
	case err == nil:
		var old e2eRecord
		_ = json.Unmarshal([]byte(oldRec), &old)
		changed = old.DK != r.DK
	default:
		changed = true
	}
	if _, err = st.db.Exec(`INSERT INTO e2e_devices(uid,dev,rec,ts,seen) VALUES(?,?,?,?,?) ON CONFLICT(uid,dev) DO UPDATE SET rec=excluded.rec, ts=excluded.ts, seen=excluded.seen`,
		r.UID, r.Dev, string(raw), r.TS, now.UnixMilli()); err != nil {
		return false, err
	}
	_, _ = st.db.Exec(`DELETE FROM e2e_devices WHERE uid=? AND dev NOT IN (SELECT dev FROM e2e_devices WHERE uid=? ORDER BY seen DESC LIMIT ?)`, r.UID, r.UID, e2eMaxDevices)
	_, _ = st.db.Exec(`DELETE FROM e2e_devices WHERE seen < ?`, idle.UnixMilli())
	return changed, nil
}

// e2eDevices: uid's device records seen since idle, by device id.
func (st *store) e2eDevices(uid string, idle time.Time) ([]json.RawMessage, error) {
	rows, err := st.db.Query(`SELECT rec FROM e2e_devices WHERE uid=? AND seen >= ? ORDER BY dev`, uid, idle.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var rec string
		if rows.Scan(&rec) == nil {
			out = append(out, json.RawMessage(rec))
		}
	}
	return out, rows.Err()
}
