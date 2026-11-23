package main

// The house games (Slots, Fruits, Roulette, Blackjack) are dealt by the server: it holds the reels, the wheel and the
// shoe (housegames.go), takes the stakes into the table's pot (pots.go) and pays the wins itself. The table's keeper
// only passes the players' moves on and shows the state, so no app can deal itself a win or pay out what it likes.
//
//	<- {"t":"pot","op":"open","id","kind","players":[uid…]}   (keeper) the game starts: -> {"t":"housed","id","state",…}
//	<- {"t":"house","id","seat","host":bool,"m":{…},"ref"}   (keeper) seat's move m ({"op":"auto"}: the quietest move
//	                                                          there is, for a seat out of time)
//	-> {"t":"housed","id","ref","state","pay":{seat:n},"profit":{seat:n},"over":[…]|null,"played":[…]}   to the keeper
//	-> {"t":"error","op":"house","why":"move|funds|allow|gone|owner"}   not taken (gone: no game here, open it again)
//
// A stake goes from the purse into the pot (within the player's allowance, unless the player is the keeper); when the
// hand or spin is settled, the stake comes back from the pot with the win (minted), or the pot keeps it (burnt). The
// games live in memory only: after a restart "house" answers "gone", and the keeper opens the pot again (which gives
// the stakes on the table back).

import (
	"log"
	"math"
	"sort"
	"strconv"
	"time"
)

type houseTable struct {
	kind    string
	seats   []string // uid by seat
	st, sec map[string]any
	rnd     houseRand
}

// houseRNG: where a new table's randomness comes from (a var for the tests).
var houseRNG = func() houseRand { return cryptoRand{} }

// houseOpen starts kind's game for players at pot id (b.mu held).
func (b *bus) houseOpen(sp *busSpace, id, kind, keeper string, players []string) *houseTable {
	ht := &houseTable{kind: kind, seats: players, rnd: houseRNG()}
	ht.st, ht.sec = houseStart(kind, len(players), ht.rnd)
	if sp.house == nil {
		sp.house = map[string]*houseTable{}
	}
	sp.house[id] = ht
	return ht
}

func seatMap(m map[int]int) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[strconv.Itoa(k)] = v
	}
	return out
}

// reply: what the keeper is told after a move (o nil: the start).
func (ht *houseTable) reply(id string, ref any, o *houseOut) map[string]any {
	r := map[string]any{"t": "housed", "id": id, "kind": ht.kind, "state": ht.st, "pay": map[string]any{}, "profit": map[string]any{}, "over": nil, "played": []int{}}
	if s, ok := ref.(string); ok && len(s) <= 64 {
		r["ref"] = s
	}
	if o != nil {
		r["pay"] = seatMap(o.pay)
		if o.profit != nil {
			r["profit"] = seatMap(o.profit)
		} else {
			r["profit"] = seatMap(o.pay)
		}
		if o.over != nil {
			r["over"] = o.over
		}
		if o.played != nil {
			r["played"] = o.played
		}
	}
	return r
}

// houseOp: the keeper passes a seat's move on (see the top of the file).
func (b *bus) houseOp(sp *busSpace, c *busClient, m map[string]any) {
	id, _ := m["id"].(string)
	refuse := func(why, msg string) { c.refuse(m, "house", why, msg) }
	mv, ok := m["m"].(map[string]any)
	seat, isSeat := kInt(m["seat"])
	if !ok || !isSeat {
		refuse("move", "a seat and a move")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p := sp.potOf(id)
	if p == nil {
		refuse("gone", "no such table")
		return
	}
	if p.K != c.uid {
		refuse("owner", "not your table")
		return
	}
	ht := sp.house[id]
	if ht == nil {
		refuse("gone", "the game isn't here any more: open it again")
		return
	}
	if seat < 0 || seat >= len(ht.seats) {
		refuse("move", "no such seat")
		return
	}
	host, _ := m["host"].(bool)
	purse := func(i int) int {
		v, _ := sp.purseOf(ht.seats[i])
		return int(min(v, math.MaxInt32))
	}
	free := func(i int) int {
		v := purse(i)
		if ht.seats[i] != p.K {
			v = int(min(int64(v), p.Al[ht.seats[i]]))
		}
		return v
	}
	var o *houseOut
	if op, _ := mv["op"].(string); op == "auto" {
		for _, a := range houseAuto(ht.kind) {
			if o = houseMove(ht.kind, ht.st, ht.sec, seat, a, host, free, ht.rnd); o != nil {
				break
			}
		}
	} else {
		o = houseMove(ht.kind, ht.st, ht.sec, seat, mv, host, free, ht.rnd)
	}
	if o == nil {
		// Why not: the allowance, the purse, or the move itself (tried again on a copy, with more to stake)
		why, msg := "move", "not allowed"
		if houseMove(ht.kind, ht.st, ht.sec, seat, mv, host, purse, cryptoRand{}) != nil {
			why, msg = "allow", "more than the player allowed this table"
		} else if houseMove(ht.kind, ht.st, ht.sec, seat, mv, host, func(int) int { return math.MaxInt32 }, cryptoRand{}) != nil {
			why, msg = "funds", "not that many Lei"
		}
		refuse(why, msg)
		return
	}
	b.houseSettle(sp, p, ht, o)
	ht.st = o.state
	if o.secret != nil {
		ht.sec = o.secret
	}
	p.At = time.Now().UnixMilli()
	sp.putPot(id, p)
	c.send(ht.reply(id, m["ref"], o))
}

// houseSettle moves the chips o says: stakes into the pot (or back out), and at the settle the wins (the stake from
// the pot, the rest minted) and the losses (the pot keeps them; burnt). b.mu held.
func (b *bus) houseSettle(sp *busSpace, p *pot, ht *houseTable, o *houseOut) {
	seats := func(m map[int]int) []int {
		out := make([]int, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Ints(out)
		return out
	}
	for _, i := range seats(o.stake) {
		uid, d := ht.seats[i], int64(o.stake[i])
		switch {
		case d < 0:
			if got := sp.potTake(p, uid, -d); got != -d {
				log.Printf("bus: !!! house %s: took %d of %d from %s", ht.kind, got, -d, uid)
			}
		case d > 0:
			sp.potPay(p, uid, min(d, max(p.In[uid], 0)))
		}
	}
	if !o.settle {
		return
	}
	for _, i := range seats(o.back) {
		uid, back := ht.seats[i], int64(o.back[i])
		if back <= 0 {
			continue
		}
		fromPot := sp.potPay(p, uid, min(back, max(p.In[uid], 0)))
		if won := back - fromPot; won > 0 {
			if _, err := sp.movePurse(uid, float64(won), p.K); err != nil {
				log.Printf("bus: !!! house %s: %d Lei won by %s not paid: %v", ht.kind, won, uid, err)
			}
		}
	}
	// What's still on the table was lost to the house
	for _, n := range p.In {
		p.V -= max(n, 0)
	}
	p.V = max(p.V, 0)
	p.In = map[string]int64{}
}
