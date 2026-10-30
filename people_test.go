package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func memberUIDs(m map[string]any) (uids []string, online map[string]bool) {
	online = map[string]bool{}
	for _, x := range m["items"].([]any) {
		p := x.(map[string]any)
		uids = append(uids, p["uid"].(string))
		online[p["uid"].(string)] = p["online"] == true
	}
	return
}

// gone closes c and waits until the server let its session go (others see member-left).
func gone(t *testing.T, c, watcher *tclient) {
	t.Helper()
	_ = c.ws.Close()
	watcher.wait(func(m map[string]any) bool { return m["t"] == "member-left" && m["uid"] == c.uid })
	time.Sleep(100 * time.Millisecond)
}

// The member list: everyone who ever came in, online first, then the offline ones; no last-seen unless the server
// shows it; the server-banned and the people one blocked aren't in it; search and pages.
func TestMembersListWithOffline(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	ana, wel := dialV(t, hs, ownerKey, "Ana", "")
	if srv := wel; srv != nil {
		t.Fatal(srv)
	}
	if ana.wel["server"].(map[string]any)["people"] != true {
		t.Fatal("the welcome doesn't say the server has the member list")
	}
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	cri, _ := dialV(t, hs, newKey(t), "Cristi", "")
	dan, _ := dialV(t, hs, newKey(t), "Dan", "")
	gone(t, bob, ana)
	gone(t, dan, ana)

	ana.send(map[string]any{"t": "members"})
	m := ana.wait(isT("members"))
	uids, online := memberUIDs(m)
	if len(uids) != 4 || m["total"].(float64) != 4 || m["online"].(float64) != 2 {
		t.Fatalf("members %v", m)
	}
	// Online first (Ana, Cristi), then the offline ones (Bob, Dan), by name
	want := []string{ana.uid, cri.uid, bob.uid, dan.uid}
	for i := range want {
		if uids[i] != want[i] {
			t.Fatalf("order %v, want %v", uids, want)
		}
	}
	if !online[ana.uid] || !online[cri.uid] || online[bob.uid] || online[dan.uid] {
		t.Fatalf("online %v", online)
	}
	for _, x := range m["items"].([]any) {
		if _, has := x.(map[string]any)["seen"]; has {
			t.Fatalf("last seen shown without SV_MEMBERS_LAST_SEEN: %v", x)
		}
	}
	// Only the offline ones
	ana.send(map[string]any{"t": "members", "offline": true})
	if p := ana.wait(isT("members")); p["total"].(float64) != 2 || len(p["items"].([]any)) != 2 || p["offline"] != true {
		t.Fatal(p)
	}
	// Search and pages
	ana.send(map[string]any{"t": "members", "q": "BO"})
	if u, _ := memberUIDs(ana.wait(isT("members"))); len(u) != 1 || u[0] != bob.uid {
		t.Fatal(u)
	}
	ana.send(map[string]any{"t": "members", "offset": 1, "limit": 2})
	if p := ana.wait(isT("members")); len(p["items"].([]any)) != 2 || p["total"].(float64) != 4 || p["offset"].(float64) != 1 {
		t.Fatal(p)
	}
	// Server-banned: gone from the list
	ana.send(map[string]any{"t": "server-ban", "uid": dan.uid})
	time.Sleep(200 * time.Millisecond)
	ana.send(map[string]any{"t": "members"})
	if u, _ := memberUIDs(ana.wait(isT("members"))); len(u) != 3 {
		t.Fatalf("a banned person listed: %v", u)
	}
	// Blocked: hidden for the one who blocked
	cri.send(map[string]any{"t": "block", "uid": bob.uid})
	cri.wait(isT("blocks"))
	cri.send(map[string]any{"t": "members"})
	if u, _ := memberUIDs(cri.wait(isT("members"))); len(u) != 2 {
		t.Fatalf("a blocked person listed: %v", u)
	}
	// Last seen, when the server shows it
	s.cfg.membersSeen = true
	ana.send(map[string]any{"t": "members", "q": "bob"})
	p := ana.wait(isT("members"))
	if it := p["items"].([]any)[0].(map[string]any); it["seen"] == nil || it["seen"].(float64) < float64(time.Now().Add(-time.Minute).UnixMilli()) {
		t.Fatalf("last seen: %v", it)
	}
}

// A private message to someone offline is kept (their devices' envelope) and waits: on their next connect the
// dms list says it's unread; read on one device, it's read on the others too and after a reconnect.
func TestOfflineDMAndReadMarkers(t *testing.T) {
	_, hs := newTestServer(t, nil)
	ka, kb := newKey(t), newKey(t)
	ana, _ := dialVWith(t, hs, ka, "Ana", map[string]any{"dev": "dev-a"})
	bob, _ := dialVWith(t, hs, kb, "Bob", map[string]any{"dev": "dev-b"})
	bob.send(map[string]any{"t": "e2e-publish", "rec": e2eRec(t, kb, "dev-b", time.Now().UnixMilli())})
	bob.wait(isT("e2e-published"))
	gone(t, bob, ana)

	env1, env2 := e2eEnv(t, ana.uid, bob.uid, "one"), e2eEnv(t, ana.uid, bob.uid, "two")
	ana.send(map[string]any{"t": "e2e-say", "to": "u:" + bob.uid, "e": env1})
	ana.wait(isT("msg"))
	ana.send(map[string]any{"t": "e2e-say", "to": "u:" + bob.uid, "e": env2})
	last := ana.wait(isT("msg"))["m"].(map[string]any)

	// Bob comes back on two devices
	b1, _ := dialVWith(t, hs, kb, "Bob", map[string]any{"dev": "dev-b"})
	b2, _ := dialVWith(t, hs, kb, "Bob", map[string]any{"dev": "dev-b2"})
	b1.send(map[string]any{"t": "dms"})
	d := b1.wait(isT("dms"))["items"].([]any)[0].(map[string]any)
	if d["with"] != ana.uid || d["unread"].(float64) != 2 || d["read"].(float64) != 0 || d["last"].(map[string]any)["e2e"] != env2 {
		t.Fatalf("dms %v", d)
	}
	b1.send(map[string]any{"t": "history", "to": "u:" + ana.uid})
	if h := b1.wait(isT("history")); len(h["items"].([]any)) != 2 {
		t.Fatal(h)
	}
	// Read on the PC: the phone is told
	b1.send(map[string]any{"t": "dm-read", "with": ana.uid, "id": last["id"]})
	if r := b2.wait(isT("dm-read")); r["with"] != ana.uid || r["id"] != last["id"] {
		t.Fatal(r)
	}
	// An older marker never goes back
	b2.send(map[string]any{"t": "dm-read", "with": ana.uid, "id": 1})
	time.Sleep(100 * time.Millisecond)
	b3, _ := dialVWith(t, hs, kb, "Bob", map[string]any{"dev": "dev-b3"})
	b3.send(map[string]any{"t": "dms"})
	d = b3.wait(isT("dms"))["items"].([]any)[0].(map[string]any)
	if d["unread"].(float64) != 0 || d["read"] != last["id"] {
		t.Fatalf("after reading: %v", d)
	}
	// Ana's own messages are never unread for her
	ana.send(map[string]any{"t": "dms"})
	if d := ana.wait(isT("dms"))["items"].([]any)[0].(map[string]any); d["unread"].(float64) != 0 {
		t.Fatal(d)
	}
	b1.send(map[string]any{"t": "dm-read", "with": b1.uid, "id": 5})
	if e := refusal(b1, "dm-read"); e["why"] != "bad" {
		t.Fatal(e)
	}
}

// Blocking: no private messages, pokes either way; told to one's own sessions; undone by unblock.
func TestBlock(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ka := newKey(t)
	ana, _ := dialVWith(t, hs, ka, "Ana", map[string]any{"dev": "dev-a"})
	ana2, _ := dialVWith(t, hs, ka, "Ana", map[string]any{"dev": "dev-a2"})
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")

	ana.send(map[string]any{"t": "block", "uid": bob.uid})
	for _, c := range []*tclient{ana, ana2} {
		if b := c.wait(isT("blocks")); len(b["items"].([]any)) != 1 || b["items"].([]any)[0] != bob.uid {
			t.Fatal(b)
		}
	}
	bob.send(map[string]any{"t": "say", "to": "u:" + ana.uid, "text": "hi"})
	if e := refusal(bob, "say"); e["why"] != "blocked" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "e2e-say", "to": "u:" + ana.uid, "e": e2eEnv(t, bob.uid, ana.uid, "x")})
	if e := refusal(bob, "e2e-say"); e["why"] != "blocked" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "poke", "uid": ana.uid})
	if e := refusal(bob, "poke"); e["why"] != "blocked" {
		t.Fatal(e)
	}
	// Both ways
	ana.send(map[string]any{"t": "say", "to": "u:" + bob.uid, "text": "hi"})
	if e := refusal(ana, "say"); e["why"] != "blocked" {
		t.Fatal(e)
	}
	// Calls too (calls.go), both ways
	if !s.callBlocked(ana.uid, bob.uid) || !s.callBlocked(bob.uid, ana.uid) {
		t.Fatal("a call between them isn't blocked")
	}
	ana2.send(map[string]any{"t": "blocks"})
	ana2.wait(isT("blocks"))
	ana.send(map[string]any{"t": "unblock", "uid": bob.uid})
	if b := ana.wait(isT("blocks")); len(b["items"].([]any)) != 0 || s.callBlocked(ana.uid, bob.uid) {
		t.Fatal(b)
	}
	bob.send(map[string]any{"t": "say", "to": "u:" + ana.uid, "text": "hi again"})
	if m := ana.wait(isT("msg"))["m"].(map[string]any); m["text"] != "hi again" {
		t.Fatal(m)
	}
	ana.send(map[string]any{"t": "block", "uid": ana.uid})
	if e := refusal(ana, "block"); e["why"] != "bad" {
		t.Fatal(e)
	}
}

func openSealed(t *testing.T, key []byte, d map[string]any) map[string]string {
	t.Helper()
	if d["sv"] != "1" || len(d) != 2 {
		t.Fatalf("not sealed, or something open next to it: %v", d)
	}
	raw, _ := base64.StdEncoding.DecodeString(d["x"].(string))
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	plain, err := g.Open(nil, raw[:12], raw[12:], []byte(pushSealAAD))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	_ = json.Unmarshal(plain, &out)
	return out
}

// A private message to someone offline wakes their phone: sealed for an app with a push key (who, where, the
// envelope), the contentless open form for an older app; through the server's own Firebase credentials.
func TestPushSealedForOfflineDM(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_, _ = io.WriteString(w, `{"access_token":"at","expires_in":3600}`)
		case strings.HasSuffix(r.URL.Path, "/messages:send"):
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			got = append(got, m["message"].(map[string]any))
			mu.Unlock()
			_, _ = io.WriteString(w, `{"name":"x"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	cred, _ := json.Marshal(map[string]string{"project_id": "proj", "client_email": "sa@proj", "token_uri": fake.URL + "/token",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	credFile := filepath.Join(t.TempDir(), "sa.json")
	_ = os.WriteFile(credFile, cred, 0o600)
	s, hs := newTestServer(t, func(c *config) { c.pushCreds = credFile; c.inviteBase = "https://voice.example.org/i/" })
	s.push.api = fake.URL

	pkey := make([]byte, 32)
	_, _ = rand.Read(pkey)
	kb := newKey(t)
	bob, _ := dialVWith(t, hs, kb, "Bob", map[string]any{"dev": "dev-b"})
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob.send(map[string]any{"t": "push-token", "token": "tok-new-0123456789abcdef", "platform": "android", "key": base64.StdEncoding.EncodeToString(pkey)})
	bob.send(map[string]any{"t": "push-token", "token": "tok-old-0123456789abcdef", "platform": "android"})
	time.Sleep(200 * time.Millisecond)
	gone(t, bob, ana)

	env := e2eEnv(t, ana.uid, bob.uid, "secret words")
	ana.send(map[string]any{"t": "e2e-say", "to": "u:" + bob.uid, "e": env})
	id := ana.wait(isT("msg"))["m"].(map[string]any)["id"].(float64)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("pushes %v", got)
	}
	for _, g := range got {
		d := g["data"].(map[string]any)
		raw, _ := json.Marshal(g)
		if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "Ana") {
			t.Fatalf("words or the sender's name through Google: %s", raw)
		}
		if g["android"].(map[string]any)["priority"] != "HIGH" {
			t.Fatal(g)
		}
		switch g["token"] {
		case "tok-new-0123456789abcdef":
			in := openSealed(t, pkey, d)
			if in["kind"] != "dm" || in["title"] != "Ana" || in["from"] != ana.uid || in["conv"] != "u:"+ana.uid || in["e"] != env ||
				in["id"] != fmt.Sprint(int64(id)) || in["srv"] != "voice.example.org" {
				t.Fatalf("sealed %v", in)
			}
		case "tok-old-0123456789abcdef":
			if d["kind"] != "dm" || d["x"] != nil || d["from"] != nil {
				t.Fatalf("open %v", d)
			}
		default:
			t.Fatal(g)
		}
	}
}

// Without Firebase credentials (a self-hosted server), a private message's push goes through the donation
// service's relay (pushrelay.go) sealed for the phone: the relay sees the token and a blob.
func TestPushSealedThroughRelay(t *testing.T) {
	got := make(chan map[string]any, 8)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env struct{ P, Sig string }
		_ = json.NewDecoder(r.Body).Decode(&env)
		pj, _ := base64.RawURLEncoding.DecodeString(env.P)
		var q map[string]any
		_ = json.Unmarshal(pj, &q)
		if q["kind"] == "push" { // the Plus build checks in too
			got <- q
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer fake.Close()
	s, hs := newTestServer(t, func(c *config) { c.licenseURL = fake.URL; c.pushRelay = true })
	if s.push == nil || s.push.relay == nil {
		t.Fatal("no relay pusher")
	}
	pkey := make([]byte, 32)
	_, _ = rand.Read(pkey)
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob.send(map[string]any{"t": "push-token", "token": "tok-live-0123456789abcdef", "platform": "android", "key": base64.StdEncoding.EncodeToString(pkey)})
	time.Sleep(200 * time.Millisecond)
	gone(t, bob, ana)
	ana.send(map[string]any{"t": "say", "to": "u:" + bob.uid, "text": "plain from an old app"})
	select {
	case q := <-got:
		raw, _ := json.Marshal(q)
		if q["kind"] != "push" || q["token"] != "tok-live-0123456789abcdef" || strings.Contains(string(raw), "Ana") || strings.Contains(string(raw), "plain") {
			t.Fatalf("relay request %s", raw)
		}
		if in := openSealed(t, pkey, q["data"].(map[string]any)); in["kind"] != "dm" || in["title"] != "Ana" || in["from"] != ana.uid || in["body"] != "" || in["e"] != "" {
			t.Fatalf("sealed %v", in)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing relayed")
	}
}

// TestWebMembersE2E: the browser's member list and a private message to someone offline (web/e2e/members.js):
// SV_WEB_E2E=1 go test -run TestWebMembersE2E -v . (SV_SHOTS=<dir> for the screenshots)
func TestWebMembersE2E(t *testing.T) {
	if os.Getenv("SV_WEB_E2E") != "1" {
		t.Skip("set SV_WEB_E2E=1 (needs node, playwright and its Chromium)")
	}
	s, hs := newTestServer(t, nil)
	secret := "see you at nine " + itoa(int(time.Now().UnixNano()%1000000))
	args := []string{"web/e2e/members.js", hs.URL, secret}
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
		t.Log("members e2e:", sc.Text())
		switch sc.Text() {
		case "SENT":
			sent = true
			var n, enc int
			_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE text LIKE ? OR enc LIKE ?`, "%at nine%", "%at nine%").Scan(&n)
			_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv LIKE 'd:%' AND enc LIKE '{"v":1,%'`).Scan(&enc)
			if n != 0 || enc != 1 {
				t.Errorf("the words are on the server (%d), or the envelope isn't (%d)", n, enc)
			}
		case "OK":
			ok = true
		}
	}
	if err := cmd.Wait(); err != nil || !sent || !ok {
		t.Fatalf("members e2e: %v (sent %v, ok %v)", err, sent, ok)
	}
	var blocks int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM blocks`).Scan(&blocks)
	if blocks != 1 {
		t.Fatalf("blocks %d", blocks)
	}
}

// A block covers whispering too: the blocked person's voice doesn't go straight into the other's ears (nor the
// other way round).
func TestBlockStopsWhisper(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	carl, _ := dialV(t, hs, newKey(t), "Carl", "")
	ana.send(map[string]any{"t": "block", "uid": bob.uid})
	ana.wait(isT("blocks"))
	targets := func(c *tclient) []int {
		time.Sleep(300 * time.Millisecond)
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		return append([]int(nil), s.hub.sessions[c.sid].whisperTo...)
	}
	bob.send(map[string]any{"t": "whisper", "on": true, "sids": []int{ana.sid, carl.sid}})
	if got := targets(bob); len(got) != 1 || got[0] != carl.sid {
		t.Fatalf("bob whispers to %v", got)
	}
	ana.send(map[string]any{"t": "whisper", "on": true, "sids": []int{bob.sid, carl.sid}})
	if got := targets(ana); len(got) != 1 || got[0] != carl.sid {
		t.Fatalf("ana whispers to %v", got)
	}
}
