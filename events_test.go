package main

import (
	"testing"
	"time"
)

func TestScheduledEvents(t *testing.T) {
	ownerKey, own := ownerKeyCfg(t)
	s, hs := newTestServer(t, func(c *config) { own(c); c.eventLead = 15 })
	ana, _ := dialV(t, hs, ownerKey, "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	lobby := chanID(t, ana.wel, "Lobby")
	start := time.Now().Add(time.Hour).UnixMilli()

	// Bob (no permission) can't make one
	bob.send(map[string]any{"t": "event-save", "title": "Mine", "start": start})
	if e := bob.wait(isErrOp("event-save")); e["why"] != "role" {
		t.Fatal(e)
	}
	// Bad times are refused
	ana.send(map[string]any{"t": "event-save", "title": "Old", "start": time.Now().Add(-3 * time.Hour).UnixMilli()})
	if e := ana.wait(isErrOp("event-save")); e["why"] != "bad" {
		t.Fatal(e)
	}
	ana.send(map[string]any{"t": "event-save", "title": "Backwards", "start": start, "end": start - 1})
	if e := ana.wait(isErrOp("event-save")); e["why"] != "bad" {
		t.Fatal(e)
	}
	// The owner makes one: everyone is told
	ana.send(map[string]any{"t": "event-save", "title": "  Game   night ", "about": "Bring snacks", "start": start, "end": start + 2*3600_000, "ch": lobby})
	id := int64(ana.wait(isDone("event-save"))["id"].(float64))
	e := bob.wait(isT("event"))["e"].(map[string]any)
	if e["title"] != "Game night" || int64(e["start"].(float64)) != start || int64(e["ch"].(float64)) != lobby || e["byName"] != "Ana" {
		t.Fatal(e)
	}
	// Bob is going: the counts reach everyone, his own answer only him
	bob.send(map[string]any{"t": "event-rsvp", "id": id, "kind": "going"})
	e = bob.wait(isT("event"))["e"].(map[string]any)
	if e["mine"] != "going" || e["going"].(float64) != 1 {
		t.Fatal(e)
	}
	e = ana.wait(func(m map[string]any) bool {
		return m["t"] == "event" && m["e"].(map[string]any)["going"].(float64) == 1
	})["e"].(map[string]any)
	if _, has := e["mine"]; has {
		t.Fatal("someone else's answer sent", e)
	}
	// The list
	bob.send(map[string]any{"t": "events"})
	l := bob.wait(isT("events"))
	items := l["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["mine"] != "going" || l["may"] != false || l["lead"].(float64) != 15 {
		t.Fatal(l)
	}
	// Bob can't edit or cancel it
	bob.send(map[string]any{"t": "event-del", "id": id})
	if e := bob.wait(isErrOp("event-del")); e["why"] != "role" {
		t.Fatal(e)
	}

	// The notices: 15 minutes before, then at the start (once each); Bob muted gets none
	s.eventTell(time.UnixMilli(start - 20*60_000))
	s.eventTell(time.UnixMilli(start - 14*60_000))
	n := ana.wait(isT("event-notice"))
	if n["kind"] != "soon" {
		t.Fatal(n)
	}
	bob.wait(isT("event-notice"))
	bob.send(map[string]any{"t": "event-mute", "on": true})
	bob.wait(isDone("event-mute"))
	s.eventTell(time.UnixMilli(start - 13*60_000)) // told already
	s.eventTell(time.UnixMilli(start + 1000))
	if n := ana.wait(isT("event-notice")); n["kind"] != "start" {
		t.Fatal(n)
	}
	pinged(bob)
	for _, m := range bob.seen {
		if m["t"] == "event-notice" {
			t.Fatal("muted, yet told", m)
		}
	}
	// An edit moving the start tells again; a cancel reaches everyone
	ana.send(map[string]any{"t": "event-save", "id": id, "title": "Game night", "start": start + 3600_000})
	ana.wait(isDone("event-save"))
	var told int
	_ = s.store.db.QueryRow(`SELECT told FROM events WHERE id=?`, id).Scan(&told)
	if told != 0 {
		t.Fatal("told stayed", told)
	}
	ana.send(map[string]any{"t": "event-del", "id": id})
	ana.wait(isDone("event-del"))
	e = bob.wait(func(m map[string]any) bool { return m["t"] == "event" && m["e"].(map[string]any)["cancelled"] == true })["e"].(map[string]any)
	s.eventTell(time.UnixMilli(start + 3600_000 + 1000))
	pinged(ana)
	for _, m := range ana.seen {
		if m["t"] == "event-notice" {
			t.Fatal("a cancelled event told", m)
		}
	}
	// A notice missed while the server was off (over 10 minutes ago) isn't sent late
	ana.send(map[string]any{"t": "event-save", "title": "Soon", "start": time.Now().Add(-30 * time.Minute).UnixMilli()})
	ana.wait(isDone("event-save"))
	s.eventTell(time.Now())
	pinged(ana)
	for _, m := range ana.seen {
		if m["t"] == "event-notice" {
			t.Fatal("told late", m)
		}
	}
	// A moderator role (events) may make them; the role's permission carries it
	if modPerms&pEvents == 0 || everyonePerms&pEvents != 0 {
		t.Fatal("moderators can't make events")
	}
}
