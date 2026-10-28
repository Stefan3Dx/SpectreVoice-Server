package main

import (
	"crypto/x509"
	"strings"
	"testing"
)

// Channel passwords: a few tries a minute, then a pause (bcrypt runs outside the hub's lock).
func TestJoinPasswordTries(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	_, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	a, _ := dialV(t, hs, ownerKey, "Ana", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	a.send(map[string]any{"t": "ch-save", "name": "Secret", "password": "pw"})
	secret := int64(a.wait(isT("channel"))["c"].(map[string]any)["id"].(float64))
	for i := 0; i < 5; i++ {
		b.send(map[string]any{"t": "join", "ch": secret, "password": "nope"})
		if e := b.wait(isT("error")); e["why"] != "password" {
			t.Fatal(i, e)
		}
	}
	b.send(map[string]any{"t": "join", "ch": secret, "password": "pw"})
	if e := b.wait(isT("error")); e["why"] != "rate" {
		t.Fatal("no pause after five tries:", e)
	}
}

// Lifting a channel ban: only by who ranks above, never for oneself.
func TestChannelUnbanScoped(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	a, _ := dialV(t, hs, ownerKey, "Ana", "")
	eve, _ := dialV(t, hs, newKey(t), "Eve", "")
	lobby := chanID(t, a.wel, "Lobby")
	a.send(map[string]any{"t": "ch-ban", "uid": eve.uid, "ch": lobby})
	a.wait(func(m map[string]any) bool { return m["t"] == "done" && m["op"] == "ch-ban" })
	eve.send(map[string]any{"t": "ch-unban", "uid": eve.uid, "ch": lobby})
	if e := eve.wait(isT("error")); e["why"] != "role" {
		t.Fatal(e)
	}
	if !s.mods.channelBanned(eve.uid, lobby) {
		t.Fatal("the ban was lifted by the banned person")
	}
}

// Private messages and pokes go only to people of this server, by a real uid.
func TestDirectMessageTargets(t *testing.T) {
	_, hs := newTestServer(t, nil)
	a, _ := dialV(t, hs, newKey(t), "Ana", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	for _, to := range []string{"u:AAAAAAAAAAAAAAAAAAAA", "u:Z:" + b.uid} {
		a.send(map[string]any{"t": "say", "to": to, "text": "hi"})
		if e := a.wait(isT("error")); e["why"] != "bad" {
			t.Fatal(to, e)
		}
	}
	a.send(map[string]any{"t": "poke", "uid": "AAAAAAAAAAAAAAAAAAAA"})
	if e := a.wait(isT("error")); e["why"] != "gone" {
		t.Fatal(e)
	}
	a.send(map[string]any{"t": "say", "to": "u:" + b.uid, "text": "salut"})
	b.wait(func(m map[string]any) bool { return m["t"] == "msg" })
}

// A bad offer doesn't leave a half-made voice connection behind to pile candidates on.
func TestVoiceBadOfferDropped(t *testing.T) {
	s, hs := newTestServer(t, nil)
	a, _ := dialV(t, hs, newKey(t), "Ana", "")
	a.send(map[string]any{"t": "rtc", "type": "offer", "sdp": "x"})
	if e := a.wait(isT("error")); e["why"] != "bad" {
		t.Fatal(e)
	}
	sid := int(a.wel["sid"].(float64))
	big := strings.Repeat("a", 4000)
	for i := 0; i < 50; i++ {
		a.send(map[string]any{"t": "ice", "c": big})
	}
	if p := s.voice.peer(sid); p != nil {
		p.mu.Lock()
		n := len(p.early)
		p.mu.Unlock()
		t.Fatal("peer kept after a bad offer, early candidates:", n)
	}
}

// One person streams two rooms at most.
func TestStreamRoomsPerPerson(t *testing.T) {
	s, _ := newTestServer(t, nil)
	for i, name := range []string{"u", "u:cam", "u:third"} {
		_, err := s.openRoom(name, "u", &conn{})
		if (i < 2) != (err == nil) {
			t.Fatal(name, err)
		}
	}
	if _, err := s.openRoom("v", "v", &conn{}); err != nil {
		t.Fatal("another person refused:", err)
	}
}
