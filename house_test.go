package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"strconv"
	"testing"
)

// queueRand hands out the numbers it's given (a replay feeds the reels, the wheel, the cards the Kotlin drew).
type queueRand struct {
	ints   []int
	floats []float64
	over   bool // more were drawn than given
}

func (q *queueRand) Intn(n int) int {
	if len(q.ints) == 0 {
		q.over = true
		return 0
	}
	v := q.ints[0]
	q.ints = q.ints[1:]
	return v
}

func (q *queueRand) Float64() float64 {
	if len(q.floats) == 0 {
		q.over = true
		return 0.5
	}
	v := q.floats[0]
	q.floats = q.floats[1:]
	return v
}

// seededRand: math/rand behind houseRand (the measured returns).
type seededRand struct{ r *rand.Rand }

func (s seededRand) Intn(n int) int   { return s.r.Intn(n) }
func (s seededRand) Float64() float64 { return s.r.Float64() }
func newSeeded(seed int64) seededRand { return seededRand{rand.New(rand.NewSource(seed))} }

// canon: v as JSON with its keys sorted and numbers as the bus has them.
func canon(v any) string {
	raw, _ := json.Marshal(v)
	var x any
	_ = json.Unmarshal(raw, &x)
	raw, _ = json.Marshal(x)
	return string(raw)
}

func seatJSON(m map[int]int) any {
	if m == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range m {
		out[strconv.Itoa(k)] = v
	}
	return out
}

func loadRecording(t *testing.T, path string) map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.NewDecoder(z).Decode(&rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// prefix: where symbol sym starts among weights (the draw that picks it).
func prefix(weights []int, sym int) int {
	return sumInts(weights[:sym])
}

func fruitsWeightsFor(reel int, scatter bool) []int {
	w := append([]int{}, fruitsWeights...)
	wild, sc := 0, 0
	if reel >= 1 && reel <= 3 {
		wild = fruitsWildWeight
	}
	if scatter {
		sc = fruitsScatterWeight
	}
	return append(w, wild, sc)
}

// replayRand: what the Kotlin drew for a step, read from its outcome.
func replayRand(kind string, m map[string]any, out map[string]any) *queueRand {
	q := &queueRand{}
	if out == nil {
		return q
	}
	st := out["state"].(map[string]any)
	switch kind {
	case "ROULETTE":
		q.ints = append(q.ints, mustInt(st["result"]))
	case "SLOTS":
		for _, r := range mustInts(st["reels"]) {
			q.ints = append(q.ints, prefix(slotsWeights, r))
		}
	case "FRUITS":
		switch m["op"] {
		case "spin":
			g := mustInts(st["grid"])
			for reel := 0; reel < fruitsReels; reel++ {
				col := g[reel*fruitsRows : reel*fruitsRows+fruitsRows]
				if col[0] == col[1] && col[1] == col[2] && col[0] != fruitsScatter {
					q.floats = append(q.floats, 0)
					q.ints = append(q.ints, prefix(fruitsWeightsFor(reel, false), col[0]))
				} else {
					q.floats = append(q.floats, 0.5)
					for _, x := range col {
						q.ints = append(q.ints, prefix(fruitsWeightsFor(reel, true), x))
					}
				}
			}
		case "gamble":
			q.ints = append(q.ints, mustInt(st["card"]))
		}
	}
	return q
}

// Every move of the apps' own rules, recorded (web/games/test/KotlinOracle.java), is taken or refused the same by the
// Go port, with the same state, secret, Lei and winners.
func TestHouseReplaysKotlin(t *testing.T) {
	games, moves, taken, skipped := 0, 0, 0, 0
	kinds := map[string]int{}
	for _, file := range []string{"web/games/test/kotlin-games.json.gz", "web/games/test/kotlin-fruits.json.gz"} {
		rec := loadRecording(t, file)
		for _, gx := range rec["games"].([]any) {
			g := gx.(map[string]any)
			kind := g["kind"].(string)
			if houseKinds[kind] == 0 {
				continue
			}
			games++
			kinds[kind]++
			n := mustInt(g["n"])
			purses := mustInts(g["purses"])
			purse := func(i int) int { return purses[i] }
			start := g["start"].(map[string]any)
			st, sec := houseStart(kind, n, cryptoRand{})
			if kind == "BLACKJACK" {
				// (the shoe is shuffled: only its size)
				want := start["state"].(map[string]any)
				if canon(st) != canon(want) || len(mustList(sec["shoe"])) != len(mustList(start["secret"].(map[string]any)["shoe"])) {
					t.Fatalf("%s start: %s vs %s", kind, canon(st), canon(want))
				}
			} else if canon(st) != canon(start["state"]) {
				t.Fatalf("%s start: %s vs %s", kind, canon(st), canon(start["state"]))
			}
			prevSt, _ := start["state"].(map[string]any)
			prevSec, _ := start["secret"].(map[string]any)
			for k, sx := range g["steps"].([]any) {
				moves++
				step := sx.(map[string]any)
				inSt, inSec := prevSt, prevSec
				out, _ := step["out"].(map[string]any)
				if out != nil {
					// Unpacked (pack.py): a shoe kept as how many cards are left of the one before
					if osec, ok := out["secret"].(map[string]any); ok {
						if keep, ok := osec["shoe"].(map[string]any); ok {
							osec["shoe"] = mustList(prevSec["shoe"])[:mustInt(keep["keep"])]
						}
					}
					prevSt = out["state"].(map[string]any)
					if osec, ok := out["secret"].(map[string]any); ok {
						prevSec = osec
					}
				}
				p := mustInt(step["p"])
				m := step["m"].(map[string]any)
				host := step["host"] == true
				where := fmt.Sprintf("%s game %d step %d seat %d %v", kind, games, k, p, m)
				// A new shoe (shuffled when it runs low) can't be told apart: skipped, the next steps go on from the Kotlin's
				if kind == "BLACKJACK" && out != nil && out["secret"] != nil && inSec != nil {
					a, b := mustInts(inSec["shoe"]), mustInts(out["secret"].(map[string]any)["shoe"])
					if len(b) > len(a) || !reflect.DeepEqual(a[:len(b)], b) {
						skipped++
						continue
					}
				}
				rnd := replayRand(kind, m, out)
				o := houseMove(kind, inSt, inSec, p, m, host, purse, rnd)
				if rnd.over {
					t.Fatalf("%s: drew more than the Kotlin did", where)
				}
				if out == nil {
					if o != nil {
						t.Fatalf("%s: taken here, refused by the Kotlin", where)
					}
					continue
				}
				if o == nil {
					t.Fatalf("%s: refused here, taken by the Kotlin", where)
				}
				taken++
				var overAny, playedAny any
				if o.over != nil {
					overAny = o.over
				}
				if o.played != nil {
					playedAny = o.played
				}
				var secAny any
				if o.secret != nil {
					secAny = o.secret
				}
				for _, f := range []struct {
					name      string
					got, want any
				}{
					{"state", o.state, out["state"]}, {"secret", secAny, out["secret"]}, {"pay", seatJSON(o.pay), out["pay"]},
					{"profit", seatJSON(o.profit), out["profit"]}, {"over", overAny, out["over"]}, {"played", playedAny, out["played"]},
				} {
					if canon(f.got) != canon(f.want) {
						t.Fatalf("%s: %s\n go     %.600s\n kotlin %.600s", where, f.name, canon(f.got), canon(f.want))
					}
				}
				// The money the server moves comes to the same as the Kotlin's pay
				sum := map[int]int{}
				for i, d := range o.stake {
					sum[i] += d
				}
				for i, d := range o.back {
					sum[i] += d
				}
				for i, d := range sum {
					if d != o.pay[i] {
						t.Fatalf("%s: stakes and wins %v %v, pay %v", where, o.stake, o.back, o.pay)
					}
				}
				if out["final"] == true {
					t.Fatalf("%s: a house game never ends", where)
				}
			}
		}
	}
	for _, k := range []string{"SLOTS", "FRUITS", "ROULETTE", "BLACKJACK"} {
		if kinds[k] == 0 {
			t.Fatalf("no %s in the recordings", k)
		}
	}
	if taken < 1000 {
		t.Fatalf("only %d moves taken", taken)
	}
	t.Logf("%d games, %d moves (%d taken, %d reshuffles skipped)", games, moves, taken, skipped)
}

// Over many spins the machines keep some of the stakes.
func TestHouseReturnUnderOne(t *testing.T) {
	rich := func(int) int { return 1 << 30 }
	for _, c := range []struct {
		kind  string
		stake int
		lo    float64
	}{{"SLOTS", 10, 0.5}, {"FRUITS", 1, 0.85}} {
		st, _ := houseStart(c.kind, 1, cryptoRand{})
		rnd := newSeeded(7)
		staked, back := 0, 0
		for i := 0; i < 300_000; i++ {
			o := houseMove(c.kind, st, nil, 0, map[string]any{"op": "spin", "v": float64(c.stake)}, true, rich, rnd)
			if o == nil {
				t.Fatal(c.kind, "refused")
			}
			staked -= o.stake[0]
			back += o.back[0]
		}
		rtp := float64(back) / float64(staked)
		t.Logf("%s returns %.4f", c.kind, rtp)
		if rtp >= 1 || rtp < c.lo {
			t.Fatalf("%s returns %.4f of the stakes", c.kind, rtp)
		}
	}
	// Slots worked out exactly: three alike and two alike over all reels
	tot := float64(sumInts(slotsWeights))
	exp := 0.0
	for a := range slotsWeights {
		for b := range slotsWeights {
			for c := range slotsWeights {
				p := float64(slotsWeights[a]*slotsWeights[b]*slotsWeights[c]) / (tot * tot * tot)
				exp += p * float64(slotsPayout([]int{a, b, c}, 1))
			}
		}
	}
	if exp >= 1 {
		t.Fatalf("slots return %.4f", exp)
	}
}

// fixedRand: Intn answers by f; Float64 always fl.
type fixedRand struct {
	f  func(n int) int
	fl float64
}

func (r fixedRand) Intn(n int) int   { return r.f(n) }
func (r fixedRand) Float64() float64 { return r.fl }

func withHouseRand(t *testing.T, r houseRand) {
	old := houseRNG
	houseRNG = func() houseRand { return r }
	t.Cleanup(func() { houseRNG = old })
}

// housed: the server's answer to the keeper among msgs (or its refusal).
func housed(t *testing.T, msgs []map[string]any) map[string]any {
	t.Helper()
	for _, m := range msgs {
		if m["t"] == "housed" {
			return m
		}
		if m["t"] == "error" {
			t.Fatalf("refused: %v", m)
		}
	}
	t.Fatalf("no answer: %v", msgs)
	return nil
}

func house(c *busConn, id string, seat int, m map[string]any) []map[string]any {
	return c.do(map[string]any{"t": "house", "id": id, "seat": seat, "host": seat == 0, "m": m, "ref": "h"})
}

func TestHouseRoulette(t *testing.T) {
	// The ball lands on 1: red, odd, low
	withHouseRand(t, fixedRand{f: func(n int) int { return 1 }})
	bt := newBusTest(t, nil, nil)
	const sp = "casino"
	k, p, q := bt.dial(sp, "K"), bt.dial(sp, "P"), bt.dial(sp, "Q")
	r := housed(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "r1", "kind": "ROULETTE", "players": []any{"K", "P", "Q"}, "ref": "o"}))
	if st := r["state"].(map[string]any); st["phase"] != "bet" || len(st["bets"].([]any)) != 3 || r["ref"] != "o" {
		t.Fatal(r)
	}
	red := map[string]any{"op": "bet", "type": "red", "value": 0.0, "v": 40.0}
	// A bet needs the player's allowance, all of it
	mustRefuse(t, house(k, "r1", 1, red), "house", "allow")
	mustTake(t, p.do(map[string]any{"t": "pot", "op": "allow", "id": "r1", "keeper": "K", "n": 30.0}))
	mustRefuse(t, house(k, "r1", 1, red), "house", "allow")
	mustTake(t, p.do(map[string]any{"t": "pot", "op": "allow", "id": "r1", "keeper": "K", "n": 100.0}))
	r = housed(t, house(k, "r1", 1, red))
	if bt.purse(sp, "P") != 960 || bt.pot(sp, "r1").V != 40 || r["pay"].(map[string]any)["1"] != -40.0 {
		t.Fatal(bt.purse(sp, "P"), bt.pot(sp, "r1"), r)
	}
	// More than the purse: funds; not a bet at all: move; only the keeper passes moves on
	mustTake(t, p.do(map[string]any{"t": "pot", "op": "allow", "id": "r1", "keeper": "K", "n": 1e6}))
	mustRefuse(t, house(k, "r1", 1, map[string]any{"op": "bet", "type": "red", "v": 5000.0}), "house", "funds")
	mustRefuse(t, house(k, "r1", 1, map[string]any{"op": "bet", "type": "blue", "v": 5.0}), "house", "move")
	mustRefuse(t, house(p, "r1", 1, red), "house", "owner")
	mustRefuse(t, house(k, "r1", 7, red), "house", "move")
	mustRefuse(t, house(k, "nope", 1, red), "house", "gone")
	// The keeper's own needs no allowance; Q bets nothing
	housed(t, house(k, "r1", 0, map[string]any{"op": "bet", "type": "black", "value": 0.0, "v": 50.0}))
	if bt.purse(sp, "K") != 950 {
		t.Fatal(bt.purse(sp, "K"))
	}
	// A keeper can't settle a pot the server deals
	mustRefuse(t, k.do(map[string]any{"t": "pot", "op": "move", "id": "r1", "d": map[string]any{"K": 90.0}}), "pot", "house")
	housed(t, house(k, "r1", 1, map[string]any{"op": "ready"}))
	housed(t, house(k, "r1", 2, map[string]any{"op": "auto"})) // Q out of time: ready for him
	r = housed(t, house(k, "r1", 0, map[string]any{"op": "ready"}))
	st := r["state"].(map[string]any)
	if st["phase"] != "spun" || st["result"] != 1.0 {
		t.Fatal(st)
	}
	if pr := r["profit"].(map[string]any); pr["1"] != 40.0 || pr["0"] != -50.0 || len(pr) != 2 || canon(r["over"]) != "[1]" {
		t.Fatal(r)
	}
	// Red paid 1:1 (the stake back from the pot, the win made); black's 50 stay with the house; no stake, no win
	if bt.purse(sp, "P") != 1040 || bt.purse(sp, "K") != 950 || bt.purse(sp, "Q") != 1000 {
		t.Fatal(bt.purse(sp, "P"), bt.purse(sp, "K"), bt.purse(sp, "Q"))
	}
	if pt := bt.pot(sp, "r1"); pt.V != 0 || len(pt.In) != 0 {
		t.Fatalf("%+v", pt)
	}
	// Chips on the table come back when the table starts again; and after a restart (the game gone) it must
	housed(t, house(k, "r1", 0, map[string]any{"op": "next"}))
	housed(t, house(k, "r1", 1, map[string]any{"op": "bet", "type": "n", "value": 7.0, "v": 15.0}))
	housed(t, house(k, "r1", 1, map[string]any{"op": "bet", "type": "n", "value": 8.0, "v": 5.0}))
	housed(t, house(k, "r1", 1, map[string]any{"op": "undo"})) // the 5 back off the table
	if bt.purse(sp, "P") != 1025 || bt.pot(sp, "r1").V != 15 {
		t.Fatal(bt.purse(sp, "P"), bt.pot(sp, "r1"))
	}
	bt.s.bus.mu.Lock()
	delete(bt.s.bus.spaces[sp].house, "r1")
	bt.s.bus.mu.Unlock()
	mustRefuse(t, house(k, "r1", 1, map[string]any{"op": "ready"}), "house", "gone")
	housed(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "r1", "kind": "ROULETTE", "players": []any{"K", "P", "Q"}}))
	if bt.purse(sp, "P") != 1040 || bt.pot(sp, "r1").V != 0 {
		t.Fatal(bt.purse(sp, "P"), bt.pot(sp, "r1"))
	}
	// Others see the pots, never a house game's answers
	for _, m := range q.sync() {
		if m["t"] == "housed" {
			t.Fatal("sent to the others", m)
		}
	}
}

func TestHouseSlots(t *testing.T) {
	withHouseRand(t, fixedRand{f: func(n int) int { return n - 1 }}) // diamonds
	bt := newBusTest(t, nil, nil)
	const sp = "slots"
	k := bt.dial(sp, "K")
	housed(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "s1", "kind": "SLOTS", "players": []any{"K"}}))
	r := housed(t, house(k, "s1", 0, map[string]any{"op": "spin", "v": 10.0}))
	if bt.purse(sp, "K") != 1000-10+5000 || canon(r["over"]) != "[0]" || r["state"].(map[string]any)["last"] != 4990.0 {
		t.Fatal(bt.purse(sp, "K"), r)
	}
	if pt := bt.pot(sp, "s1"); pt.V != 0 {
		t.Fatal(pt)
	}
	// No stake, no spin; nor past the most a spin stakes; nor a pull
	mustRefuse(t, house(k, "s1", 0, map[string]any{"op": "spin", "v": 0.0}), "house", "move")
	mustRefuse(t, house(k, "s1", 0, map[string]any{"op": "spin", "v": 3000.0}), "house", "move")
	mustRefuse(t, house(k, "s1", 0, map[string]any{"op": "pull"}), "house", "move")
	mustRefuse(t, house(k, "s1", 0, map[string]any{"op": "auto"}), "house", "move")
	// More than the purse
	bt.s.bus.mu.Lock()
	_, _ = bt.s.bus.spaces[sp].movePurse("K", -5980, "test")
	bt.s.bus.mu.Unlock()
	mustRefuse(t, house(k, "s1", 0, map[string]any{"op": "spin", "v": 20.0}), "house", "funds")
}

func TestHouseFruits(t *testing.T) {
	// Every reel stacked with 7s; the gamble's card red
	sevens := prefix(fruitsWeightsFor(0, false), 7)
	withHouseRand(t, fixedRand{fl: 0, f: func(n int) int {
		if n == 52 {
			return 0
		}
		return sevens
	}})
	bt := newBusTest(t, nil, nil)
	const sp = "fruits"
	k := bt.dial(sp, "K")
	housed(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "f1", "kind": "FRUITS", "players": []any{"K"}}))
	mustRefuse(t, house(k, "f1", 0, map[string]any{"op": "collect"}), "house", "move")
	mustRefuse(t, house(k, "f1", 0, map[string]any{"op": "spin", "v": 3.0}), "house", "move")
	r := housed(t, house(k, "f1", 0, map[string]any{"op": "spin", "v": 1.0}))
	// Five 7s on all ten lines: 5000 each
	if st := r["state"].(map[string]any); st["win"] != 50000.0 || st["g"] != 50000.0 || bt.purse(sp, "K") != 1000-10+50000 {
		t.Fatal(st, bt.purse(sp, "K"))
	}
	r = housed(t, house(k, "f1", 0, map[string]any{"op": "gamble", "red": true}))
	if st := r["state"].(map[string]any); st["g"] != 100000.0 || st["gl"] != 0.0 || bt.purse(sp, "K") != 100990 {
		t.Fatal(st, bt.purse(sp, "K"))
	}
	mustRefuse(t, house(k, "f1", 0, map[string]any{"op": "gamble", "red": true}), "house", "move")
	housed(t, house(k, "f1", 0, map[string]any{"op": "auto"})) // collects
	// A win is paid up to 1,000,000
	r = housed(t, house(k, "f1", 0, map[string]any{"op": "spin", "v": 100.0}))
	if st := r["state"].(map[string]any); st["win"] != 1e6 || bt.purse(sp, "K") != 100990-1000+1e6 {
		t.Fatal(st, bt.purse(sp, "K"))
	}
	if pt := bt.pot(sp, "f1"); pt.V != 0 {
		t.Fatal(pt)
	}
}

func TestHouseBlackjackHoleCard(t *testing.T) {
	// The shoe as made, unshuffled: the cards come K, Q, J, 10 from its end (player, dealer, player, dealer)
	withHouseRand(t, fixedRand{f: func(n int) int { return n - 1 }})
	bt := newBusTest(t, nil, nil)
	const sp = "bj"
	k, o := bt.dial(sp, "K"), bt.dial(sp, "O")
	r := housed(t, k.do(map[string]any{"t": "pot", "op": "open", "id": "b1", "kind": "BLACKJACK", "players": []any{"K"}}))
	if st := r["state"].(map[string]any); canon(st["shoe"]) != "[]" || st["left"] != 312.0 {
		t.Fatal(st)
	}
	r = housed(t, house(k, "b1", 0, map[string]any{"op": "bet", "v": 10.0}))
	st := r["state"].(map[string]any)
	if st["phase"] != "play" || canon(st["dealer"]) != "[50,-1]" || canon(st["hands"]) != "[[51,49]]" || canon(st["shoe"]) != "[]" {
		t.Fatal(st)
	}
	if bt.purse(sp, "K") != 990 {
		t.Fatal(bt.purse(sp, "K"))
	}
	// What the others get is the pot only
	for _, m := range o.sync() {
		if m["t"] != "put" || (m["kind"] != "pots" && m["kind"] != "lei") {
			t.Fatal("sent to the others", m)
		}
	}
	r = housed(t, house(k, "b1", 0, map[string]any{"op": "stand"}))
	st = r["state"].(map[string]any)
	if st["phase"] != "done" || canon(st["dealer"]) != "[50,48]" || canon(st["outcome"]) != `["Egalitate"]` {
		t.Fatal(st)
	}
	if bt.purse(sp, "K") != 1000 || bt.pot(sp, "b1").V != 0 {
		t.Fatal(bt.purse(sp, "K"), bt.pot(sp, "b1"))
	}
	housed(t, house(k, "b1", 0, map[string]any{"op": "next"}))
	if bt.purse(sp, "K") != 1000 {
		t.Fatal("next moved Lei")
	}
	// Closing gives back the bet on the table
	housed(t, house(k, "b1", 0, map[string]any{"op": "bet", "v": 100.0}))
	if bt.purse(sp, "K") != 900 {
		t.Fatal(bt.purse(sp, "K"))
	}
	mustTake(t, k.do(map[string]any{"t": "pot", "op": "close", "id": "b1"}))
	if bt.purse(sp, "K") != 1000 || bt.pot(sp, "b1") != nil {
		t.Fatal(bt.purse(sp, "K"))
	}
	mustRefuse(t, house(k, "b1", 0, map[string]any{"op": "next"}), "house", "gone")
}
