package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dialBot signs in the way docs/bots/echo does: v2 (the host signed), client "bot/…", a device id.
func dialBot(t *testing.T, hs *httptest.Server, key *ecdsa.PrivateKey, extra map[string]any) (*tclient, map[string]any) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+"/v1/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	var ch map[string]any
	if err := ws.ReadJSON(&ch); err != nil || ch["t"] != "challenge" {
		t.Fatalf("challenge: %v %v", ch, err)
	}
	nonce, _ := base64.StdEncoding.DecodeString(ch["n"].(string))
	u, _ := url.Parse(hs.URL)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	digest := sha256.Sum256(append([]byte("spectrevoice-auth-v2:"+strings.ToLower(u.Hostname())+"\n"), nonce...))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	hello := map[string]any{"t": "hello", "v": 2, "pub": base64.StdEncoding.EncodeToString(spki), "sig": base64.StdEncoding.EncodeToString(sig),
		"name": "Echo", "client": "bot/echo-test", "dev": "echo-test"}
	for k, v := range extra {
		hello[k] = v
	}
	_ = ws.WriteJSON(hello)
	var w map[string]any
	if err := ws.ReadJSON(&w); err != nil {
		t.Fatal(err)
	}
	if w["t"] != "welcome" {
		_ = ws.Close()
		return nil, w
	}
	in := make(chan map[string]any, 1024)
	c := &tclient{t: t, ws: ws, key: key, in: in, uid: w["uid"].(string), sid: int(w["sid"].(float64)), tok: w["token"].(string), wel: w}
	go func() {
		defer close(in)
		for {
			var m map[string]any
			if ws.ReadJSON(&m) != nil {
				return
			}
			in <- m
		}
	}()
	t.Cleanup(func() { _ = ws.Close() })
	return c, nil
}

func uidOfKey(k *ecdsa.PrivateKey) string {
	spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return uidOf(spki)
}

// A manager registers a bot's key; the bot connects as the echo example does, is marked a bot, answers in the
// chat, holds only the permissions it was given, and never needs the server password.
func TestBotsAPI(t *testing.T) {
	anaKey, own := ownerKeyCfg(t)
	s, hs := newTestServer(t, func(c *config) { own(c); c.joinPass = "pw" })
	t.Cleanup(s.bus.conns.Wait)
	ana, _ := dialV(t, hs, anaKey, "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "pw")
	clubID := spaceID
	general := chanID(t, ana.wel, "general")
	lobby := chanID(t, ana.wel, "Lobby")
	botKey := newKey(t)
	botUID := uidOfKey(botKey)

	// Not registered yet: it's just a visitor, not a bot
	if v, _ := dialBot(t, hs, botKey, map[string]any{"password": "pw"}); v == nil || v.wel["bot"] == true {
		t.Fatal("an unregistered key was taken for a bot")
	}
	// Bob (no manage_community) can't register bots
	bob.send(map[string]any{"t": "bot-add", "uid": botUID, "name": "Echo"})
	if e := bob.wait(isT("error")); e["why"] != "role" {
		t.Fatal(e)
	}
	// Ana can't register a person (Bob) as a bot
	ana.send(map[string]any{"t": "bot-add", "uid": bob.uid, "name": "Bobbot"})
	if e := ana.wait(isT("error")); e["why"] != "taken" {
		t.Fatal(e)
	}
	// Nor the visitor's key: it connected as a person
	ana.send(map[string]any{"t": "bot-add", "uid": botUID, "name": "Echo"})
	if e := ana.wait(isT("error")); e["why"] != "taken" {
		t.Fatal(e)
	}
	// A fresh key: in, with only the permissions that may be given (administrator and manage_roles never)
	botKey = newKey(t)
	botUID = uidOfKey(botKey)
	ana.send(map[string]any{"t": "bot-add", "uid": botUID, "name": "Echo Bot", "perms": []string{"administrator", "manage_roles", "move", "kick"}})
	added := ana.wait(isT("bot-added"))["bot"].(map[string]any)
	if added["uid"] != botUID || added["name"] != "Echo Bot" || added["kind"] != "bot:echo-bot" || fmt.Sprint(added["perms"]) != "[kick move]" {
		t.Fatal(added)
	}
	ana.send(map[string]any{"t": "bots"})
	if l := ana.wait(isT("bots"))["items"].([]any); len(l) != 1 || l[0].(map[string]any)["uid"] != botUID {
		t.Fatal(l)
	}

	// In without the server password
	bot, e := dialBot(t, hs, botKey, nil)
	if bot == nil {
		t.Fatal(e)
	}
	if bot.wel["bot"] != true || fmt.Sprint(bot.wel["perms"]) != "[kick move]" {
		t.Fatal(bot.wel["bot"], bot.wel["perms"])
	}
	// Ana sees it come, marked as a bot, with the name its manager gave it
	m := ana.wait(func(m map[string]any) bool { return m["t"] == "member" && m["m"].(map[string]any)["uid"] == botUID })["m"].(map[string]any)
	if m["bot"] != true || m["name"] != "Echo Bot" {
		t.Fatal(m)
	}
	// It doesn't take a place
	s.hub.mu.Lock()
	people := s.hub.people()
	s.hub.mu.Unlock()
	if people != 3 {
		t.Fatalf("people %d", people)
	}

	// The echo example's loop, in short: "!ping" in a chat it reads gets "pong"; the rest stays for the checks below
	rest, raw := make(chan map[string]any, 1024), bot.in
	go func() {
		defer close(rest)
		for m := range raw {
			if m["t"] != "msg" {
				rest <- m
				continue
			}
			msg := m["m"].(map[string]any)
			if msg["from"] != botUID && msg["text"] == "!ping" {
				bot.send(map[string]any{"t": "say", "to": msg["to"], "text": "pong"})
			}
		}
	}()
	bot.in = rest
	ana.send(map[string]any{"t": "say", "to": chanConv(general), "text": "!ping"})
	pong := ana.wait(func(m map[string]any) bool { return m["t"] == "msg" && m["m"].(map[string]any)["text"] == "pong" })["m"].(map[string]any)
	if pong["from"] != botUID || pong["name"] != "Echo Bot" {
		t.Fatal(pong)
	}
	// It may move people without a role (move), not the owner
	ana.send(map[string]any{"t": "ch-save", "name": "Games"})
	games := int64(ana.wait(isT("channel"))["c"].(map[string]any)["id"].(float64))
	bot.send(map[string]any{"t": "move", "sid": ana.sid, "ch": games})
	if e := bot.wait(isT("error")); e["op"] != "move" || e["why"] != "role" {
		t.Fatal(e)
	}
	bot.send(map[string]any{"t": "move", "sid": bob.sid, "ch": games})
	if mv := bob.wait(isT("moved")); int64(mv["ch"].(float64)) != games || mv["by"] != "Echo Bot" {
		t.Fatal(mv)
	}
	_ = lobby

	// Not beyond its permissions, and never the things kept from bots
	bot2, _ := dialBot(t, hs, botKey, map[string]any{"dev": "second"})
	bot2.send(map[string]any{"t": "ch-save", "name": "Nope"})
	if e := bot2.wait(isT("error")); e["op"] != "ch-save" || e["why"] != "role" {
		t.Fatal(e)
	}
	for _, msg := range []map[string]any{
		{"t": "rtc", "type": "offer", "sdp": "v=0"}, // no speak permission: no voice
		{"t": "server-save", "name": "Bots' server"},
		{"t": "community-join", "invite": "abcdefgh"},
		{"t": "invite-create"},
		{"t": "name", "name": "Admin"},
		{"t": "bot-add", "uid": uidOfKey(newKey(t)), "name": "Friend"},
		{"t": "role-save", "name": "Boss", "perms": []string{"administrator"}},
	} {
		bot2.send(msg)
		if e := bot2.wait(isT("error")); e["op"] != msg["t"] || e["why"] != "bot" {
			t.Fatalf("%v: %v", msg["t"], e)
		}
	}
	// Still itself
	s.hub.mu.Lock()
	for _, o := range s.hub.sessions {
		if o.uid == botUID && (o.comm != clubID || o.name != "Echo Bot" || !o.bot || !o.api) {
			s.hub.mu.Unlock()
			t.Fatalf("bot session changed: comm %d name %q", o.comm, o.name)
		}
	}
	s.hub.mu.Unlock()
	// Its own bus kind: it writes, a member can't, everyone reads
	busURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/bus?space=" + spaceOf(clubID) + "&token="
	bws, _, err := websocket.DefaultDialer.Dial(busURL+url.QueryEscape(bot.tok), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bws.Close()
	var st map[string]any
	_ = bws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := bws.ReadJSON(&st); err != nil || st["t"] != "state" {
		t.Fatalf("bus state: %v %v", st, err)
	}
	_ = bws.WriteJSON(map[string]any{"t": "put", "kind": "bot:echo-bot", "id": "counter", "v": map[string]any{"n": 1}})
	var put map[string]any
	if err := bws.ReadJSON(&put); err != nil || put["t"] != "put" || put["kind"] != "bot:echo-bot" {
		t.Fatalf("bot put: %v %v", put, err)
	}
	_ = bws.WriteJSON(map[string]any{"t": "put", "kind": "bot:other", "id": "x", "v": map[string]any{}})
	var ref map[string]any
	if err := bws.ReadJSON(&ref); err != nil || ref["t"] != "error" || ref["why"] != "owner" {
		t.Fatalf("another bot's kind: %v %v", ref, err)
	}
	// Ana (a member who isn't a manager would be refused; she manages, so she may clear it) reads it in the state
	aws, _, err := websocket.DefaultDialer.Dial(busURL+url.QueryEscape(ana.tok), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer aws.Close()
	_ = aws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := aws.ReadJSON(&st); err != nil || st["state"].(map[string]any)["bot:echo-bot"] == nil {
		t.Fatalf("state without the bot's kind: %v %v", st, err)
	}
	// Bob, a member, can't write the bot's kind
	wws, _, err := websocket.DefaultDialer.Dial(busURL+url.QueryEscape(bob.tok), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer wws.Close()
	_ = wws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = wws.ReadJSON(&st)
	_ = wws.WriteJSON(map[string]any{"t": "put", "kind": "bot:echo-bot", "id": "counter", "v": map[string]any{"n": 2}})
	if err := wws.ReadJSON(&ref); err != nil || ref["t"] != "error" || ref["why"] != "owner" {
		t.Fatalf("a member wrote the bot's kind: %v %v", ref, err)
	}

	// Removed: its connections go, and the key is a stranger again
	ana.send(map[string]any{"t": "bot-del", "uid": botUID})
	if d := ana.wait(isT("bot-deleted")); d["uid"] != botUID {
		t.Fatal(d)
	}
	bot2.wait(isT("kicked"))
	ana.send(map[string]any{"t": "bots"})
	if l := ana.wait(isT("bots"))["items"].([]any); len(l) != 0 {
		t.Fatal(l)
	}
	if s.store.isMember(clubID, botUID) {
		t.Fatal("still a member")
	}
}

// A bot's message budget is the smaller one.
func TestBotRateLimit(t *testing.T) {
	oldR, oldB := botRatePerSec, botRateBurst
	botRatePerSec, botRateBurst = 0.001, 5
	t.Cleanup(func() { botRatePerSec, botRateBurst = oldR, oldB })
	anaKey, own := ownerKeyCfg(t)
	s, hs := newTestServer(t, own)
	// Before the budget goes back (the cleanup above runs last): the sessions, which read it on every message,
	// have ended (the clients' sockets close first). Else the race detector sees the reset and fails the run.
	t.Cleanup(func() {
		for i := 0; i < 200; i++ {
			s.hub.mu.Lock()
			n := len(s.hub.sessions)
			s.hub.mu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	ana, _ := dialV(t, hs, anaKey, "Ana", "")
	botKey := newKey(t)
	ana.send(map[string]any{"t": "bot-add", "uid": uidOfKey(botKey), "name": "Fast", "perms": []string{}})
	ana.wait(isT("bot-added"))
	bot, e := dialBot(t, hs, botKey, nil)
	if bot == nil || bot.wel["bot"] != true {
		t.Fatal(e)
	}
	for i := 0; i < 8; i++ {
		bot.send(map[string]any{"t": "ping", "n": i})
	}
	if e := bot.wait(isT("error")); e["why"] != "rate" {
		t.Fatal(e)
	}
	// People keep the normal budget
	for i := 0; i < 8; i++ {
		ana.send(map[string]any{"t": "ping", "n": i})
	}
	for i := 0; i < 8; i++ {
		ana.wait(isT("pong"))
	}
	_ = s
}
