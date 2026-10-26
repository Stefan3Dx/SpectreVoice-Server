package main

// The Lei shop, done by the server: a purchase takes the price from the buyer's purse and gives the item in one
// step, so an app can't hand itself items without paying (or pay with Lei it doesn't have).
//
//	<- {"t":"buy","item":"c_red"}
//	-> two puts to everyone: lei/<uid> (the new balance) and profiles/<uid> (own.<item> = now ms, eq.<SLOT> = item)
//	-> {"t":"error","op":"buy","item":"c_red","why":"unknown|owned|lei|full|uid|auth","message":"…"} when not

import (
	"encoding/json"
	"time"
)

type shopItem struct {
	slot  string // the Shop.Slot name: COLOR, FRAME, BADGE, TITLE
	price float64
}

// Must match Shop.ITEMS in the app (Community.kt): id -> slot and price in Lei.
var shopItems = map[string]shopItem{
	"c_red":     {"COLOR", 1500},
	"c_gold":    {"COLOR", 2500},
	"c_mint":    {"COLOR", 1500},
	"c_sky":     {"COLOR", 1500},
	"c_pink":    {"COLOR", 1500},
	"c_violet":  {"COLOR", 1500},
	"f_gold":    {"FRAME", 3000},
	"f_neon":    {"FRAME", 3000},
	"f_fire":    {"FRAME", 4000},
	"f_toxic":   {"FRAME", 4000},
	"b_fire":    {"BADGE", 800},
	"b_goat":    {"BADGE", 2000},
	"b_skull":   {"BADGE", 800},
	"b_clover":  {"BADGE", 1200},
	"b_gamer":   {"BADGE", 800},
	"b_crown":   {"BADGE", 5000},
	"b_clown":   {"BADGE", 600},
	"t_smecher": {"TITLE", 2000},
	"t_boss":    {"TITLE", 3000},
	"t_legenda": {"TITLE", 5000},
	"t_tryhard": {"TITLE", 1500},
	"t_afk":     {"TITLE", 1000},
}

const (
	shopTitleDays = 7    // Shop.TITLE_DAYS: a title runs out and can be bought again
	leiStart      = 1000 // Lei.START: a purse nobody wrote yet
	leiRefill     = 100  // Lei.REFILL: an empty purse counts as this
)

func decodeRecord(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if raw != nil {
		_ = json.Unmarshal(raw, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// buy sells c the item, paid from its purse (see the top of the file).
func (b *bus) buy(sp *busSpace, c *busClient, itemID string) {
	refuse := func(why, msg string) {
		c.send(map[string]any{"t": "error", "op": "buy", "item": itemID, "why": why, "message": msg})
	}
	it, ok := shopItems[itemID]
	if !ok {
		refuse("unknown", "no such item")
		return
	}
	if !safeID.MatchString(c.uid) {
		refuse("uid", "no uid")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now().UnixMilli()
	profiles, purses := sp.state["profiles"], sp.state["lei"]
	prof := decodeRecord(profiles[c.uid])
	own, _ := prof["own"].(map[string]any)
	if at, has := own[itemID].(float64); has && (it.slot != "TITLE" || int64(at)+shopTitleDays*86_400_000 > now) {
		refuse("owned", "already owned")
		return
	}
	purse := decodeRecord(purses[c.uid])
	balance, has := purse["v"].(float64)
	if !has {
		balance = leiStart
	}
	if balance <= 0 {
		balance = leiRefill
	}
	if balance < it.price {
		refuse("lei", "not enough Lei")
		return
	}
	purse["v"] = balance - it.price
	if own == nil {
		own = map[string]any{}
	}
	own[itemID] = now
	prof["own"] = own
	eq, _ := prof["eq"].(map[string]any)
	if eq == nil {
		eq = map[string]any{}
	}
	eq[it.slot] = itemID
	prof["eq"] = eq
	stamp(purse, c)
	stamp(prof, c)
	purseRaw, _ := json.Marshal(purse)
	profRaw, _ := json.Marshal(prof)
	grow := len(purseRaw) - len(purses[c.uid]) + len(profRaw) - len(profiles[c.uid])
	if len(purseRaw) > busMaxValue || len(profRaw) > busMaxValue || sp.size+grow > busMaxSpace ||
		(purses[c.uid] == nil && len(purses) >= busKinds["lei"]) || (profiles[c.uid] == nil && len(profiles) >= busKinds["profiles"]) {
		refuse("full", "too much")
		return
	}
	sp.set("lei", c.uid, purseRaw)
	sp.set("profiles", c.uid, profRaw)
	sp.dirty = true
	sp.broadcast(map[string]any{"t": "put", "kind": "lei", "id": c.uid, "v": json.RawMessage(purseRaw), "from": c.uid}, nil)
	sp.broadcast(map[string]any{"t": "put", "kind": "profiles", "id": c.uid, "v": json.RawMessage(profRaw), "from": c.uid}, nil)
}
