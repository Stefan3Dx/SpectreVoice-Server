package main

// Escrow pots: the Lei on a game table or a tournament are held by the server, not moved between purses by the
// table's keeper. A player's own app allows the keeper to take up to so much of their Lei into the table's pot; the
// keeper moves Lei from purses into the pot (within each one's allowance and purse) and from the pot to the table's
// members (within what the pot holds), so no app can mint Lei or take them from someone who didn't sit down. What's
// left in a pot when it's closed (or its keeper is gone) goes back to those who put it in.
//
//	<- {"t":"pot","op":"open","id","kind","players":[uid…]?}   keeper: a new pot; on its own existing one, what's in it
//	                                                            goes back first (a table started again). "players": a
//	                                                            house game the server deals (house.go)
//	<- {"t":"pot","op":"allow","id","keeper","n","add"?}        player: the keeper may take up to n of my Lei (add: n more;
//	                                                            0: no more); makes them a member of the pot
//	<- {"t":"pot","op":"move","id","d":{uid:±n},"ref"}          keeper: negative takes purse→pot (up to the allowance and
//	                                                            the purse; the keeper's own: the purse), then positive
//	                                                            pays pot→purse to members, in the order listed, up to what
//	                                                            the pot holds. -> error why "short" with "short":{uid:n}
//	                                                            when something couldn't be moved whole
//	<- {"t":"pot","op":"prize","id","what":"tour"|"cup"}        keeper: the tournament's added Lei, into the pot once
//	                                                            (tour: 500, 4 members, one a keeper a day; cup: 1000,
//	                                                            4 members, a weekend day's cup whose scores mark is the
//	                                                            keeper's, one a day)
//	<- {"t":"pot","op":"close","id"}                            keeper: what's left goes back, the pot is gone
//	-> puts of kind "pots": {"k":keeper,"kind","v":total,"in":{uid:net},"al":{uid:left},"pl":[members],"at","gone","pz"}
//
// A keeper whose last connection to the space closes leaves its pots "gone"; after potGoneGrace they're given back
// and deleted (a reconnect before that keeps them). Pots nobody touched for a day are given back, empty ones go after
// an hour. Records whose id starts with "~" are the server's marks (prizes given).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"
)

type pot struct {
	K    string           `json:"k"`
	Kind string           `json:"kind"`
	V    int64            `json:"v"`
	In   map[string]int64 `json:"in"`
	Al   map[string]int64 `json:"al"`
	Pl   []string         `json:"pl"`
	At   int64            `json:"at"`
	Gone int64            `json:"gone"`
	Pz   bool             `json:"pz"`
}

const (
	potsPerKeeper  = 48
	potMembers     = 64
	potEntryMax    = 1_000_000 // the most one entry of a move takes or pays
	potAllowMax    = 1_000_000_000_000
	potIdleFull    = 24 * time.Hour
	potIdleEmpty   = time.Hour
	potMarkKeep    = 72 * time.Hour
	potPrizeTour   = 500 // Tournaments.BONUS
	potPrizeCup    = 1000
	potPrizeMinPl  = 4 // Tournaments.MIN_FOR_PRIZE
	potMarkPrefix  = "~"
	potMovesAtOnce = 64
)

// potGoneGrace: how long a keeper may be away before its pots are given back (a var for the tests).
var potGoneGrace = 120 * time.Second

// potNow: the clock the prizes' days are read by (a var for the tests).
var potNow = time.Now

func (p *pot) member(uid string) bool { return slices.Contains(p.Pl, uid) }

// ====== Purses (kind "lei", id = uid); b.mu held for all of these ======

// purseOf: uid's Lei (a purse nobody wrote yet holds leiStart) and its record.
func (sp *busSpace) purseOf(uid string) (float64, map[string]any) {
	rec := decodeRecord(sp.state["lei"][uid])
	v, has := rec["v"].(float64)
	if !has {
		v = leiStart
	}
	return max(0, v), rec
}

// writePurse keeps rec as uid's purse, stamped by who changed it, and tells everyone.
func (sp *busSpace) writePurse(uid string, rec map[string]any, by string) error {
	if sp.state["lei"] == nil {
		sp.state["lei"] = map[string]json.RawMessage{}
	}
	purses := sp.state["lei"]
	rec["_by"], rec["_a"] = by, true
	raw, _ := json.Marshal(rec)
	if len(raw) > busMaxValue || (purses[uid] == nil && len(purses) >= busKinds["lei"]) || sp.size+len(raw)-len(purses[uid]) > busMaxSpace {
		return errLeiFull
	}
	sp.set("lei", uid, raw)
	sp.dirty = true
	sp.broadcast(map[string]any{"t": "put", "kind": "lei", "id": uid, "v": json.RawMessage(raw), "from": by}, nil)
	return nil
}

// movePurse adds d to uid's purse (never below 0); the new balance.
func (sp *busSpace) movePurse(uid string, d float64, by string) (float64, error) {
	if !safeID.MatchString(uid) {
		return 0, errors.New("bad uid")
	}
	v, rec := sp.purseOf(uid)
	v = max(0, v+d)
	rec["v"] = v
	if err := sp.writePurse(uid, rec, by); err != nil {
		return 0, err
	}
	return v, nil
}

// ====== Pots ======

// potOf: the pot id (nil: none, or a mark).
func (sp *busSpace) potOf(id string) *pot {
	if strings.HasPrefix(id, potMarkPrefix) {
		return nil
	}
	raw := sp.state["pots"][id]
	if raw == nil {
		return nil
	}
	var p pot
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	if p.In == nil {
		p.In = map[string]int64{}
	}
	if p.Al == nil {
		p.Al = map[string]int64{}
	}
	if p.Pl == nil {
		p.Pl = []string{}
	}
	return &p
}

// putPot keeps p and tells everyone.
func (sp *busSpace) putPot(id string, p *pot) {
	if sp.state["pots"] == nil {
		sp.state["pots"] = map[string]json.RawMessage{}
	}
	raw, _ := json.Marshal(p)
	sp.set("pots", id, raw)
	sp.dirty = true
	sp.broadcast(map[string]any{"t": "put", "kind": "pots", "id": id, "v": json.RawMessage(raw), "from": p.K}, nil)
}

// delPot forgets the pot (and its house game) and tells everyone.
func (sp *busSpace) delPot(id string) {
	if _, ok := sp.state["pots"][id]; ok {
		sp.del("pots", id)
		sp.dirty = true
		sp.broadcast(map[string]any{"t": "del", "kind": "pots", "id": id}, nil)
	}
	delete(sp.house, id)
}

// potTake moves up to n of uid's Lei into the pot (within the allowance, unless uid is the keeper); what it took.
func (sp *busSpace) potTake(p *pot, uid string, n int64) int64 {
	have, _ := sp.purseOf(uid)
	amt := min(n, int64(have))
	if uid != p.K {
		amt = min(amt, p.Al[uid])
	}
	if amt <= 0 {
		return 0
	}
	if _, err := sp.movePurse(uid, -float64(amt), p.K); err != nil {
		return 0
	}
	p.V += amt
	p.In[uid] += amt
	if uid != p.K {
		if p.Al[uid] -= amt; p.Al[uid] <= 0 {
			delete(p.Al, uid)
		}
	}
	return amt
}

// potPay moves up to n from the pot to uid's purse; what it paid.
func (sp *busSpace) potPay(p *pot, uid string, n int64) int64 {
	amt := min(n, p.V)
	if amt <= 0 {
		return 0
	}
	if _, err := sp.movePurse(uid, float64(amt), p.K); err != nil {
		return 0
	}
	p.V -= amt
	if p.In[uid] -= amt; p.In[uid] == 0 {
		delete(p.In, uid)
	}
	return amt
}

// potRefund gives what's in the pot back to those who put it in, by how much each put in net (the first, by the
// members' order, gets the odd Lei); a pot nobody put anything in is just emptied.
func (sp *busSpace) potRefund(p *pot) {
	if p.V <= 0 {
		p.V, p.In = 0, map[string]int64{}
		return
	}
	var who []string
	seen := map[string]bool{}
	for _, uid := range p.Pl {
		if p.In[uid] > 0 && !seen[uid] {
			who, seen[uid] = append(who, uid), true
		}
	}
	var rest []string
	for uid, n := range p.In {
		if n > 0 && !seen[uid] {
			rest = append(rest, uid)
		}
	}
	sort.Strings(rest)
	who = append(who, rest...)
	total := new(big.Int)
	for _, uid := range who {
		total.Add(total, big.NewInt(p.In[uid]))
	}
	if total.Sign() == 0 {
		log.Printf("bus: pot of %s: %d Lei nobody put in, gone", p.K, p.V)
		p.V, p.In = 0, map[string]int64{}
		return
	}
	shares := make([]int64, len(who))
	given := int64(0)
	for i, uid := range who {
		s := new(big.Int).Mul(big.NewInt(p.V), big.NewInt(p.In[uid]))
		shares[i] = s.Div(s, total).Int64()
		given += shares[i]
	}
	shares[0] += p.V - given
	for i, uid := range who {
		if shares[i] > 0 {
			if _, err := sp.movePurse(uid, float64(shares[i]), p.K); err != nil {
				log.Printf("bus: !!! %d Lei of a pot not given back to %s: %v", shares[i], uid, err)
			}
		}
	}
	p.V, p.In = 0, map[string]int64{}
}

// keeperSeen: uid is connected to the space: its pots aren't gone.
func (sp *busSpace) keeperSeen(uid string) {
	for id := range sp.state["pots"] {
		if p := sp.potOf(id); p != nil && p.K == uid && p.Gone != 0 {
			p.Gone = 0
			sp.putPot(id, p)
		}
	}
}

// keeperGone: one of uid's connections closed; when it was the last one, its pots are gone (given back after the
// grace, unless it comes back).
func (sp *busSpace) keeperGone(uid string) {
	for c := range sp.clients {
		if c.uid == uid {
			return
		}
	}
	now := time.Now().UnixMilli()
	for id := range sp.state["pots"] {
		if p := sp.potOf(id); p != nil && p.K == uid && p.Gone == 0 {
			p.Gone = now
			sp.putPot(id, p)
		}
	}
}

// sweepPots gives back and deletes the pots whose keeper is gone for good, or that nobody touched for long; and
// forgets old marks.
func (sp *busSpace) sweepPots(now time.Time) {
	ms := now.UnixMilli()
	for _, id := range sortedKeys(sp.state["pots"]) {
		if strings.HasPrefix(id, potMarkPrefix) {
			at, _ := decodeRecord(sp.state["pots"][id])["at"].(float64)
			if ms-int64(at) > potMarkKeep.Milliseconds() {
				sp.del("pots", id)
				sp.dirty = true
			}
			continue
		}
		p := sp.potOf(id)
		switch {
		case p == nil:
			sp.delPot(id)
		case p.Gone != 0 && ms-p.Gone > potGoneGrace.Milliseconds(), p.V > 0 && ms-p.At > potIdleFull.Milliseconds():
			sp.potRefund(p)
			sp.delPot(id)
		case p.V <= 0 && ms-p.At > potIdleEmpty.Milliseconds():
			sp.delPot(id)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// potEntry: one uid's ±n in a move, in the order the keeper listed them.
type potEntry struct {
	uid string
	n   float64
}

// potEntries reads the message's "d" object keeping its order.
func potEntries(raw []byte) ([]potEntry, error) {
	var top struct {
		D json.RawMessage `json:"d"`
	}
	if err := json.Unmarshal(raw, &top); err != nil || len(top.D) == 0 {
		return nil, errors.New("no d")
	}
	dec := json.NewDecoder(bytes.NewReader(top.D))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("d is not an object")
	}
	var out []potEntry
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := tok.(string)
		var n float64
		if err := dec.Decode(&n); err != nil {
			return nil, err
		}
		out = append(out, potEntry{k, n})
	}
	return out, nil
}

// wholeLei: n as a whole number of Lei within [lo, hi].
func wholeLei(x any, lo, hi float64) (int64, bool) {
	f, ok := x.(float64)
	if !ok || math.IsNaN(f) || f != math.Trunc(f) || f < lo || f > hi {
		return 0, false
	}
	return int64(f), true
}

// utcDay: the date of t in UTC, as the apps write days.
func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// potOp does a pot message (see the top of the file); raw is the message as it came (for the order of "d").
func (b *bus) potOp(sp *busSpace, c *busClient, m map[string]any, raw []byte) {
	op, _ := m["op"].(string)
	id, _ := m["id"].(string)
	refuse := func(why, msg string) { c.refuse(m, "pot", why, msg) }
	if !safeID.MatchString(id) || len(id) > 64 || strings.HasPrefix(id, potMarkPrefix) {
		refuse("bad", "bad pot id")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := potNow()
	ms := now.UnixMilli()
	p := sp.potOf(id)
	if op != "open" && p == nil {
		refuse("gone", "no such pot")
		return
	}
	keeperOnly := func() bool {
		if p.K != c.uid {
			refuse("owner", "not your pot")
			return false
		}
		return true
	}
	switch op {
	case "open":
		kind, _ := m["kind"].(string)
		if len(kind) > 24 || (kind != "" && !safeID.MatchString(kind)) {
			refuse("bad", "bad kind")
			return
		}
		var players []string
		if l, ok := m["players"].([]any); ok {
			for _, x := range l {
				uid, _ := x.(string)
				if !safeID.MatchString(uid) || slices.Contains(players, uid) {
					refuse("bad", "bad players")
					return
				}
				players = append(players, uid)
			}
		}
		house := len(players) > 0
		if house && (houseKinds[kind] == 0 || len(players) > houseKinds[kind]) {
			refuse("bad", "not a house game, or too many players")
			return
		}
		if p != nil {
			if !keeperOnly() {
				return
			}
			// Started again: what's in it goes back
			sp.potRefund(p)
			p.Kind, p.Gone, p.At = kind, 0, ms
		} else {
			mine := 0
			for oid := range sp.state["pots"] {
				if o := sp.potOf(oid); o != nil && o.K == c.uid {
					mine++
				}
			}
			if len(sp.state["pots"]) >= busKinds["pots"] || mine >= potsPerKeeper {
				refuse("full", "too many pots")
				return
			}
			p = &pot{K: c.uid, Kind: kind, In: map[string]int64{}, Al: map[string]int64{}, Pl: []string{c.uid}, At: ms}
		}
		delete(sp.house, id)
		sp.putPot(id, p)
		if house {
			ht := b.houseOpen(sp, id, kind, c.uid, players)
			c.send(ht.reply(id, m["ref"], nil))
		}
	case "allow":
		keeper, _ := m["keeper"].(string)
		if keeper != p.K {
			refuse("owner", "that pot is someone else's")
			return
		}
		n, ok := wholeLei(m["n"], 0, potAllowMax)
		if !ok {
			refuse("bad", "the allowance is a whole number of Lei")
			return
		}
		if !p.member(c.uid) {
			if len(p.Pl) >= potMembers {
				refuse("full", "too many at this table")
				return
			}
			p.Pl = append(p.Pl, c.uid)
		}
		if add, _ := m["add"].(bool); add {
			n = min(potAllowMax, p.Al[c.uid]+n)
		}
		if n > 0 {
			p.Al[c.uid] = n
		} else {
			delete(p.Al, c.uid)
		}
		p.At = ms
		sp.putPot(id, p)
	case "move":
		if !keeperOnly() {
			return
		}
		if sp.house[id] != nil {
			refuse("house", "the server deals this table")
			return
		}
		entries, err := potEntries(raw)
		if err != nil || len(entries) > potMovesAtOnce {
			refuse("bad", "d: {uid: ±Lei}")
			return
		}
		for _, e := range entries {
			if _, ok := wholeLei(e.n, -potEntryMax, potEntryMax); !ok || !safeID.MatchString(e.uid) {
				refuse("bad", fmt.Sprintf("a whole number of Lei, up to %d, for each", potEntryMax))
				return
			}
		}
		short := map[string]any{}
		addShort := func(uid string, n int64) {
			if n > 0 {
				short[uid] = num(short[uid]) + float64(n)
			}
		}
		for _, e := range entries {
			if n := int64(-e.n); n > 0 {
				addShort(e.uid, n-sp.potTake(p, e.uid, n))
			}
		}
		for _, e := range entries {
			if n := int64(e.n); n > 0 {
				if !p.member(e.uid) {
					addShort(e.uid, n)
					continue
				}
				addShort(e.uid, n-sp.potPay(p, e.uid, n))
			}
		}
		p.At = ms
		sp.putPot(id, p)
		if len(short) > 0 {
			e := map[string]any{"t": "error", "op": "pot", "why": "short", "message": "not all of it could be moved", "id": id, "short": short}
			if ref, ok := m["ref"].(string); ok && len(ref) <= 64 {
				e["ref"] = ref
			}
			c.send(e)
		}
	case "prize":
		if !keeperOnly() {
			return
		}
		what, _ := m["what"].(string)
		var mark string
		var amount int64
		switch {
		case p.Pz:
			refuse("once", "this pot already had its prize")
			return
		case sp.house[id] != nil:
			refuse("house", "the server deals this table")
			return
		case len(p.Pl) < potPrizeMinPl:
			refuse("few", fmt.Sprintf("a prize needs %d players", potPrizeMinPl))
			return
		case what == "tour":
			mark, amount = potMarkPrefix+"tour:"+utcDay(now)+":"+c.uid, potPrizeTour
		case what == "cup":
			// The weekend's cup of a day (the apps' local day: UTC's, or the one either side) the keeper marked
			for _, d := range []time.Time{now, now.AddDate(0, 0, -1), now.AddDate(0, 0, 1)} {
				day := utcDay(d)
				wd := d.UTC().Weekday()
				if wd != time.Saturday && wd != time.Sunday {
					continue
				}
				if by, _ := decodeRecord(sp.state["scores"]["_cup:"+day])["by"].(string); by == c.uid {
					mark, amount = potMarkPrefix+"cup:"+day, potPrizeCup
					break
				}
			}
			if mark == "" {
				refuse("cup", "no weekend cup of yours today")
				return
			}
		default:
			refuse("bad", "what: tour or cup")
			return
		}
		if sp.state["pots"][mark] != nil {
			refuse("once", "that prize was given already")
			return
		}
		markRaw, _ := json.Marshal(map[string]any{"at": ms, "k": c.uid, "pot": id})
		sp.set("pots", mark, markRaw)
		p.V += amount
		p.Pz = true
		p.At = ms
		sp.putPot(id, p)
		log.Printf("bus: %s prize of %d Lei into pot %s (%s)", what, amount, id, c.uid)
	case "close":
		if !keeperOnly() {
			return
		}
		sp.potRefund(p)
		sp.delPot(id)
	default:
		refuse("bad", "unknown pot op")
	}
}
