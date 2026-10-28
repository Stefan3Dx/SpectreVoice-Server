package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newUID gives the uid of a fresh device key, as the apps have.
func newUID(t *testing.T) string {
	spki, err := x509.MarshalPKIXPublicKey(&newKey(t).PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return uidOf(spki)
}

// A server for the side connections (bus, files, backups…), where any community name may be opened, and a way in.
type busTest struct {
	t   *testing.T
	s   *server
	srv *httptest.Server
	url string // ws://…/bus
}

// newBusTest builds the server (mod changes its config, prep the server before it answers).
func newBusTest(t *testing.T, mod func(*config), prep func(*server)) *busTest {
	t.Helper()
	cfg := config{data: t.TempDir(), name: "Test", maxUsers: 50, udpMin: 40000, udpMax: 40400, maxRooms: 4, maxSubs: 10, maxSpaces: 4}
	if mod != nil {
		mod(&cfg)
	}
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.testSpaces = func(name string) bool { return name != "" && len(name) <= 64 }
	if prep != nil {
		prep(s)
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(s.store.close)
	t.Cleanup(s.bus.conns.Wait) // runs before the temp dir goes: the last one out saves there
	t.Cleanup(srv.Close)
	return &busTest{t: t, s: s, srv: srv, url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/bus"}
}

// token is uid's token; admin makes uid an admin first.
func (bt *busTest) token(uid string, admin bool) string {
	_ = bt.s.store.join(1, uid) // a member of the first community (its space is "main")
	if admin {
		if err := bt.s.store.setRole(uid, roleAdmin); err != nil {
			bt.t.Fatal(err)
		}
	}
	return bt.s.makeToken(uid)
}

type busConn struct {
	t     *testing.T
	ws    *websocket.Conn
	state map[string]map[string]map[string]any
	ops   []any
	admin bool
}

func (bt *busTest) connect(space, uid string, admin bool) *busConn {
	bt.t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(bt.url+"?token="+url.QueryEscape(bt.token(uid, admin))+"&space="+url.QueryEscape(space), nil)
	if err != nil {
		bt.t.Fatal(err)
	}
	bt.t.Cleanup(func() { ws.Close() })
	c := &busConn{t: bt.t, ws: ws}
	st := c.read()
	if st["t"] != "state" {
		bt.t.Fatalf("hello: %v", st)
	}
	raw, _ := json.Marshal(st["state"])
	_ = json.Unmarshal(raw, &c.state)
	c.ops, _ = st["ops"].([]any)
	c.admin, _ = st["admin"].(bool)
	if c.admin != admin {
		bt.t.Fatalf("admin %v, wanted %v", st["admin"], admin)
	}
	return c
}

func (bt *busTest) dial(space, uid string) *busConn { return bt.connect(space, uid, false) }

// dialAdmin connects as an admin.
func (bt *busTest) dialAdmin(space, uid string) *busConn { return bt.connect(space, uid, true) }

func (c *busConn) write(m map[string]any) {
	if err := c.ws.WriteJSON(m); err != nil {
		c.t.Fatal(err)
	}
}

func (c *busConn) read() map[string]any {
	c.t.Helper()
	var m map[string]any
	_ = c.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := c.ws.ReadJSON(&m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

// expect reads the next message, which must be of type t.
func (c *busConn) expect(t string) map[string]any {
	c.t.Helper()
	m := c.read()
	if m["t"] != t {
		c.t.Fatalf("wanted %q, got %v", t, m)
	}
	return m
}

func hasOp(ops []any, op string) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

func TestVerifyIdentity(t *testing.T) {
	k := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	pub := base64.StdEncoding.EncodeToString(spki)
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	sign := func(k *ecdsa.PrivateKey, nonce []byte) []byte {
		h := sha256.Sum256(append([]byte(authPrefix), nonce...))
		sig, err := ecdsa.SignASN1(rand.Reader, k, h[:])
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	sig := sign(k, nonce)
	uid, err := verifyIdentity(pub, nonce, sig)
	if err != nil || uid != uidOf(spki) || len(uid) != 20 || !safeID.MatchString(uid) {
		t.Fatalf("good signature: %q %v", uid, err)
	}
	if _, err := verifyIdentity(pub, make([]byte, 32), sig); err == nil {
		t.Fatal("signature over another nonce accepted")
	}
	// Signed without the prefix: not a signature for this server
	raw := sha256.Sum256(nonce)
	noPrefix, _ := ecdsa.SignASN1(rand.Reader, k, raw[:])
	if _, err := verifyIdentity(pub, nonce, noPrefix); err == nil {
		t.Fatal("signature without the prefix accepted")
	}
	other, _ := x509.MarshalPKIXPublicKey(&newKey(t).PublicKey)
	if _, err := verifyIdentity(base64.StdEncoding.EncodeToString(other), nonce, sig); err == nil {
		t.Fatal("signature by another key accepted")
	}
	for _, junk := range []string{"", "not base64!", "bm9wZQ=="} {
		if _, err := verifyIdentity(junk, nonce, sig); err == nil {
			t.Fatalf("junk key %q accepted", junk)
		}
	}
	if _, err := verifyIdentity(pub, nonce, nil); err == nil {
		t.Fatal("no signature accepted")
	}
	// Only P-256
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	spki384, _ := x509.MarshalPKIXPublicKey(&k384.PublicKey)
	h := sha256.Sum256(append([]byte(authPrefix), nonce...))
	sig384, _ := ecdsa.SignASN1(rand.Reader, k384, h[:])
	if _, err := verifyIdentity(base64.StdEncoding.EncodeToString(spki384), nonce, sig384); err == nil {
		t.Fatal("P-384 key accepted")
	}
}

func TestBusToken(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	get := func(path string, hdr string) int {
		req, _ := http.NewRequest("GET", bt.srv.URL+path, nil)
		if hdr != "" {
			req.Header.Set("Authorization", "Bearer "+hdr)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	// No token, a forged one, an expired one: the bus isn't opened
	tok := bt.token("U", false)
	parts := strings.Split(tok, ".")
	forged := "V." + parts[1] + "." + parts[2]
	expired := hmacToken(bt.s, "U", time.Now().Add(-time.Minute).Unix())
	if _, ok := bt.s.checkToken(hmacToken(bt.s, "U", time.Now().Add(time.Minute).Unix())); !ok {
		t.Fatal("hmacToken doesn't sign as makeToken")
	}
	for _, q := range []string{"?space=main", "?space=main&token=nope", "?space=main&token=" + url.QueryEscape(forged), "?space=main&token=" + url.QueryEscape(expired)} {
		if c := get("/bus"+q, ""); c != http.StatusUnauthorized {
			t.Fatalf("%s: %d", q, c)
		}
	}
	// The token in the header works the same (a plain GET, not a WebSocket: not 401 is enough)
	if c := get("/bus?space=main", tok); c == http.StatusUnauthorized {
		t.Fatal("Bearer token refused")
	}
	// /health too
	if c := get("/health", ""); c != http.StatusUnauthorized {
		t.Fatalf("health without a token: %d", c)
	}
	res, err := http.Get(bt.srv.URL + "/health?token=" + url.QueryEscape(tok))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("health: %v %v", err, res)
	}
	var h health
	_ = json.NewDecoder(res.Body).Decode(&h)
	res.Body.Close()
	if h.Version != version {
		t.Fatalf("health: %+v", h)
	}

	// Signed in from the token right away: stamped with the token's uid, stamps sent by the app dropped
	c := bt.dial("main", "U")
	for _, op := range busOps {
		if !hasOp(c.ops, op) {
			t.Fatalf("ops: %v", c.ops)
		}
	}
	other := bt.dial("main", "W")
	c.write(map[string]any{"t": "put", "kind": "profiles", "id": "U", "v": map[string]any{"hue": 1, "_by": "x", "_a": false}})
	v := c.expect("put")["v"].(map[string]any)
	if v["_by"] != "U" || v["_a"] != true || v["hue"] != 1.0 {
		t.Fatalf("stamp: %v", v)
	}
	if m := other.expect("put"); m["id"] != "U" || m["from"] != "U" {
		t.Fatalf("others: %v", m)
	}

	// In production only the main community exists
	s2, hs := newTestServer(t, nil)
	t.Cleanup(s2.bus.conns.Wait)
	base := "ws" + strings.TrimPrefix(hs.URL, "http") + "/bus?token="
	s2tok := url.QueryEscape(s2.makeToken("U"))
	// Not a member: refused
	if ws, _, err := websocket.DefaultDialer.Dial(base+s2tok+"&space="+mainSpace, nil); err == nil {
		var st map[string]any
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		if ws.ReadJSON(&st) != nil || st["why"] != "member" {
			t.Fatalf("not a member: %v", st)
		}
		ws.Close()
	}
	_ = s2.store.join(1, "U")
	if _, res, err := websocket.DefaultDialer.Dial(base+s2tok+"&space=elsewhere", nil); err == nil || res == nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown community: %v %v", err, res)
	}
	ws, _, err := websocket.DefaultDialer.Dial(base+s2tok+"&space="+mainSpace, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var st map[string]any
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := ws.ReadJSON(&st); err != nil || st["t"] != "state" {
		t.Fatalf("main: %v %v", st, err)
	}
}

// hmacToken makes a token for uid that runs out at exp (unix seconds), signed as makeToken signs.
func hmacToken(s *server, uid string, exp int64) string {
	body := uid + "." + strconv.FormatInt(exp, 10)
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

func TestBusRelayAndOwnership(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	a := bt.dial("o", "A")
	b := bt.dial("o", "B")

	// Server-only types are never passed on; the rest carry the true sender and "relay"
	for _, forged := range []string{"state", "error", "challenge", "authed", "reported", "wiped"} {
		a.write(map[string]any{"t": forged, "admin": true, "state": map[string]any{}})
	}
	a.write(map[string]any{"t": "hello", "from": "B"})
	if m := b.read(); m["t"] != "hello" || m["from"] != "A" || m["relay"] != true {
		t.Fatalf("relay: %v", m)
	}

	refused := func(c *busConn, why string) {
		t.Helper()
		if m := c.expect("error"); m["why"] != why {
			t.Fatalf("wanted refusal %q: %v", why, m)
		}
	}
	put := func(c *busConn, kind, id string, v map[string]any) {
		c.write(map[string]any{"t": "put", "kind": kind, "id": id, "v": v})
	}
	del := func(c *busConn, kind, id string) { c.write(map[string]any{"t": "del", "kind": kind, "id": id}) }
	both := func(t0 string) map[string]any {
		t.Helper()
		m := a.expect(t0)
		b.expect(t0)
		return m
	}

	// Profiles and intros: only your own
	put(b, "profiles", "A", map[string]any{"hue": 3})
	refused(b, "owner")
	b.write(map[string]any{"t": "incr", "kind": "intros", "id": "A", "set": map[string]any{"x": 1}})
	refused(b, "owner")
	del(b, "profiles", "A")
	refused(b, "owner")

	// "own" only comes from buying: a put can't add it, and keeps what's there
	bt.s.bus.mu.Lock()
	bt.s.bus.spaces["o"].set("profiles", "A", json.RawMessage(`{"own":{"c_red":1}}`))
	bt.s.bus.mu.Unlock()
	put(a, "profiles", "A", map[string]any{"hue": 3, "own": map[string]any{"b_crown": 1}})
	v := both("put")["v"].(map[string]any)
	if own := v["own"].(map[string]any); own["c_red"] == nil || own["b_crown"] != nil || v["_by"] != "A" || v["_a"] != true {
		t.Fatalf("profile put: %v", v)
	}
	a.write(map[string]any{"t": "incr", "kind": "profiles", "id": "A", "set": map[string]any{"own.b_goat": 1, "eq.BADGE": "b_goat"}, "weekly": []any{"own"}, "week": "W9"})
	v = both("put")["v"].(map[string]any)
	if own := v["own"].(map[string]any); own["b_goat"] != nil || own["c_red"] == nil || v["eq"].(map[string]any)["BADGE"] != "b_goat" {
		t.Fatalf("profile incr: %v", v)
	}

	// Sounds: the uploader's
	put(a, "sounds", "s1", map[string]any{"n": "boom"})
	both("put")
	put(b, "sounds", "s1", map[string]any{"n": "mine now"})
	refused(b, "owner")
	del(b, "sounds", "s1")
	refused(b, "owner")
	put(b, "sounds", "s2", map[string]any{"n": "b's"})
	both("put")
	del(a, "sounds", "s1")
	both("del")
	// Old ones, from before "_by": anyone may clear them
	bt.s.bus.mu.Lock()
	bt.s.bus.spaces["o"].set("sounds", "old", json.RawMessage(`{"n":"old"}`))
	bt.s.bus.mu.Unlock()
	put(b, "sounds", "old", map[string]any{"n": "taken"})
	refused(b, "owner")
	del(b, "sounds", "old")
	both("del")

	// Feed: written once; cleared by its writer, or by anyone when old
	put(a, "scores", "_feed:1", map[string]any{"at": float64(time.Now().UnixMilli())})
	both("put")
	put(b, "scores", "_feed:1", map[string]any{"at": 1})
	refused(b, "owner")
	put(a, "scores", "_feed:1", map[string]any{"at": 1})
	refused(a, "owner")
	del(b, "scores", "_feed:1")
	refused(b, "owner")
	del(a, "scores", "_feed:1")
	both("del")
	put(a, "scores", "_feed:2", map[string]any{"at": float64(time.Now().Add(-4 * 24 * time.Hour).UnixMilli())})
	both("put")
	del(b, "scores", "_feed:2")
	both("del")
	// Other scores stay open to everyone (game tables write for all players)
	put(b, "scores", "A", map[string]any{"w": 1})
	both("put")
}

func TestBusBuy(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	a := bt.dial("shop", "A")
	if !hasOp(a.ops, "buy") {
		t.Fatalf("ops: %v", a.ops)
	}
	a.write(map[string]any{"t": "buy", "item": "nope"})
	if m := a.expect("error"); m["op"] != "buy" || m["why"] != "unknown" || m["item"] != "nope" {
		t.Fatalf("unknown: %v", m)
	}
	// A new purse has 1000
	a.write(map[string]any{"t": "buy", "item": "c_gold"})
	if m := a.expect("error"); m["why"] != "lei" {
		t.Fatalf("too dear: %v", m)
	}
	a.write(map[string]any{"t": "buy", "item": "c_red"})
	if m := a.expect("error"); m["why"] != "lei" {
		t.Fatalf("too dear: %v", m)
	}
	a.write(map[string]any{"t": "add", "kind": "lei", "id": "A", "d": 5000.0, "start": 1000.0, "min": 100.0})
	if v := a.expect("put")["v"].(map[string]any); v["v"] != 6000.0 {
		t.Fatalf("purse: %v", v)
	}
	a.write(map[string]any{"t": "buy", "item": "c_red"})
	if m := a.expect("put"); m["kind"] != "lei" || m["v"].(map[string]any)["v"] != 4500.0 {
		t.Fatalf("paid: %v", m)
	}
	m := a.expect("put")
	v := m["v"].(map[string]any)
	if m["kind"] != "profiles" || v["own"].(map[string]any)["c_red"] == nil || v["eq"].(map[string]any)["COLOR"] != "c_red" {
		t.Fatalf("given: %v", m)
	}
	a.write(map[string]any{"t": "buy", "item": "c_red"})
	if m := a.expect("error"); m["why"] != "owned" {
		t.Fatalf("twice: %v", m)
	}
	// A title that ran out may be bought again
	a.write(map[string]any{"t": "buy", "item": "t_afk"})
	a.expect("put")
	a.expect("put")
	a.write(map[string]any{"t": "buy", "item": "t_afk"})
	if m := a.expect("error"); m["why"] != "owned" {
		t.Fatalf("running title: %v", m)
	}
	bt.s.bus.mu.Lock()
	sp := bt.s.bus.spaces["shop"]
	p := decodeRecord(sp.state["profiles"]["A"])
	p["own"].(map[string]any)["t_afk"] = float64(time.Now().Add(-8 * 24 * time.Hour).UnixMilli())
	raw, _ := json.Marshal(p)
	sp.set("profiles", "A", raw)
	bt.s.bus.mu.Unlock()
	a.write(map[string]any{"t": "buy", "item": "t_afk"})
	if m := a.expect("put"); m["v"].(map[string]any)["v"] != 2500.0 {
		t.Fatalf("title again: %v", m)
	}
	a.expect("put")
	// An empty purse counts as 100
	a.write(map[string]any{"t": "put", "kind": "lei", "id": "A", "v": map[string]any{"v": 0}})
	a.expect("put")
	a.write(map[string]any{"t": "buy", "item": "b_clown"})
	if m := a.expect("error"); m["why"] != "lei" {
		t.Fatalf("refill isn't enough: %v", m)
	}
}

func TestBusSpaces(t *testing.T) {
	bt := newBusTest(t, nil, func(s *server) { s.bus.maxSpaces = 1 })
	a := bt.dial("one", "A")
	a.write(map[string]any{"t": "put", "kind": "events", "id": "e1", "v": map[string]any{"x": 1}})
	a.expect("put")

	// A second space doesn't fit
	ws, _, err := websocket.DefaultDialer.Dial(bt.url+"?token="+url.QueryEscape(bt.token("B", false))+"&space=two", nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := ws.ReadJSON(&m); err != nil || m["t"] != "error" || m["why"] != "space" {
		t.Fatalf("too many spaces: %v %v", m, err)
	}
	ws.Close()

	// The last one out: saved and unloaded
	a.ws.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		bt.s.bus.mu.Lock()
		n := len(bt.s.bus.spaces)
		bt.s.bus.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("space not unloaded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c := bt.dial("one", "A"); c.state["events"]["e1"] == nil {
		t.Fatalf("lost: %v", c.state)
	}
}

func TestBusDamagedFile(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "bus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := spaceFile(dir, mainSpace)
	if err := os.WriteFile(f, []byte(`{"events":{"e1":`), 0o644); err != nil {
		t.Fatal(err)
	}
	bt := newBusTest(t, func(c *config) { c.data = data }, nil)
	c := bt.dial(mainSpace, "A")
	if len(c.state["events"]) != 0 {
		t.Fatalf("state: %v", c.state)
	}
	bad, _ := filepath.Glob(f + ".bad-*")
	if len(bad) != 1 {
		t.Fatalf("damaged file not put aside: %v", bad)
	}
	if data, _ := os.ReadFile(bad[0]); string(data) != `{"events":{"e1":` {
		t.Fatalf("aside: %q", data)
	}
}

func TestBusSlowClient(t *testing.T) {
	burst := busRateBurst
	busRateBurst = 10000 // this test floods on purpose
	t.Cleanup(func() { busRateBurst = burst })
	bt := newBusTest(t, nil, nil)
	a := bt.dial("slow", "A")
	fast := bt.dial("slow", "F")
	slow := bt.dial("slow", "S") // never reads
	_ = slow
	big := strings.Repeat("x", 100<<10)
	const n = 400
	got := make(chan int, 1)
	go func() {
		i := 0
		for ; i < n; i++ {
			var m map[string]any
			_ = fast.ws.SetReadDeadline(time.Now().Add(20 * time.Second))
			if err := fast.ws.ReadJSON(&m); err != nil {
				break
			}
		}
		got <- i
	}()
	for i := 0; i < n; i++ {
		a.write(map[string]any{"t": "draw", "d": big})
	}
	if i := <-got; i != n {
		t.Fatalf("the fast client got %d of %d", i, n)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		bt.s.bus.mu.Lock()
		n := len(bt.s.bus.spaces["slow"].clients)
		bt.s.bus.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slow client wasn't let go (%d in the space)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLimits(t *testing.T) {
	// Files: a total quota
	var fdir string
	bt := newBusTest(t, nil, func(s *server) {
		fdir = t.TempDir()
		s.files = &fileStore{dir: fdir, maxSize: 1 << 20, keep: time.Hour, quota: 100}
	})
	s := bt.s
	tok := url.QueryEscape(bt.token("U", false))
	post := func(path, body string) int {
		res, err := http.Post(bt.srv.URL+path, "", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := post("/files?token="+tok+"&name=a.txt", strings.Repeat("a", 60)); c != 200 {
		t.Fatalf("first file: %d", c)
	}
	if c := post("/files?token="+tok+"&name=b.txt", strings.Repeat("b", 60)); c != http.StatusInsufficientStorage {
		t.Fatalf("over the quota: %d", c)
	}
	s.files.recount()
	s.files.mu.Lock()
	used, reserved := s.files.used, s.files.reserved
	s.files.mu.Unlock()
	if used != 60 || reserved != 0 {
		t.Fatalf("count: %d %d", used, reserved)
	}

	// Backups: by count and bytes
	oldN, oldB := busMaxBackups, busBackupBytes
	defer func() { busMaxBackups, busBackupBytes = oldN, oldB }()
	busMaxBackups, busBackupBytes = 2, 20
	id := func(c string) string { return strings.Repeat(c, 32) }
	if c := post("/backup/"+id("a")+"?token="+tok, "0123456789"); c != 200 {
		t.Fatalf("backup 1: %d", c)
	}
	if c := post("/backup/"+id("a")+"?token="+tok, "0123456789"); c != 200 {
		t.Fatalf("backup 1 again: %d", c)
	}
	if c := post("/backup/"+id("b")+"?token="+tok, "01234567890"); c != http.StatusInsufficientStorage {
		t.Fatalf("too many bytes: %d", c)
	}
	if c := post("/backup/"+id("b")+"?token="+tok, "0123456789"); c != 200 {
		t.Fatalf("backup 2: %d", c)
	}
	busBackupBytes = 1000
	if c := post("/backup/"+id("c")+"?token="+tok, "x"); c != http.StatusInsufficientStorage {
		t.Fatalf("too many backups: %d", c)
	}
}

func TestRooms(t *testing.T) {
	s, srv := newTestServer(t, func(c *config) { c.maxRooms = 1 })
	// A live streamer's room isn't taken over; once its connection is gone, it is
	first, second := &conn{}, &conn{}
	if _, err := s.openRoom("r", first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.openRoom("r", second); err == nil {
		t.Fatal("took over a live stream")
	}
	first.gone.Store(true)
	if _, err := s.openRoom("r", second); err != nil {
		t.Fatalf("restart after the streamer left: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // the old room's endRoom runs on its own
	s.mu.Lock()
	delete(s.rooms, "r")
	s.mu.Unlock()

	uid := newUID(t)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/streams?token=" + url.QueryEscape(s.makeToken(uid))
	try := func(room, sdp string) message {
		ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		_ = ws.WriteJSON(message{Type: "publish", Room: room, SDP: sdp})
		var m message
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("publish %s: %v", room, err)
		}
		return m
	}
	// Only as yourself: another's name, or one that merely starts like yours, is refused
	for _, room := range []string{"someone-else", "someone-else:" + uid, uid + "x"} {
		if m := try(room, "not an offer"); m.Type != "error" || !strings.Contains(m.Message, "yourself") {
			t.Fatalf("publish as %s: %+v", room, m)
		}
	}
	// A publish that fails leaves no room behind (else max-rooms of them block every stream)
	for i := 0; i < 3; i++ {
		if m := try(uid+":screen", "not an offer"); m.Type != "error" || strings.Contains(m.Message, "too many") {
			t.Fatalf("publish %d: %+v", i, m)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			s.mu.Lock()
			n := len(s.rooms)
			s.mu.Unlock()
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a failed publish left %d rooms", n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// A signature an app made for another server (v2 binds the host) is refused here: a hostile server can't relay
// our challenge to an app and sign in as it.
func TestAuthV2BoundToHost(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	pub := base64.StdEncoding.EncodeToString(der)
	nonce := []byte("0123456789abcdef0123456789abcdef")
	sign := func(host string) []byte {
		h := sha256.Sum256(signedV2(host, nonce))
		s, _ := ecdsa.SignASN1(rand.Reader, k, h[:])
		return s
	}
	if _, err := verifyIdentityV2(pub, "voice.example.com:443", nonce, sign("VOICE.example.com")); err != nil {
		t.Fatal("same host refused:", err)
	}
	if _, err := verifyIdentityV2(pub, "voice.example.com", nonce, sign("evil.example.net")); err == nil {
		t.Fatal("another server's signature accepted")
	}
	if _, err := verifyIdentityV2(pub, "[::1]:7880", nonce, sign("::1")); err != nil {
		t.Fatal("ipv6 host refused:", err)
	}
}
