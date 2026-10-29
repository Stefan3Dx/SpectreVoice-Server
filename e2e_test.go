package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// e2eRec makes a device record for key's identity, signed as the apps sign it.
func e2eRec(t *testing.T, key *ecdsa.PrivateKey, dev string, ts int64) map[string]any {
	t.Helper()
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	dk, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dkDER, _ := x509.MarshalPKIXPublicKey(dk.PublicKey())
	ik, dks := base64.StdEncoding.EncodeToString(spki), base64.StdEncoding.EncodeToString(dkDER)
	uid := uidOf(spki)
	h := sha256.Sum256([]byte(e2eSignedPrefix + uid + "\n" + dev + "\n" + ik + "\n" + dks + "\n" + strconv.FormatInt(ts, 10)))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, h[:])
	return map[string]any{"v": 1, "uid": uid, "dev": dev, "ik": ik, "dk": dks, "ts": ts, "sig": base64.StdEncoding.EncodeToString(sig)}
}

// e2eEnv: an envelope as the apps make it, with secret really sealed inside (AES-256-GCM, a random key).
func e2eEnv(t *testing.T, from, to, secret string) string {
	t.Helper()
	k := make([]byte, 32)
	iv := make([]byte, 12)
	_, _ = rand.Read(k)
	_, _ = rand.Read(iv)
	b, _ := aes.NewCipher(k)
	g, _ := cipher.NewGCM(b)
	ct := g.Seal(nil, iv, []byte(`{"text":`+strconv.Quote(secret)+`}`), nil)
	mid := make([]byte, 16)
	_, _ = rand.Read(mid)
	env, _ := json.Marshal(map[string]any{"v": 1, "from": from, "fdev": "dev-a", "to": to, "ts": time.Now().UnixMilli(),
		"mid": base64.RawURLEncoding.EncodeToString(mid), "iv": base64.StdEncoding.EncodeToString(iv), "ct": base64.StdEncoding.EncodeToString(ct),
		"keys": []any{map[string]any{"to": to, "dev": "x", "epk": "AA==", "iv": "AA==", "wk": "AA=="}}, "sig": "AA=="})
	return string(env)
}

func refusal(c *tclient, op string) map[string]any {
	return c.wait(func(m map[string]any) bool { return m["t"] == "error" && m["op"] == op })
}

// Device keys: published only signed by the identity whose hash the uid is, for the device of the hello; newer
// replaces older (an older one is refused); looked up only for oneself and people of this server; the others told.
func TestE2EDeviceKeys(t *testing.T) {
	_, hs := newTestServer(t, nil)
	ka, kb := newKey(t), newKey(t)
	a, _ := dialVWith(t, hs, ka, "Ana", map[string]any{"dev": "dev-a"})
	b, _ := dialVWith(t, hs, kb, "Bogdan", map[string]any{"dev": "dev-b"})

	now := time.Now().UnixMilli()
	a.send(map[string]any{"t": "e2e-publish", "rec": e2eRec(t, ka, "dev-a", now)})
	if m := a.wait(isT("e2e-published")); m["dev"] != "dev-a" {
		t.Fatal(m)
	}
	if m := b.wait(isT("e2e-changed")); m["uid"] != a.uid {
		t.Fatal(m)
	}
	b.send(map[string]any{"t": "e2e-keys", "uid": a.uid})
	r := b.wait(isT("e2e-keys"))
	devs := r["devices"].([]any)
	if r["uid"] != a.uid || len(devs) != 1 || devs[0].(map[string]any)["dev"] != "dev-a" || devs[0].(map[string]any)["sig"] == "" {
		t.Fatal(r)
	}

	// Someone else's identity, another device than the hello's, a broken signature, a record from the future
	bad := []map[string]any{e2eRec(t, kb, "dev-a", now), e2eRec(t, ka, "dev-other", now), e2eRec(t, ka, "dev-a", now+5), e2eRec(t, ka, "dev-a", now+48*3600_000)}
	bad[2]["ts"] = now + 6
	for i, rec := range bad {
		a.send(map[string]any{"t": "e2e-publish", "rec": rec})
		if m := refusal(a, "e2e-publish"); m["why"] != "bad" {
			t.Fatal(i, m)
		}
	}
	// An older record than the one kept: refused (a server can't be made to roll a device back by its own app)
	a.send(map[string]any{"t": "e2e-publish", "rec": e2eRec(t, ka, "dev-a", now-1000)})
	if m := refusal(a, "e2e-publish"); m["why"] != "old" {
		t.Fatal(m)
	}
	// A newer one replaces it
	newer := e2eRec(t, ka, "dev-a", now+1000)
	a.send(map[string]any{"t": "e2e-publish", "rec": newer})
	a.wait(isT("e2e-published"))
	a.send(map[string]any{"t": "e2e-keys", "uid": a.uid})
	if devs := a.wait(isT("e2e-keys"))["devices"].([]any); len(devs) != 1 || devs[0].(map[string]any)["dk"] != newer["dk"] {
		t.Fatal(devs)
	}
	// A uid that isn't on this server: nothing said about it
	b.send(map[string]any{"t": "e2e-keys", "uid": newUID(t)})
	if m := refusal(b, "e2e-keys"); m["why"] != "gone" {
		t.Fatal(m)
	}
	// No device in the hello: nothing to publish for
	kc := newKey(t)
	c, _ := dialV(t, hs, kc, "Cristi", "")
	c.send(map[string]any{"t": "e2e-publish", "rec": e2eRec(t, kc, "dev-c", now)})
	if m := refusal(c, "e2e-publish"); m["why"] != "bad" {
		t.Fatal(m)
	}
}

// A person keeps at most e2eMaxDevices; the one seen longest ago goes, and devices not seen for long go.
func TestE2EDevicePruning(t *testing.T) {
	s, _ := newTestServer(t, nil)
	key := newKey(t)
	now := time.Now()
	for i := 0; i < e2eMaxDevices+3; i++ {
		rec := e2eRec(t, key, "d"+itoa(i), now.UnixMilli())
		raw, _ := json.Marshal(rec)
		var r e2eRecord
		_ = json.Unmarshal(raw, &r)
		if why := r.check(r.UID, r.Dev, now); why != "" {
			t.Fatal(why)
		}
		if _, err := s.store.e2ePut(&r, raw, now.Add(time.Duration(i)*time.Second), now.Add(-e2eDeviceIdle)); err != nil {
			t.Fatal(err)
		}
	}
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	devs, _ := s.store.e2eDevices(uidOf(spki), now.Add(-e2eDeviceIdle))
	if len(devs) != e2eMaxDevices {
		t.Fatal(len(devs))
	}
	for _, d := range devs {
		if strings.Contains(string(d), `"dev":"d0"`) || strings.Contains(string(d), `"dev":"d1"`) || strings.Contains(string(d), `"dev":"d2"`) {
			t.Fatal("the oldest stayed", string(d))
		}
	}
	// Not seen for longer than e2eDeviceIdle: not handed out
	if devs, _ := s.store.e2eDevices(uidOf(spki), now.Add(time.Hour)); len(devs) != 0 {
		t.Fatal(len(devs))
	}
}

// What the server keeps of an encrypted private message is the envelope alone: no words in any column, nothing for
// search to find, no text in the push; history and dms hand the envelope out as it came; an app that published its
// key can't send plaintext privately, an old one still can (marked by the apps); edit refused, unsay clears it.
func TestE2EStoredOpaque(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.pushContent = true })
	p := &pusher{queue: make(chan pushJob, 16)}
	s.push = p
	ka, kb := newKey(t), newKey(t)
	a, _ := dialVWith(t, hs, ka, "Ana", map[string]any{"dev": "dev-a"})
	b, _ := dialVWith(t, hs, kb, "Bogdan", map[string]any{"dev": "dev-b"})
	a.send(map[string]any{"t": "e2e-publish", "rec": e2eRec(t, ka, "dev-a", time.Now().UnixMilli())})
	a.wait(isT("e2e-published"))

	const secret = "the eagle lands at midnight"
	env := e2eEnv(t, a.uid, b.uid, secret)
	a.send(map[string]any{"t": "e2e-say", "to": "u:" + b.uid, "e": env, "n": "x1"})
	got := b.wait(isT("msg"))["m"].(map[string]any)
	if got["text"] != "" || got["e2e"] != env || got["from"] != a.uid {
		t.Fatal(got)
	}
	if m := a.wait(isT("msg")); m["n"] != "x1" {
		t.Fatal(m)
	}
	// Every column of every row: no words
	rows, err := s.store.db.Query(`SELECT conv, from_uid, from_name, text, enc FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var conv, from, name, text, enc string
		_ = rows.Scan(&conv, &from, &name, &text, &enc)
		n++
		for _, v := range []string{conv, from, name, text, enc} {
			if strings.Contains(strings.ToLower(v), "eagle") || strings.Contains(v, base64.StdEncoding.EncodeToString([]byte(secret))[:12]) {
				t.Fatalf("the words are on the server: %q", v)
			}
		}
		if text != "" || enc != env {
			t.Fatalf("text %q enc %q", text, enc)
		}
	}
	rows.Close()
	if n != 1 {
		t.Fatal(n)
	}
	// Search: private chats aren't searched by the server at all
	b.send(map[string]any{"t": "search", "q": "eagle", "to": "*"})
	if r := b.wait(isT("search")); len(r["items"].([]any)) != 0 {
		t.Fatal(r)
	}
	b.send(map[string]any{"t": "search", "q": "eagle", "to": "u:" + a.uid})
	if m := refusal(b, "search"); m["why"] != "e2e" {
		t.Fatal(m)
	}
	// The history and the list of private chats: the envelope as it came
	b.send(map[string]any{"t": "history", "to": "u:" + a.uid})
	h := b.wait(isT("history"))
	if items := h["items"].([]any); len(items) != 1 || items[0].(map[string]any)["e2e"] != env {
		t.Fatal(h)
	}
	b.send(map[string]any{"t": "dms"})
	if d := b.wait(isT("dms"))["items"].([]any); len(d) != 1 || d[0].(map[string]any)["last"].(map[string]any)["e2e"] != env {
		t.Fatal(d)
	}
	// The push for someone offline: who wrote, never the words (not even with SV_PUSH_CONTENT)
	b.ws.Close()
	time.Sleep(200 * time.Millisecond)
	a.send(map[string]any{"t": "e2e-say", "to": "u:" + b.uid, "e": e2eEnv(t, a.uid, b.uid, secret)})
	select {
	case j := <-p.queue:
		if j.uid != b.uid || j.data["kind"] != "dm" || j.data["body"] != "" || j.data["title"] != "Ana" {
			t.Fatal(j.data)
		}
		for k, v := range j.data {
			if strings.Contains(v, "eagle") || strings.Contains(v, "\"ct\"") {
				t.Fatal(k, v)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no push")
	}
	b, _ = dialVWith(t, hs, kb, "Bogdan", map[string]any{"dev": "dev-b"})

	// An app that published its key: no plaintext privately; an old one (no key) still writes as before
	a.send(map[string]any{"t": "say", "to": "u:" + b.uid, "text": "plain words"})
	if m := refusal(a, "say"); m["why"] != "e2e" {
		t.Fatal(m)
	}
	b.send(map[string]any{"t": "say", "to": "u:" + a.uid, "text": "old app words"})
	if m := a.wait(func(m map[string]any) bool {
		return m["t"] == "msg" && m["m"].(map[string]any)["from"] == b.uid
	})["m"].(map[string]any); m["text"] != "old app words" || m["e2e"] != nil {
		t.Fatal(m)
	}
	// An encrypted line can't be edited (the server can't rewrite it), its writer can take it back
	id := got["id"].(float64)
	a.send(map[string]any{"t": "edit", "id": id, "text": "changed"})
	if m := refusal(a, "edit"); m["why"] != "e2e" {
		t.Fatal(m)
	}
	a.send(map[string]any{"t": "unsay", "id": id})
	if m := b.wait(func(m map[string]any) bool {
		return m["t"] == "msg" && m["m"].(map[string]any)["id"] == id
	})["m"].(map[string]any); m["del"] != true || m["e2e"] != nil {
		t.Fatal(m)
	}
	var enc string
	_ = s.store.db.QueryRow(`SELECT enc FROM messages WHERE id=?`, int64(id)).Scan(&enc)
	if enc != "" {
		t.Fatal("the envelope stayed after unsay")
	}
}

// What e2e-say refuses: an envelope that isn't from the sender to the recipient, too long, to a channel, to someone not
// on the server, too fast.
func TestE2ESayRefusals(t *testing.T) {
	_, hs := newTestServer(t, nil)
	a, _ := dialVWith(t, hs, newKey(t), "Ana", map[string]any{"dev": "dev-a"})
	b, _ := dialVWith(t, hs, newKey(t), "Bogdan", map[string]any{"dev": "dev-b"})
	lobby := chanID(t, a.wel, "Lobby")
	cases := []map[string]any{
		{"to": "u:" + b.uid, "e": e2eEnv(t, b.uid, b.uid, "x")},                      // not from a
		{"to": "u:" + b.uid, "e": e2eEnv(t, a.uid, a.uid, "x")},                      // not to b
		{"to": "u:" + b.uid, "e": "not json"},                                        // not an envelope
		{"to": "u:" + b.uid, "e": strings.Repeat("x", e2eMaxEnvelope+1)},             // too long
		{"to": "c:" + itoa(int(lobby)), "e": e2eEnv(t, a.uid, b.uid, "x")},           // a channel
		{"to": "u:" + newUID(t), "e": e2eEnv(t, a.uid, "zzzzzzzzzzzzzzzzzzzz", "x")}, // not on the server
	}
	for i, c := range cases {
		c["t"] = "e2e-say"
		a.send(c)
		if m := refusal(a, "e2e-say"); m["why"] != "bad" && m["why"] != "full" {
			t.Fatal(i, m)
		}
	}
	// Too fast: the budget (20 at once, 5 a second)
	refused := false
	for i := 0; i < 40; i++ {
		a.send(map[string]any{"t": "e2e-say", "to": "u:" + b.uid, "e": e2eEnv(t, a.uid, b.uid, "fast")})
	}
	for i := 0; i < 40; i++ {
		m := a.wait(func(m map[string]any) bool { return m["t"] == "msg" || (m["t"] == "error" && m["op"] == "e2e-say") })
		if m["t"] == "error" {
			if m["why"] != "rate" {
				t.Fatal(m)
			}
			refused = true
			break
		}
	}
	if !refused {
		t.Fatal("never slowed down")
	}
}

// Two browsers (two identities) exchange end-to-end encrypted private messages and a picture through this server,
// in headless Chromium (web/e2e/dm-run.js): SV_WEB_E2E=1 go test -run TestWebDmE2E -v . (SV_SHOTS=<dir> keeps two
// screenshots). Meanwhile the database and the files folder never hold the words or the picture.
func TestWebDmE2E(t *testing.T) {
	if os.Getenv("SV_WEB_E2E") != "1" {
		t.Skip("set SV_WEB_E2E=1 (needs node, playwright and its Chromium)")
	}
	s, hs := newTestServer(t, func(c *config) { c.files = true; c.fileMB = 10; c.fileDays = 1 })
	secret := "meet me at the old mill " + itoa(int(time.Now().UnixNano()%1000000))
	args := []string{"web/e2e/dm-run.js", hs.URL, secret}
	if d := os.Getenv("SV_SHOTS"); d != "" {
		args = append(args, d)
	}
	cmd := exec.Command("node", args...)
	cmd.Stderr = &testWriter{t}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	sent, ok := false, false
	for sc.Scan() {
		t.Log("dm e2e:", sc.Text())
		switch sc.Text() {
		case "SENT":
			sent = true
			var n int
			_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE text LIKE ? OR enc LIKE ? OR from_name LIKE ?`, "%old mill%", "%old mill%", "%old mill%").Scan(&n)
			var enc int
			_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv LIKE 'd:%' AND text='' AND enc LIKE '{"v":1,%'`).Scan(&enc)
			if n != 0 || enc != 1 {
				t.Errorf("the words are on the server (%d rows), or the envelope isn't (%d)", n, enc)
			}
		case "OK":
			ok = true
		}
	}
	if err := cmd.Wait(); err != nil || !sent || !ok {
		t.Fatalf("dm e2e: %v (sent %v, ok %v)", err, sent, ok)
	}
	// The uploaded picture is ciphertext: no PNG on the disk, and it went up as file.sve
	_ = filepath.WalkDir(filepath.Join(s.cfg.data, "files"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == spaceMark {
			return nil
		}
		b, _ := os.ReadFile(p)
		if d.Name() != "file.sve" || strings.HasPrefix(string(b), "\x89PNG") {
			t.Errorf("a file in the clear: %s", p)
		}
		return nil
	})
}

// A private message's push never carries its words, not even a plaintext one from an older app and not even with
// SV_PUSH_CONTENT (docs/E2E.md §6): who wrote, nothing more.
func TestPushNoWordsForPlainPrivate(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.pushContent = true })
	p := &pusher{queue: make(chan pushJob, 16)}
	s.push = p
	a, _ := dialVWith(t, hs, newKey(t), "Ana", nil)
	kb := newKey(t)
	b, _ := dialVWith(t, hs, kb, "Bogdan", nil)
	buid := b.uid
	b.ws.Close()
	time.Sleep(200 * time.Millisecond)
	a.send(map[string]any{"t": "say", "to": "u:" + buid, "text": "the eagle lands at midnight"})
	select {
	case j := <-p.queue:
		if j.uid != buid || j.data["kind"] != "dm" || j.data["title"] != "Ana" {
			t.Fatal(j.data)
		}
		for k, v := range j.data {
			if strings.Contains(v, "eagle") {
				t.Fatalf("the words went in the push: %s=%q", k, v)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no push")
	}
}
