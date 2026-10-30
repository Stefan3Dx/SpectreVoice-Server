package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// callPair: two people signed in on one server.
func callPair(t *testing.T, mod func(*config)) (*server, string, *tclient, *tclient) {
	s, hs := newTestServer(t, mod)
	a, _ := dialV(t, hs, newKey(t), "Alice", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	return s, hs.URL, a, b
}

func isCall(t, id string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["t"] == t && (id == "" || m["id"] == id) }
}

// noMsg: nothing matching comes within d.
func noMsg(t *testing.T, c *tclient, d time.Duration, match func(map[string]any) bool) {
	t.Helper()
	for _, m := range c.seen {
		if match(m) {
			t.Fatalf("unexpected %v", m)
		}
	}
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-c.in:
			if !ok {
				return
			}
			if match(m) {
				t.Fatalf("unexpected %v", m)
			}
			c.seen = append(c.seen, m)
		case <-deadline:
			return
		}
	}
}

func callLineOf(t *testing.T, c *tclient) map[string]any {
	t.Helper()
	m := c.wait(func(m map[string]any) bool {
		if m["t"] != "msg" {
			return false
		}
		mm, _ := m["m"].(map[string]any)
		return mm["sys"] != nil && mm["sys"] != ""
	})["m"].(map[string]any)
	var sys map[string]any
	if err := json.Unmarshal([]byte(m["sys"].(string)), &sys); err != nil {
		t.Fatal(err)
	}
	m["sysv"] = sys
	return m
}

func ring(t *testing.T, a, b *tclient) string {
	t.Helper()
	a.send(map[string]any{"t": "call-invite", "to": b.uid, "n": "r1"})
	r := a.wait(isCall("call-ringing", ""))
	if r["n"] != "r1" || r["to"] != b.uid || r["name"] == "" {
		t.Fatalf("ringing: %v", r)
	}
	id := r["id"].(string)
	got := b.wait(isCall("call-ring", id))
	if got["from"] != a.uid || got["name"] == "" {
		t.Fatalf("ring: %v", got)
	}
	return id
}

func TestCallRingAnswerSignalHangUp(t *testing.T) {
	_, _, a, b := callPair(t, func(c *config) { c.callsRoute = "p2p" })
	id := ring(t, a, b)
	b.send(map[string]any{"t": "call-accept", "id": id})
	for _, c := range []*tclient{a, b} {
		acc := c.wait(isCall("call-accepted", id))
		if acc["route"] != "p2p" {
			t.Fatalf("route: %v", acc)
		}
	}
	// The signalling goes to the other side only, the fingerprint's signature with it
	a.send(map[string]any{"t": "call-sdp", "id": id, "type": "offer", "sdp": "v=0 offer", "ik": "IK", "fsig": "SIG"})
	off := b.wait(isCall("call-sdp", id))
	if off["sdp"] != "v=0 offer" || off["ik"] != "IK" || off["fsig"] != "SIG" || off["type"] != "offer" {
		t.Fatalf("sdp: %v", off)
	}
	b.send(map[string]any{"t": "call-ice", "id": id, "candidate": "candidate:1 1 udp 1 1.2.3.4 5 typ host", "mid": "0", "mline": 0})
	if c := a.wait(isCall("call-ice", id)); !strings.HasPrefix(c["candidate"].(string), "candidate:") {
		t.Fatalf("ice: %v", c)
	}
	time.Sleep(1100 * time.Millisecond)
	b.send(map[string]any{"t": "call-end", "id": id})
	end := a.wait(isCall("call-end", id))
	if end["why"] != "hangup" || end["secs"].(float64) < 1 {
		t.Fatalf("end: %v", end)
	}
	for _, c := range []*tclient{a, b} {
		l := callLineOf(t, c)
		if l["sysv"].(map[string]any)["kind"] != "done" || !strings.HasPrefix(l["text"].(string), "📞 Call · ") || l["from"] != a.uid {
			t.Fatalf("line: %v", l)
		}
	}
	// The history has it, and nobody may edit it
	b.send(map[string]any{"t": "history", "to": "u:" + a.uid})
	h := b.wait(isT("history"))
	items := h["items"].([]any)
	last := items[len(items)-1].(map[string]any)
	if last["sys"] == nil {
		t.Fatalf("history: %v", last)
	}
	a.send(map[string]any{"t": "edit", "id": last["id"], "text": "x"})
	if e := a.wait(isT("error")); e["op"] != "edit" {
		t.Fatalf("edit: %v", e)
	}
}

func TestCallDeclineBusyAndTimeout(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.callsRoute = "p2p"; c.callRing = 1500 * time.Millisecond })
	a, _ := dialV(t, hs, newKey(t), "Alice", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	c, _ := dialV(t, hs, newKey(t), "Cara", "")

	// Declined
	id := ring(t, a, b)
	b.send(map[string]any{"t": "call-decline", "id": id})
	if e := a.wait(isCall("call-end", id)); e["why"] != "declined" {
		t.Fatalf("decline: %v", e)
	}
	if l := callLineOf(t, a); l["sysv"].(map[string]any)["kind"] != "declined" {
		t.Fatalf("line: %v", l)
	}

	// Busy: Bob in a call with Alice; Cara calling him hears busy at once, Alice calling again is refused
	id = ring(t, a, b)
	b.send(map[string]any{"t": "call-accept", "id": id})
	a.wait(isCall("call-accepted", id))
	c.send(map[string]any{"t": "call-invite", "to": b.uid, "n": "x"})
	if e := c.wait(isCall("call-end", "")); e["why"] != "busy" || e["n"] != "x" {
		t.Fatalf("busy: %v", e)
	}
	a.send(map[string]any{"t": "call-invite", "to": c.uid})
	if e := a.wait(isT("error")); e["op"] != "call-invite" || e["why"] != "busy" {
		t.Fatalf("caller busy: %v", e)
	}
	// Cara can't speak into their call
	c.send(map[string]any{"t": "call-sdp", "id": id, "type": "offer", "sdp": "v=0 evil"})
	noMsg(t, b, 300*time.Millisecond, isCall("call-sdp", id))
	a.send(map[string]any{"t": "call-end", "id": id})
	b.wait(isCall("call-end", id))

	// Unanswered: both told, a missed call in the chat
	id = ring(t, c, b)
	if e := c.wait(isCall("call-end", id)); e["why"] != "timeout" {
		t.Fatalf("timeout: %v", e)
	}
	if e := b.wait(isCall("call-end", id)); e["why"] != "timeout" {
		t.Fatalf("timeout callee: %v", e)
	}
	l := callLineOf(t, b)
	for l["sysv"].(map[string]any)["call"] != id {
		l = callLineOf(t, b)
	}
	if l["sysv"].(map[string]any)["kind"] != "missed" || l["text"] != "📞 Missed call" {
		t.Fatalf("missed: %v", l)
	}
	if n := len(s.calls.calls) + len(s.calls.byUID); n != 0 {
		t.Fatalf("left behind: %d", n)
	}
}

func TestCallBlockedRingsNobody(t *testing.T) {
	_, _, a, b := callPair(t, func(c *config) { c.callsRoute = "p2p"; c.callRing = time.Second })
	before := callBlockedHook // the blocks' own (people.go), back after this test
	callBlockedHook = func(_ *server, x, y string) bool { return x == b.uid && y == a.uid }
	t.Cleanup(func() { callBlockedHook = before })
	a.send(map[string]any{"t": "call-invite", "to": b.uid})
	id := a.wait(isCall("call-ringing", ""))["id"].(string)
	if e := a.wait(isCall("call-end", id)); e["why"] != "timeout" {
		t.Fatalf("end: %v", e)
	}
	noMsg(t, b, 300*time.Millisecond, func(m map[string]any) bool {
		return strings.HasPrefix(m["t"].(string), "call-") || m["t"] == "msg"
	})
	noMsg(t, a, 200*time.Millisecond, isT("msg"))
}

func TestCallRules(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.callsRoute = "p2p" })
	a, _ := dialV(t, hs, newKey(t), "Alice", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	// Someone who was never on this server
	a.send(map[string]any{"t": "call-invite", "to": "AAAAAAAAAAAAAAAAAAAA"})
	if e := a.wait(isT("error")); e["why"] != "bad" {
		t.Fatalf("stranger: %v", e)
	}
	// Oneself
	a.send(map[string]any{"t": "call-invite", "to": a.uid})
	if e := a.wait(isT("error")); e["why"] != "bad" {
		t.Fatalf("self: %v", e)
	}
	// Offline, no phone: told at once, a missed call in the chat
	bob := b.uid
	_ = b.ws.Close()
	time.Sleep(200 * time.Millisecond)
	a.send(map[string]any{"t": "call-invite", "to": bob})
	if e := a.wait(isCall("call-end", "")); e["why"] != "offline" {
		t.Fatalf("offline: %v", e)
	}
	callLineOf(t, a)
	// Rate: four at once, then one every 10 s
	refused := false
	for i := 0; i < 6 && !refused; i++ {
		a.send(map[string]any{"t": "call-invite", "to": bob})
		m := a.wait(func(m map[string]any) bool { return m["t"] == "call-end" || m["t"] == "error" })
		refused = m["t"] == "error" && m["why"] == "rate"
	}
	if !refused {
		t.Fatal("no rate limit")
	}
	// Off on this server
	s.cfg.callsRoute = "off"
	a.send(map[string]any{"t": "call-invite", "to": bob})
	if e := a.wait(isT("error")); e["why"] != "off" {
		t.Fatalf("off: %v", e)
	}
}

func TestCallTurnCredentialsAndRoute(t *testing.T) {
	_, _, a, b := callPair(t, func(c *config) { c.turn = "turn:turn.example.com:3478"; c.turnSecret = "s3cret" })
	id := ring(t, a, b)
	b.send(map[string]any{"t": "call-accept", "id": id})
	acc := a.wait(isCall("call-accepted", id))
	if acc["route"] != "p2p" {
		t.Fatalf("route with TURN: %v", acc["route"])
	}
	ice := acc["ice"].([]any)[0].(map[string]any)
	user := ice["username"].(string)
	if !strings.HasSuffix(user, ":"+a.uid) {
		t.Fatalf("user %q", user)
	}
	m := hmac.New(sha1.New, []byte("s3cret"))
	m.Write([]byte(user))
	if ice["credential"] != base64.StdEncoding.EncodeToString(m.Sum(nil)) {
		t.Fatal("credential isn't the TURN REST one")
	}
	// Bob's own, for him
	if u := b.wait(isCall("call-accepted", id))["ice"].([]any)[0].(map[string]any)["username"].(string); !strings.HasSuffix(u, ":"+b.uid) {
		t.Fatalf("bob's user %q", u)
	}
}

func TestCallSfuRoomsOnlyForTheTwo(t *testing.T) {
	s, hs := newTestServer(t, nil) // no TURN: through the server
	a, _ := dialV(t, hs, newKey(t), "Alice", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	c, _ := dialV(t, hs, newKey(t), "Cara", "")
	id := ring(t, a, b)
	if s.callRoomAllowed(a.uid, callRoomOf(a.uid, id), true) {
		t.Fatal("a room before the answer")
	}
	b.send(map[string]any{"t": "call-accept", "id": id})
	if r := a.wait(isCall("call-accepted", id))["route"]; r != "sfu" {
		t.Fatalf("route: %v", r)
	}
	ra, rb := callRoomOf(a.uid, id), callRoomOf(b.uid, id)
	switch {
	case !s.callRoomAllowed(a.uid, ra, true), !s.callRoomAllowed(b.uid, rb, true):
		t.Fatal("may publish own")
	case s.callRoomAllowed(a.uid, rb, true):
		t.Fatal("published someone else's")
	case !s.hub.mayWatch(b.uid, ra), !s.hub.mayWatch(a.uid, rb):
		t.Fatal("may take the other's")
	case s.hub.mayWatch(c.uid, ra), s.hub.mayWatch(a.uid, ra):
		t.Fatal("a third (or oneself) takes it")
	}
	a.send(map[string]any{"t": "call-end", "id": id})
	b.wait(isCall("call-end", id))
	if s.hub.mayWatch(b.uid, ra) {
		t.Fatal("after the end")
	}
}

func TestCallRejoinAfterReconnect(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.callsRoute = "p2p" })
	ka := newKey(t)
	a, _ := dialV(t, hs, ka, "Alice", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	id := ring(t, a, b)
	b.send(map[string]any{"t": "call-accept", "id": id})
	a.wait(isCall("call-accepted", id))
	_ = a.ws.Close()
	time.Sleep(200 * time.Millisecond)
	a2, _ := dialV(t, hs, ka, "Alice", "")
	a2.send(map[string]any{"t": "call-rejoin", "id": id})
	if r := a2.wait(isCall("call-rejoined", id)); r["active"] != true {
		t.Fatalf("rejoin: %v", r)
	}
	b.send(map[string]any{"t": "call-sdp", "id": id, "type": "offer", "sdp": "v=0 restart"})
	if m := a2.wait(isCall("call-sdp", id)); m["sdp"] != "v=0 restart" {
		t.Fatalf("after rejoin: %v", m)
	}
	s.calls.mu.Lock()
	n := len(s.calls.calls)
	s.calls.mu.Unlock()
	if n != 1 {
		t.Fatal("the call ended on the drop")
	}
}

// openPush is what the phone does with a sealed call push.
func openPush(t *testing.T, key []byte, e string) map[string]any {
	t.Helper()
	raw, _ := base64.StdEncoding.DecodeString(e)
	blk, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(blk)
	plain, err := g.Open(nil, raw[:12], raw[12:], []byte("sv-push-v1"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(plain, &m)
	return m
}

func nextPush(t *testing.T, p *pusher) pushJob {
	t.Helper()
	select {
	case j := <-p.queue:
		return j
	case <-time.After(5 * time.Second):
		t.Fatal("no push")
	}
	return pushJob{}
}

func TestCallPushSealedAndDeclineFromNotification(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.callsRoute = "p2p"; c.inviteBase = "https://voice.example.com/i/" })
	if err := s.store.migratePush(); err != nil {
		t.Fatal(err)
	}
	p := &pusher{queue: make(chan pushJob, 16), st: s.store}
	s.push = p
	a, _ := dialV(t, hs, newKey(t), "Alice", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	tok := strings.Repeat("t", 40)
	b.send(map[string]any{"t": "push-token", "token": tok, "platform": "android", "key": base64.StdEncoding.EncodeToString(key), "addr": "voice.example.com"})
	// An older app's token (no key) never gets a call
	b.send(map[string]any{"t": "push-token", "token": strings.Repeat("o", 40), "platform": "android"})
	time.Sleep(300 * time.Millisecond)
	bob := b.uid
	_ = b.ws.Close()
	time.Sleep(200 * time.Millisecond)

	a.send(map[string]any{"t": "call-invite", "to": bob})
	id := a.wait(isCall("call-ringing", ""))["id"].(string)
	j := nextPush(t, p)
	if j.only != tok || j.uid != bob || j.data["kind"] != "call" || j.ttl != "45s" || len(j.data) != 2 {
		t.Fatalf("push: %+v", j)
	}
	body := openPush(t, key, j.data["e"])
	if body["call"] != id || body["from"] != a.uid || body["name"] != "Alice" || body["addr"] != "voice.example.com" || body["srv"] != "voice.example.com" || body["dt"] == "" {
		t.Fatalf("sealed: %v", body)
	}
	// Google sees nothing of who
	for _, v := range j.data {
		if strings.Contains(v, "Alice") || strings.Contains(v, a.uid) {
			t.Fatal("the caller in the clear")
		}
	}
	// A wrong token can't decline; the right one does, without signing in
	post := func(tk string) int {
		b, _ := json.Marshal(map[string]string{"call": id, "t": tk})
		res, err := http.Post(hs.URL+"/v1/call/decline", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if post(strings.Repeat("0", 32)) != 404 {
		t.Fatal("declined with a made-up token")
	}
	if post(body["dt"].(string)) != 200 {
		t.Fatal("decline refused")
	}
	if e := a.wait(isCall("call-end", id)); e["why"] != "declined" {
		t.Fatalf("end: %v", e)
	}
	end := nextPush(t, p)
	if end.data["kind"] != "call-end" || openPush(t, key, end.data["e"])["why"] != "declined" {
		t.Fatalf("end push: %+v", end)
	}

	// Cancelled by the caller: the phone stops ringing and shows a missed call
	a.send(map[string]any{"t": "call-invite", "to": bob})
	id = a.wait(isCall("call-ringing", ""))["id"].(string)
	nextPush(t, p)
	a.send(map[string]any{"t": "call-cancel", "id": id})
	end = nextPush(t, p)
	if m := openPush(t, key, end.data["e"]); m["why"] != "missed" || m["call"] != id {
		t.Fatalf("cancel push: %v", m)
	}
}

// A self-hosted server without credentials: its pushes go through the donation service, signed.
func TestPushRelayThroughDonationService(t *testing.T) {
	got := make(chan map[string]any, 4)
	fake := http.NewServeMux()
	fake.HandleFunc("/v1/push", func(w http.ResponseWriter, r *http.Request) {
		var env struct{ P, Sig string }
		_ = json.NewDecoder(r.Body).Decode(&env)
		pj, _ := base64.RawURLEncoding.DecodeString(env.P)
		var q map[string]any
		_ = json.Unmarshal(pj, &q)
		got <- q
		if q["token"] == strings.Repeat("g", 40) {
			w.WriteHeader(410)
			_, _ = w.Write([]byte(`{"error":"token no longer valid"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	ls := newHTTPTest(t, fake)
	s, _ := newTestServer(t, func(c *config) { c.licenseURL = ls; c.pushRelay = true })
	if s.push == nil || s.push.relay == nil {
		t.Fatal("no relay")
	}
	_, _ = s.store.db.Exec(`INSERT INTO push_tokens(token,uid,platform,updated) VALUES(?,?,?,?),(?,?,?,?)`,
		strings.Repeat("k", 40), "U1", "android", 1, strings.Repeat("g", 40), "U1", "android", 2)
	s.push.notify("U1", map[string]string{"kind": "dm"})
	for i := 0; i < 2; i++ {
		select {
		case q := <-got:
			if q["kind"] != "push" || q["sig"] != nil || q["server"] != s.lic.fp || q["data"].(map[string]any)["kind"] != "dm" || q["ttl"] != "86400s" {
				t.Fatalf("relay request: %v", q)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("nothing relayed")
		}
	}
	time.Sleep(200 * time.Millisecond)
	if toks := s.store.pushTokens("U1"); len(toks) != 1 {
		t.Fatalf("a gone token stayed: %v", toks)
	}
}

func newHTTPTest(t *testing.T, h http.Handler) string {
	hs := httptest.NewServer(h)
	t.Cleanup(hs.Close)
	return hs.URL
}

// /v1/call/decline needs no sign-in, so it's held to a budget per address: guessing calls and tokens from one place
// stops after a few tries (429), even for a right token then.
func TestCallDeclineRateLimited(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.callsRoute = "p2p" })
	a, _ := dialV(t, hs, newKey(t), "Alice", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	id := ring(t, a, b)
	post := func(tk string) int {
		raw, _ := json.Marshal(map[string]string{"call": id, "t": tk})
		res, err := http.Post(hs.URL+"/v1/call/decline", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	limited := false
	for i := 0; i < 40 && !limited; i++ {
		limited = post(strings.Repeat("0", 32)) == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("40 wrong tokens from one address, never slowed down")
	}
	if code := post(s.declineToken(id, b.uid)); code != http.StatusTooManyRequests {
		t.Fatalf("over the budget, still answered %d", code)
	}
}


// The welcome says whether this server has calls (and how), so the apps show the call button only where it works.
func TestCallsInWelcome(t *testing.T) {
	for route, want := range map[string]any{"sfu": "sfu", "p2p": "p2p", "off": nil} {
		_, hs := newTestServer(t, func(c *config) { c.callsRoute = route })
		c, _ := dialV(t, hs, newKey(t), "Ana", "")
		if got := c.wel["server"].(map[string]any)["calls"]; got != want {
			t.Fatalf("route %s: calls %v", route, got)
		}
	}
}

// A callee only on clients without calls (the web client, an app before 0.9.0) isn't rung: the caller hears
// "offline" at once, not a ring for the whole ring time; a 0.9.0 app of theirs rings.
func TestCallNotRungOnClientsWithoutCalls(t *testing.T) {
	_, hs := newTestServer(t, func(c *config) { c.callsRoute = "p2p" })
	a, _ := dialV(t, hs, newKey(t), "Alice", "")
	kb := newKey(t)
	old, _ := dialVWith(t, hs, kb, "Bob", map[string]any{"client": "windows/0.8.0"})
	web, _ := dialVWith(t, hs, kb, "Bob", map[string]any{"client": "web/0.8.0"})
	a.send(map[string]any{"t": "call-invite", "to": old.uid, "n": "o"})
	if e := a.wait(isCall("call-end", "")); e["why"] != "offline" || e["n"] != "o" {
		t.Fatalf("not offline: %v", e)
	}
	noMsg(t, old, 300*time.Millisecond, isCall("call-ring", ""))
	noMsg(t, web, 0, isCall("call-ring", ""))
	// Bob's new app comes: it rings there only
	nb, _ := dialVWith(t, hs, kb, "Bob", map[string]any{"client": "android/0.9.0"})
	id := ring(t, a, nb)
	noMsg(t, old, 300*time.Millisecond, isCall("call-ring", id))
	a.send(map[string]any{"t": "call-cancel", "id": id})
}

func TestClientCanCall(t *testing.T) {
	for c, want := range map[string]bool{
		"windows/0.9.0": true, "android/0.9.0": true, "linux/0.10.2": true, "macos/1.0.0": true, "Windows/0.9.1-beta": true,
		"windows/0.8.0": false, "android/0.8.3": false, "linux/0.7.2": false, "web/0.9.0": false, "web/dev": false,
		"test/1": true, "jvm": true, "linux/dev": true, "": true, "android/x.y": true, "android/0.1.0": true, "windows/0.2.0": false,
	} {
		if got := clientCanCall(c); got != want {
			t.Errorf("%q: %v, want %v", c, got, want)
		}
	}
}
