package main

import (
	"strings"
	"testing"
	"time"
)

func isTVParty(k string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["t"] == "tv-party" && m["k"] == k }
}

// tvWatch puts uids among the TV room's viewers (as /streams subscribe does), making the room when there's none.
func tvWatch(s *server, uids ...string) *room {
	s.mu.Lock()
	rm := s.rooms[tvRoom]
	if rm == nil {
		rm = &room{name: tvRoom, subs: map[*subscriber]bool{}, local: true, onViewers: s.tv.changed}
		s.rooms[tvRoom] = rm
	}
	s.mu.Unlock()
	rm.mu.Lock()
	for _, u := range uids {
		rm.subs[&subscriber{uid: u}] = true
	}
	rm.mu.Unlock()
	rm.onViewers()
	return rm
}

func TestTVParty(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.tv = true; c.ffmpeg = "ffmpeg-not-here" })
	a, _ := dialV(t, hs, newKey(t), "Ana", "")
	b, _ := dialV(t, hs, newKey(t), "Bogdan", "")
	c, _ := dialV(t, hs, newKey(t), "Cristi", "")
	if w, _ := tvState(a.wel)["watching"].([]any); w == nil || len(w) != 0 || tvState(a.wel)["watchers"] != 0.0 {
		t.Fatalf("welcome watching: %v", a.wel["tv"])
	}
	// Not watching: no reactions
	a.send(map[string]any{"t": "tv", "op": "react", "e": "🔥"})
	if m := a.wait(isTVErr); m["why"] != "watch" || m["what"] != "react" {
		t.Fatalf("not watching: %v", m)
	}
	tvWatch(s, a.uid, b.uid)
	// Everyone sees who watches (the row of faces), C too
	m := c.wait(func(m map[string]any) bool { return isTV(m) && tvState(m)["watchers"] == 2.0 })
	w := tvState(m)["watching"].([]any)
	if len(w) != 2 || !(w[0] == a.uid || w[1] == a.uid) || !(w[0] == b.uid || w[1] == b.uid) {
		t.Fatalf("watching: %v", w)
	}
	a.send(map[string]any{"t": "tv", "op": "react", "e": "🔥"})
	for _, x := range []*tclient{a, b} {
		r := x.wait(isTVParty("react"))
		if r["e"] != "🔥" || r["uid"] != a.uid || r["name"] != "Ana" {
			t.Fatalf("react: %v", r)
		}
	}
	for _, e := range []string{"hello", "", "🔥 🔥", "<b>", strings.Repeat("🔥", 9)} {
		a.send(map[string]any{"t": "tv", "op": "react", "e": e})
		if m := a.wait(isTVErr); m["why"] != "bad" {
			t.Fatalf("react %q: %v", e, m)
		}
	}
	a.send(map[string]any{"t": "tv", "op": "react", "e": "GG"})
	b.wait(func(m map[string]any) bool { return isTVParty("react")(m) && m["e"] == "GG" })
	// A line over the picture, cleaned and cut
	b.send(map[string]any{"t": "tv", "op": "say", "text": "  ce   gol!\n" + strings.Repeat("a", 300)})
	l := a.wait(isTVParty("say"))
	if txt := l["text"].(string); !strings.HasPrefix(txt, "ce gol! aaa") || len([]rune(txt)) != tvSayMax || l["name"] != "Bogdan" {
		t.Fatalf("say: %v", l)
	}
	b.send(map[string]any{"t": "tv", "op": "say", "text": "   "})
	if m := b.wait(isTVErr); m["why"] != "bad" {
		t.Fatalf("empty line: %v", m)
	}
	// Not watching: neither sends nor gets anything
	c.send(map[string]any{"t": "tv", "op": "say", "text": "hi"})
	if m := c.wait(isTVErr); m["why"] != "watch" {
		t.Fatalf("c says: %v", m)
	}
	c.send(map[string]any{"t": "ping", "n": 1})
	c.wait(isT("pong"))
	for _, m := range c.seen {
		if m["t"] == "tv-party" {
			t.Fatalf("c got the party: %v", m)
		}
	}
	// Muted in chat: no lines
	s.mods.put(ban{UID: b.uid, Kind: "mute", Ch: spaceID, By: "test"})
	b.send(map[string]any{"t": "tv", "op": "say", "text": "still here"})
	if m := b.wait(isTVErr); m["why"] != "mute" {
		t.Fatalf("muted: %v", m)
	}
	// Too fast: refused (each person's own budget)
	refused := 0
	for i := 0; i < 15; i++ {
		a.send(map[string]any{"t": "tv", "op": "react", "e": "😂"})
	}
	a.send(map[string]any{"t": "ping", "n": 2})
	a.wait(isT("pong"))
	for _, m := range a.seen {
		if isTVErr(m) && m["why"] == "rate" {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("no reaction refused")
	}
	// Watching ends: the row empties
	s.mu.Lock()
	rm := s.rooms[tvRoom]
	delete(s.rooms, tvRoom)
	s.mu.Unlock()
	rm.onViewers()
	c.wait(func(m map[string]any) bool { return isTV(m) && tvState(m)["watchers"] == 0.0 })
}

func TestTVPartyRoomCap(t *testing.T) {
	p := newTVParty()
	now := time.Unix(100, 0)
	passed := 0
	for sid := 1; sid <= 40; sid++ {
		if ok, room := p.allow(sid, false, now); ok && room {
			passed++
		}
	}
	if passed != tvRoomReactsPerSec {
		t.Fatalf("passed %d", passed)
	}
	if ok, room := p.allow(1, false, now.Add(time.Second)); !ok || !room {
		t.Fatal("the next second")
	}
	// Lines: three at once, then one every 1.5 s
	for i := 0; i < 3; i++ {
		if ok, _ := p.allow(9, true, now); !ok {
			t.Fatalf("line %d", i)
		}
	}
	if ok, _ := p.allow(9, true, now); ok {
		t.Fatal("fourth line")
	}
	if ok, _ := p.allow(9, true, now.Add(1600*time.Millisecond)); !ok {
		t.Fatal("after 1.5 s")
	}
}
