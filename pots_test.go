package main

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var busSyncN atomic.Int64

// sync reads what the server sent c until it answers a marker sent now (everything c's earlier messages caused
// comes before it: the server handles one connection's messages in order).
func (c *busConn) sync() []map[string]any {
	c.t.Helper()
	ref := fmt.Sprintf("sync-%d", busSyncN.Add(1))
	c.write(map[string]any{"t": "lei", "op": "sync", "ref": ref})
	var out []map[string]any
	for {
		m := c.read()
		if m["t"] == "error" && m["ref"] == ref {
			return out
		}
		out = append(out, m)
	}
}

// do sends m and returns what came back for it.
func (c *busConn) do(m map[string]any) []map[string]any {
	c.t.Helper()
	c.write(m)
	return c.sync()
}

// doRaw sends a message as it's written (the order of its keys kept).
func (c *busConn) doRaw(js string) []map[string]any {
	c.t.Helper()
	if err := c.ws.WriteMessage(websocket.TextMessage, []byte(js)); err != nil {
		c.t.Fatal(err)
	}
	return c.sync()
}

// errOf: the refusal among msgs for op ("" any), or nil.
func errOf(msgs []map[string]any, op string) map[string]any {
	for _, m := range msgs {
		if m["t"] == "error" && (op == "" || m["op"] == op) {
			return m
		}
	}
	return nil
}

func mustRefuse(t *testing.T, msgs []map[string]any, op, why string) map[string]any {
	t.Helper()
	e := errOf(msgs, op)
	if e == nil || e["why"] != why {
		t.Fatalf("wanted %s refused (%s), got %v", op, why, msgs)
	}
	return e
}

func mustTake(t *testing.T, msgs []map[string]any) {
	t.Helper()
	if e := errOf(msgs, ""); e != nil {
		t.Fatalf("refused: %v", e)
	}
}

func (bt *busTest) purse(space, uid string) float64 {
	bt.s.bus.mu.Lock()
	defer bt.s.bus.mu.Unlock()
	v, _ := bt.s.bus.spaces[space].purseOf(uid)
	return v
}

func (bt *busTest) pot(space, id string) *pot {
	bt.s.bus.mu.Lock()
	defer bt.s.bus.mu.Unlock()
	sp := bt.s.bus.spaces[space]
	if sp == nil {
		return nil
	}
	return sp.potOf(id)
}

// lei: all the Lei of uids in the space, the pots' included (what's conserved when nothing is minted).
func (bt *busTest) lei(space string, uids ...string) float64 {
	t := 0.0
	for _, u := range uids {
		t += bt.purse(space, u)
	}
	bt.s.bus.mu.Lock()
	defer bt.s.bus.mu.Unlock()
	sp := bt.s.bus.spaces[space]
	for id := range sp.state["pots"] {
		if p := sp.potOf(id); p != nil {
			t += float64(p.V)
		}
	}
	return t
}

func TestPotEscrow(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	const sp = "pots"
	k := bt.dial(sp, "K")
	p1 := bt.dial(sp, "P1")
	x := bt.dial(sp, "X")
	if !hasOp(k.ops, "pot") || !hasOp(k.ops, "house") || !hasOp(k.ops, "lei") {
		t.Fatalf("ops: %v", k.ops)
	}
	all := []string{"K", "P1", "X"}
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "t1", "kind": "POKER"}))
	if p := bt.pot(sp, "t1"); p == nil || p.K != "K" || p.Kind != "POKER" || p.V != 0 || len(p.Pl) != 1 {
		t.Fatalf("opened: %+v", p)
	}
	start := bt.lei(sp, all...)

	// No allowance: nothing is taken (P1 isn't even at the table)
	e := mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"P1": -100.0}, "ref": "m1"}), "pot", "short")
	if e["ref"] != "m1" || e["short"].(map[string]any)["P1"] != 100.0 {
		t.Fatalf("short: %v", e)
	}
	if bt.purse(sp, "P1") != 1000 {
		t.Fatal("taken without an allowance")
	}
	// An allowance names the pot's keeper: another one's is refused (no squatting a table's id)
	mustRefuse(t, p1.do(map[string]any{"t": "pot", "op": "allow", "id": "t1", "keeper": "X", "n": 300.0}), "pot", "owner")
	mustRefuse(t, p1.do(map[string]any{"t": "pot", "op": "allow", "id": "nope", "keeper": "K", "n": 300.0}), "pot", "gone")
	mustRefuse(t, p1.do(map[string]any{"t": "pot", "op": "allow", "id": "t1", "keeper": "K", "n": 2.5}), "pot", "bad")
	mustTake(t, p1.do(map[string]any{"t": "pot", "op": "allow", "id": "t1", "keeper": "K", "n": 300.0}))
	if p := bt.pot(sp, "t1"); p.Al["P1"] != 300 || !p.member("P1") {
		t.Fatalf("allowed: %+v", p)
	}
	// Taken within the allowance; the keeper's own needs none
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"P1": -200.0, "K": -50.0}}))
	if p := bt.pot(sp, "t1"); p.V != 250 || p.In["P1"] != 200 || p.In["K"] != 50 || p.Al["P1"] != 100 {
		t.Fatalf("taken: %+v", p)
	}
	if bt.purse(sp, "P1") != 800 || bt.purse(sp, "K") != 950 {
		t.Fatal("purses", bt.purse(sp, "P1"), bt.purse(sp, "K"))
	}
	// Past the allowance: what's left of it, the rest short
	e = mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"P1": -200.0}}), "pot", "short")
	if e["short"].(map[string]any)["P1"] != 100.0 || bt.purse(sp, "P1") != 700 || bt.pot(sp, "t1").V != 350 {
		t.Fatalf("over the allowance: %v %v", e, bt.pot(sp, "t1"))
	}
	// Only the keeper moves
	mustRefuse(t, p1.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"P1": 350.0}}), "pot", "owner")
	mustRefuse(t, x.do(map[string]any{"t": "pot", "op": "close", "id": "t1"}), "pot", "owner")
	// A pay goes only to the table's members
	e = mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"X": 100.0}}), "pot", "short")
	if e["short"].(map[string]any)["X"] != 100.0 || bt.purse(sp, "X") != 1000 {
		t.Fatalf("paid a stranger: %v", e)
	}
	// Bounds: a whole number, at most 1,000,000, an id
	for _, d := range []map[string]any{{"P1": 1.5}, {"P1": 2e6}, {"P1": -2e6}, {"bad id!": 1.0}} {
		mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": d}), "pot", "bad")
	}
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": []any{1}}), "pot", "bad")
	if bt.lei(sp, all...) != start {
		t.Fatalf("Lei made or lost: %v, was %v", bt.lei(sp, all...), start)
	}
	// Pays never more than the pot holds, in the order listed: P1 first gets 300, K the 50 left
	e = mustRefuse(t, k.doRaw(`{"t":"pot","op":"move","id":"t1","d":{"P1":300,"K":300}}`), "pot", "short")
	if e["short"].(map[string]any)["K"] != 250.0 || bt.purse(sp, "P1") != 1000 || bt.purse(sp, "K") != 1000 || bt.pot(sp, "t1").V != 0 {
		t.Fatalf("paid past the pot: %v %v", e, bt.pot(sp, "t1"))
	}
	if bt.lei(sp, all...) != start {
		t.Fatal("Lei made or lost")
	}

	// Opening it again (a table started again) gives back what's in it
	mustTake(t, p1.do(map[string]any{"t": "pot", "op": "allow", "id": "t1", "keeper": "K", "n": 100.0, "add": true}))
	if bt.pot(sp, "t1").Al["P1"] != 100 {
		t.Fatal("add to the allowance", bt.pot(sp, "t1").Al)
	}
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"P1": -100.0, "K": -30.0}}))
	if bt.purse(sp, "P1") != 900 {
		t.Fatal("not taken")
	}
	// (nobody else's to open)
	mustRefuse(t, x.do(map[string]any{"t": "pot", "op": "open", "id": "t1", "kind": "POKER"}), "pot", "owner")
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "t1", "kind": "BARBUT"}))
	if p := bt.pot(sp, "t1"); p.V != 0 || len(p.In) != 0 || p.Kind != "BARBUT" || bt.purse(sp, "P1") != 1000 || bt.purse(sp, "K") != 1000 {
		t.Fatalf("not given back: %+v", p)
	}
	// Revoked: 0
	mustTake(t, p1.do(map[string]any{"t": "pot", "op": "allow", "id": "t1", "keeper": "K", "n": 0.0}))
	if _, has := bt.pot(sp, "t1").Al["P1"]; has {
		t.Fatal("not revoked")
	}
	// Closing gives back pro rata (P1 put in 3, K 1 of the 4 there) and forgets it, for everyone
	mustTake(t, p1.do(map[string]any{"t": "pot", "op": "allow", "id": "t1", "keeper": "K", "n": 300.0}))
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"P1": -300.0, "K": -100.0}}))
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"K": 101.0}}))
	// net in: P1 300, K -1: P1 gets all 299 back
	msgs := k.do(map[string]any{"t": "pot", "op": "close", "id": "t1"})
	mustTake(t, msgs)
	if bt.pot(sp, "t1") != nil || bt.purse(sp, "P1") != 999 || bt.purse(sp, "K") != 1001 {
		t.Fatalf("closed: %v %v %v", bt.pot(sp, "t1"), bt.purse(sp, "P1"), bt.purse(sp, "K"))
	}
	gotDel := false
	for _, m := range x.sync() {
		if m["t"] == "del" && m["kind"] == "pots" && m["id"] == "t1" {
			gotDel = true
		}
	}
	if !gotDel {
		t.Fatal("the others weren't told")
	}
	if bt.lei(sp, all...) != start {
		t.Fatal("Lei made or lost")
	}
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"K": 1.0}}), "pot", "gone")
	// Nobody writes a pot through put
	mustRefuse(t, k.do(map[string]any{"t": "put", "kind": "pots", "id": "t2", "v": map[string]any{"v": 100}}), "put", "server")
}

// The refund splits what's left by what each put in net, the odd Lei to the first member.
func TestPotRefundShares(t *testing.T) {
	sp := &busSpace{name: "x", clients: map[*busClient]bool{}, state: map[string]map[string]json.RawMessage{"lei": {}, "pots": {}}}
	p := &pot{K: "K", V: 100, In: map[string]int64{"A": 1, "B": 1, "C": 1, "K": -5}, Pl: []string{"K", "B", "A", "C"}}
	sp.potRefund(p)
	if a, b, c := num(decodeRecord(sp.state["lei"]["A"])["v"]), num(decodeRecord(sp.state["lei"]["B"])["v"]), num(decodeRecord(sp.state["lei"]["C"])["v"]); b != 1034 || a != 1033 || c != 1033 {
		t.Fatal(a, b, c)
	}
	if _, has := sp.state["lei"]["K"]; has || p.V != 0 || len(p.In) != 0 {
		t.Fatal(p)
	}
	// Nobody put anything in: it's just emptied
	p = &pot{K: "K", V: 50, In: map[string]int64{}}
	sp.potRefund(p)
	if p.V != 0 || len(sp.state["lei"]) != 3 {
		t.Fatal(p)
	}
}

func TestPotKeeperGone(t *testing.T) {
	old := potGoneGrace
	defer func() { potGoneGrace = old }()
	potGoneGrace = time.Hour
	bt := newBusTest(t, nil, nil)
	const sp = "gone"
	k := bt.dial(sp, "K")
	p1 := bt.dial(sp, "P1")
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "t1", "kind": "BLACKJACK"}))
	mustTake(t, p1.do(map[string]any{"t": "pot", "op": "allow", "id": "t1", "keeper": "K", "n": 500.0}))
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"P1": -400.0}}))
	waitGone := func(gone bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if p := bt.pot(sp, "t1"); p != nil && (p.Gone != 0) == gone {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("pot gone %v never came: %+v", gone, bt.pot(sp, "t1"))
	}
	// The keeper's last connection closes mid-hand: the pot is stamped gone, kept through the grace
	_ = k.ws.Close()
	waitGone(true)
	bt.s.bus.saveDirty()
	if p := bt.pot(sp, "t1"); p == nil || p.V != 400 || bt.purse(sp, "P1") != 600 {
		t.Fatalf("given back within the grace: %+v", p)
	}
	// Back in time: the pot stays
	k = bt.dial(sp, "K")
	waitGone(false)
	// Two connections: one closing isn't gone
	k2 := bt.dial(sp, "K")
	_ = k2.ws.Close()
	time.Sleep(50 * time.Millisecond)
	if bt.pot(sp, "t1").Gone != 0 {
		t.Fatal("gone with a connection left")
	}
	// Gone past the grace: the stakes come back, the pot is deleted
	_ = k.ws.Close()
	waitGone(true)
	potGoneGrace = 0
	time.Sleep(5 * time.Millisecond)
	bt.s.bus.saveDirty()
	if bt.pot(sp, "t1") != nil || bt.purse(sp, "P1") != 1000 {
		t.Fatalf("not given back: %+v %v", bt.pot(sp, "t1"), bt.purse(sp, "P1"))
	}
	got := false
	for _, m := range p1.sync() {
		if m["t"] == "del" && m["kind"] == "pots" {
			got = true
		}
	}
	if !got {
		t.Fatal("not told")
	}
}

// Pots are kept with the purses: a space unloaded and loaded again has them, its keeper away until it's back.
func TestPotPersist(t *testing.T) {
	old := potGoneGrace
	defer func() { potGoneGrace = old }()
	potGoneGrace = time.Hour
	bt := newBusTest(t, nil, nil)
	const sp = "keep"
	k := bt.dial(sp, "K")
	p1 := bt.dial(sp, "P1")
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "t1", "kind": "POKER"}))
	mustTake(t, p1.do(map[string]any{"t": "pot", "op": "allow", "id": "t1", "keeper": "K", "n": 500.0}))
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "t1", "d": map[string]any{"P1": -120.0, "K": -80.0}}))
	_ = k.ws.Close()
	_ = p1.ws.Close()
	for i := 0; ; i++ {
		bt.s.bus.mu.Lock()
		loaded := bt.s.bus.spaces[sp] != nil
		bt.s.bus.mu.Unlock()
		if !loaded {
			break
		}
		if i > 300 {
			t.Fatal("not unloaded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p1 = bt.dial(sp, "P1")
	st := p1.state["pots"]["t1"]
	if st == nil || st["v"] != 200.0 || st["gone"] == 0.0 || p1.state["lei"]["P1"]["v"] != 880.0 {
		t.Fatalf("after loading: %v %v", st, p1.state["lei"]["P1"])
	}
	if p := bt.pot(sp, "t1"); p.In["P1"] != 120 || p.In["K"] != 80 || p.Al["P1"] != 380 {
		t.Fatalf("%+v", p)
	}
	k = bt.dial(sp, "K")
	if bt.pot(sp, "t1").Gone != 0 {
		t.Fatal("the keeper is back")
	}
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "close", "id": "t1"}))
	if bt.purse(sp, "P1") != 1000 || bt.purse(sp, "K") != 1000 {
		t.Fatal("not given back")
	}
}

// Old pots go: given back after a day untouched, empty ones after an hour; the prizes' marks after three days.
func TestPotSweep(t *testing.T) {
	sp := &busSpace{name: "x", clients: map[*busClient]bool{}, state: map[string]map[string]json.RawMessage{"lei": {}, "pots": {}}}
	now := time.Now()
	put := func(id string, p *pot) { raw, _ := json.Marshal(p); sp.set("pots", id, raw) }
	put("full", &pot{K: "K", V: 10, In: map[string]int64{"A": 10}, Pl: []string{"K", "A"}, At: now.Add(-25 * time.Hour).UnixMilli()})
	put("young", &pot{K: "K", V: 10, In: map[string]int64{"A": 10}, Pl: []string{"K", "A"}, At: now.Add(-23 * time.Hour).UnixMilli()})
	put("empty", &pot{K: "K", At: now.Add(-61 * time.Minute).UnixMilli()})
	put("fresh", &pot{K: "K", At: now.Add(-59 * time.Minute).UnixMilli()})
	sp.set("pots", "~tour:old", json.RawMessage(fmt.Sprintf(`{"at":%d}`, now.Add(-73*time.Hour).UnixMilli())))
	sp.set("pots", "~tour:new", json.RawMessage(fmt.Sprintf(`{"at":%d}`, now.Add(-71*time.Hour).UnixMilli())))
	sp.sweepPots(now)
	for _, id := range []string{"full", "empty", "~tour:old"} {
		if _, has := sp.state["pots"][id]; has {
			t.Fatal(id, "kept")
		}
	}
	for _, id := range []string{"young", "fresh", "~tour:new"} {
		if _, has := sp.state["pots"][id]; !has {
			t.Fatal(id, "gone")
		}
	}
	if v, _ := sp.purseOf("A"); v != 1010 {
		t.Fatal("not given back", v)
	}
}

func TestPotPrize(t *testing.T) {
	saturday := time.Date(2026, 11, 28, 18, 0, 0, 0, time.UTC)
	oldNow := potNow
	defer func() { potNow = oldNow }()
	potNow = func() time.Time { return saturday }
	bt := newBusTest(t, nil, nil)
	const sp = "prize"
	k := bt.dial(sp, "K")
	others := []*busConn{bt.dial(sp, "A"), bt.dial(sp, "B"), bt.dial(sp, "C")}
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "tour1", "kind": "tour"}))
	// Too few at the table
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "prize", "id": "tour1", "what": "tour"}), "pot", "few")
	for _, c := range others {
		mustTake(t, c.do(map[string]any{"t": "pot", "op": "allow", "id": "tour1", "keeper": "K", "n": 0.0}))
	}
	mustRefuse(t, others[0].do(map[string]any{"t": "pot", "op": "prize", "id": "tour1", "what": "tour"}), "pot", "owner")
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "prize", "id": "tour1", "what": "gold"}), "pot", "bad")
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "prize", "id": "tour1", "what": "tour"}))
	if p := bt.pot(sp, "tour1"); p.V != 500 || !p.Pz {
		t.Fatalf("prize: %+v", p)
	}
	// Once a pot, once a keeper a day
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "prize", "id": "tour1", "what": "tour"}), "pot", "once")
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "tour2", "kind": "tour"}))
	for _, c := range others {
		mustTake(t, c.do(map[string]any{"t": "pot", "op": "allow", "id": "tour2", "keeper": "K", "n": 0.0}))
	}
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "prize", "id": "tour2", "what": "tour"}), "pot", "once")
	// The winner is paid from the pot; nobody else's Lei were made
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "tour1", "d": map[string]any{"B": 500.0}}))
	if bt.purse(sp, "B") != 1500 {
		t.Fatal(bt.purse(sp, "B"))
	}
	// The cup: the weekend day's mark must be the keeper's
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "prize", "id": "tour2", "what": "cup"}), "pot", "cup")
	mustTake(t, others[0].do(map[string]any{"t": "incr", "kind": "scores", "id": "_cup:2026-11-28", "set": map[string]any{"by": "A"}}))
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "prize", "id": "tour2", "what": "cup"}), "pot", "cup")
	mustTake(t, k.do(map[string]any{"t": "incr", "kind": "scores", "id": "_cup:2026-11-28", "set": map[string]any{"by": "K"}}))
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "prize", "id": "tour2", "what": "cup"}))
	if p := bt.pot(sp, "tour2"); p.V != 1000 {
		t.Fatalf("cup: %+v", p)
	}
	// One cup a day in the space, whoever keeps it
	mustTake(t, others[0].do(map[string]any{"t": "pot", "op": "open", "id": "tour3", "kind": "tour"}))
	for _, c := range []*busConn{k, others[1], others[2]} {
		mustTake(t, c.do(map[string]any{"t": "pot", "op": "allow", "id": "tour3", "keeper": "A", "n": 0.0}))
	}
	mustTake(t, others[0].do(map[string]any{"t": "incr", "kind": "scores", "id": "_cup:2026-11-28", "set": map[string]any{"by": "A"}}))
	mustRefuse(t, others[0].do(map[string]any{"t": "pot", "op": "prize", "id": "tour3", "what": "cup"}), "pot", "once")
	// Another keeper's own tournament prize is its own
	mustTake(t, others[0].do(map[string]any{"t": "pot", "op": "prize", "id": "tour3", "what": "tour"}))
	// On a weekday, no cup
	potNow = func() time.Time { return time.Date(2026, 11, 25, 18, 0, 0, 0, time.UTC) }
	mustTake(t, k.do(map[string]any{"t": "incr", "kind": "scores", "id": "_cup:2026-11-25", "set": map[string]any{"by": "K"}}))
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "tour4", "kind": "tour"}))
	for _, c := range others {
		mustTake(t, c.do(map[string]any{"t": "pot", "op": "allow", "id": "tour4", "keeper": "K", "n": 0.0}))
	}
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "prize", "id": "tour4", "what": "cup"}), "pot", "cup")
	// Closing a prize pot gives what's left to those who put Lei in (nobody here): emptied
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "close", "id": "tour2"}))
}

func TestPotLimits(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	const sp = "limits"
	k := bt.dial(sp, "K")
	for i := 0; i < potsPerKeeper; i++ {
		mustTake(t, k.do(map[string]any{"t": "pot", "op": "open", "id": fmt.Sprintf("p%d", i), "kind": "POKER"}))
	}
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "one-more", "kind": "POKER"}), "pot", "full")
	// Opening one's own again isn't another
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "p0", "kind": "POKER"}))
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "~tour:x", "kind": "POKER"}), "pot", "bad")
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "p1", "kind": "SLOTS", "players": []any{"K", "A"}}), "pot", "bad")
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "p1", "kind": "POKER", "players": []any{"K"}}), "pot", "bad")
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "p1", "kind": "ROULETTE", "players": []any{"K", "K"}}), "pot", "bad")
	// 64 at a table at most (the keeper is one)
	bt.s.bus.mu.Lock()
	p := bt.s.bus.spaces[sp].potOf("p1")
	for i := 0; i < potMembers-1; i++ {
		p.Pl = append(p.Pl, fmt.Sprintf("m%d", i))
	}
	bt.s.bus.spaces[sp].putPot("p1", p)
	bt.s.bus.mu.Unlock()
	k.sync()
	a := bt.dial(sp, "A")
	mustRefuse(t, a.do(map[string]any{"t": "pot", "op": "allow", "id": "p1", "keeper": "K", "n": 5.0}), "pot", "full")
	// A member already there may change theirs
	bt.s.bus.mu.Lock()
	p = bt.s.bus.spaces[sp].potOf("p1")
	p.Pl[5] = "A"
	bt.s.bus.spaces[sp].putPot("p1", p)
	bt.s.bus.mu.Unlock()
	mustTake(t, a.do(map[string]any{"t": "pot", "op": "allow", "id": "p1", "keeper": "K", "n": 5.0}))
	// 64 entries in a move at most
	d := map[string]any{}
	for i := 0; i < potMovesAtOnce+1; i++ {
		d[fmt.Sprintf("u%d", i)] = 1.0
	}
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "p1", "d": d}), "pot", "bad")
}
