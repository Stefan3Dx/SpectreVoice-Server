package main

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestOwnerGivesLei(t *testing.T) {
	ownerKey, own := ownerKeyCfg(t)
	s, hs := newTestServer(t, func(c *config) { own(c); c.leiMax = 5000; c.leiLegacy = false }) // the old apps' add refused (SV_LEI_LEGACY=0)
	ana, _ := dialV(t, hs, ownerKey, "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	cara, _ := dialV(t, hs, newKey(t), "Cara", "")
	if !hasPerm(ana.wel, "give_lei") || hasPerm(bob.wel, "give_lei") {
		t.Fatal(ana.wel["perms"], bob.wel["perms"])
	}
	// Bob's bus, to see his purse move
	u := "ws" + strings.TrimPrefix(hs.URL, "http") + "/bus?space=" + mainSpace + "&token=" + url.QueryEscape(s.makeToken(bob.uid))
	bus, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	var st map[string]any
	if bus.ReadJSON(&st) != nil || st["t"] != "state" {
		t.Fatal(st)
	}

	// An ordinary person can't
	bob.send(map[string]any{"t": "lei-give", "uid": cara.uid, "amount": 100})
	if e := bob.wait(isErrOp("lei-give")); e["why"] != "role" {
		t.Fatal(e)
	}
	// Bounds
	for _, a := range []any{0.0, 5001.0, -5001.0, 1.5} {
		ana.send(map[string]any{"t": "lei-give", "uid": bob.uid, "amount": a})
		if e := ana.wait(isErrOp("lei-give")); e["why"] != "bad" {
			t.Fatal(a, e)
		}
	}
	ana.send(map[string]any{"t": "lei-give", "uid": "nobody-here", "amount": 10})
	if e := ana.wait(isErrOp("lei-give")); e["why"] != "gone" {
		t.Fatal(e)
	}
	// The owner gives 2500: a new purse starts at 1000, so 3500; Bob is told, the bus has it
	ana.send(map[string]any{"t": "lei-give", "uid": bob.uid, "amount": 2500, "reason": "won the  tournament"})
	if d := ana.wait(isDone("lei-give")); d["balance"].(float64) != 3500 {
		t.Fatal(d)
	}
	g := bob.wait(isT("lei-gift"))
	if g["amount"].(float64) != 2500 || g["byName"] != "Ana" || g["reason"] != "won the tournament" || g["balance"].(float64) != 3500 {
		t.Fatal(g)
	}
	for {
		var m map[string]any
		if err := bus.ReadJSON(&m); err != nil {
			t.Fatal(err)
		}
		if m["t"] == "put" && m["kind"] == "lei" && m["id"] == bob.uid {
			if v := m["v"].(map[string]any); v["v"].(float64) != 3500 || v["_by"] != ana.uid {
				t.Fatal(v)
			}
			break
		}
	}
	// Taking more than there is leaves 0
	ana.send(map[string]any{"t": "lei-give", "uid": bob.uid, "amount": -5000})
	if d := ana.wait(isDone("lei-give")); d["balance"].(float64) != 0 {
		t.Fatal(d)
	}
	// An app's own add (an app from before the pots) is refused, and Bob is told once to update
	for k := 0; k < 2; k++ {
		_ = bus.WriteJSON(map[string]any{"t": "add", "kind": "lei", "id": bob.uid, "d": 300.0, "start": 1000.0, "ref": "x"})
		for {
			var m map[string]any
			if err := bus.ReadJSON(&m); err != nil {
				t.Fatal(err)
			}
			if m["t"] == "put" && m["kind"] == "lei" && m["from"] == bob.uid {
				t.Fatal("an app added to a purse", m)
			}
			if m["t"] == "error" && m["ref"] == "x" {
				if m["why"] != "old" {
					t.Fatal(m)
				}
				break
			}
		}
	}
	if a := bob.wait(isT("announce")); a["text"] != leiOldNotice {
		t.Fatal(a)
	}

	// The log, for admins
	cara.send(map[string]any{"t": "lei-log"})
	if e := cara.wait(isErrOp("lei-log")); e["why"] != "role" {
		t.Fatal(e)
	}
	ana.send(map[string]any{"t": "lei-log"})
	items := ana.wait(isT("lei-log"))["items"].([]any)
	if len(items) != 2 || items[1].(map[string]any)["reason"] != "won the tournament" || items[1].(map[string]any)["toName"] != "Bob" || items[0].(map[string]any)["amount"].(float64) != -5000 {
		t.Fatal(items)
	}

	// Only the owner gives give_lei: an administrator (staff, not owner) doesn't have it and can't hand it out
	if err := s.store.setRole(cara.uid, roleAdmin); err != nil {
		t.Fatal(err)
	}
	cara.ws.Close()
	cara, _ = dialV(t, hs, cara.key, "Cara", "")
	if hasPerm(cara.wel, "give_lei") || !hasPerm(cara.wel, "manage_roles") {
		t.Fatal(cara.wel["perms"])
	}
	cara.send(map[string]any{"t": "role-save", "name": "Banker", "pos": 5, "perms": []string{"give_lei", "move"}})
	r := ana.wait(func(m map[string]any) bool { return m["t"] == "roles" })
	var banker map[string]any
	for _, x := range r["items"].([]any) {
		if x.(map[string]any)["name"] == "Banker" {
			banker = x.(map[string]any)
		}
	}
	if banker == nil || hasPerm(banker, "give_lei") || !hasPerm(banker, "move") {
		t.Fatal(banker)
	}
	cara.send(map[string]any{"t": "lei-give", "uid": bob.uid, "amount": 10})
	if e := cara.wait(isErrOp("lei-give")); e["why"] != "role" {
		t.Fatal(e)
	}
	// The owner gives the role give_lei and puts Bob in it; Cara (admin) can't take Bob out of it
	bid := banker["id"].(float64)
	ana.send(map[string]any{"t": "role-save", "id": bid, "name": "Banker", "pos": 5, "perms": []string{"give_lei", "move"}})
	ana.wait(func(m map[string]any) bool { return m["t"] == "roles" })
	ana.send(map[string]any{"t": "member-roles", "uid": bob.uid, "roles": []any{bid}})
	bob.wait(func(m map[string]any) bool { return m["t"] == "role" && hasPerm(m, "give_lei") })
	cara.send(map[string]any{"t": "member-roles", "uid": bob.uid, "roles": []any{}})
	cara.wait(isDone("member-roles"))
	bob.send(map[string]any{"t": "lei-give", "uid": cara.uid, "amount": 7, "reason": "thanks"})
	if d := bob.wait(isDone("lei-give")); d["balance"].(float64) != 1007 {
		t.Fatal(d)
	}
	// Nor change the role's give_lei, nor delete it
	cara.send(map[string]any{"t": "role-save", "id": bid, "name": "Banker", "pos": 5, "perms": []string{"move"}})
	ana.wait(func(m map[string]any) bool { return m["t"] == "roles" })
	s.hub.mu.Lock()
	still := s.hub.access(spaceID).roles[int64(bid)].p&pGiveLei != 0
	s.hub.mu.Unlock()
	if !still {
		t.Fatal("an admin took give_lei off the role")
	}
	cara.send(map[string]any{"t": "role-del", "id": bid})
	if e := cara.wait(isErrOp("role-del")); e["why"] != "role" {
		t.Fatal(e)
	}
}

// The day's bonus is the server's: once a day, a day within the server's.
func TestLeiDaily(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	a := bt.dial("daily", "A")
	now := time.Now().UTC()
	day := func(d int) string { return now.AddDate(0, 0, d).Format("2006-01-02") }
	daily := func(d string) []map[string]any {
		return a.do(map[string]any{"t": "lei", "op": "daily", "day": d, "name": "  Ana  B ", "ref": "d1"})
	}
	mustTake(t, daily(day(0)))
	bt.s.bus.mu.Lock()
	v, rec := bt.s.bus.spaces["daily"].purseOf("A")
	bt.s.bus.mu.Unlock()
	if v != 1100 || rec["d"] != day(0) || rec["n"] != "Ana B" {
		t.Fatal(v, rec)
	}
	if e := mustRefuse(t, daily(day(0)), "lei", "once"); e["ref"] != "d1" {
		t.Fatal(e)
	}
	mustRefuse(t, daily(day(-1)), "lei", "once")
	mustRefuse(t, daily(day(2)), "lei", "day")
	mustRefuse(t, daily(day(-3)), "lei", "day")
	mustRefuse(t, daily("tomorrow"), "lei", "day")
	// The apps' own day may be a day ahead (their midnight came first)
	mustTake(t, daily(day(1)))
	if bt.purse("daily", "A") != 1200 {
		t.Fatal(bt.purse("daily", "A"))
	}
	mustRefuse(t, daily(day(1)), "lei", "once")
}

// A gift: one's own Lei, all or nothing, to someone else on the server; the recipient is told.
func TestLeiSend(t *testing.T) {
	old := leiSendRate
	leiSendRate = 4
	defer func() { leiSendRate = old }()
	bt := newBusTest(t, nil, nil)
	ua, ub := newUID(t), newUID(t)
	a := bt.dial(mainSpace, ua)
	b := bt.dial(mainSpace, ub)
	send := func(to string, n any) []map[string]any {
		return a.do(map[string]any{"t": "lei", "op": "send", "to": to, "n": n, "ref": "g1"})
	}
	mustTake(t, send(ub, 300.0))
	if bt.purse(mainSpace, ua) != 700 || bt.purse(mainSpace, ub) != 1300 {
		t.Fatal(bt.purse(mainSpace, ua), bt.purse(mainSpace, ub))
	}
	got := false
	for _, m := range b.sync() {
		if m["t"] == "lei-got" {
			if m["from"] != ua || m["n"] != 300.0 {
				t.Fatal(m)
			}
			got = true
		}
	}
	if !got {
		t.Fatal("not told")
	}
	for _, n := range []any{0.0, 1.5, 100001.0, "5", -10.0} {
		mustRefuse(t, send(ub, n), "lei", "bad")
	}
	mustRefuse(t, send(ua, 10.0), "lei", "member")
	mustRefuse(t, send(newUID(t), 10.0), "lei", "member") // not on this server
	if e := mustRefuse(t, send(ub, 701.0), "lei", "funds"); e["ref"] != "g1" {
		t.Fatal(e)
	}
	mustTake(t, send(ub, 700.0))
	if bt.purse(mainSpace, ua) != 0 || bt.purse(mainSpace, ub) != 2000 {
		t.Fatal(bt.purse(mainSpace, ua), bt.purse(mainSpace, ub))
	}
	// Ten a minute (four here)
	mustRefuse(t, send(ub, 1.0), "lei", "funds")
	mustRefuse(t, send(ub, 1.0), "lei", "rate")
	// A gift isn't relayed as a message to the others either
	for _, m := range b.sync() {
		if m["t"] == "lei" {
			t.Fatal("relayed", m)
		}
	}
}
