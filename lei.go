package main

// The owner gives Lei: the server's owner, and those the owner gave "give_lei" (no one else can give it, and being
// an administrator doesn't include it), add Lei to anyone's purse or take some away, with a reason. The purse is the
// bus's own (kind "lei", id = the uid, bus.go): the server changes it itself, so everyone's apps get the new balance
// as a put, as after a game. Apps' own "add" stays as it was (games, bounded per message); this is the only way to
// a purse that checks who asks.
//
//	<- {"t":"lei-give","uid","amount":±N,"reason"?}   -> {"t":"done","op":"lei-give","uid","amount","balance"}
//	                                                     the recipient's apps: {"t":"lei-gift","amount","by","byName","reason","balance"}
//	<- {"t":"lei-log"}   (give_lei, administrator)   -> {"t":"lei-log","items":[{at,by,byName,to,toName,amount,reason,balance}]}
//
// Every grant is kept in the lei_log table (who, to whom, how much, why) and shown in the admin panel.
// SV_LEI_MAX (100000) is the most one grant moves; 30 grants per minute per giver.

import (
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"
)

const leiSchema = `
CREATE TABLE IF NOT EXISTS lei_log(id INTEGER PRIMARY KEY, at INTEGER NOT NULL, by TEXT NOT NULL, to_uid TEXT NOT NULL, amount INTEGER NOT NULL, reason TEXT NOT NULL DEFAULT '', balance INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS lei_log_at ON lei_log(at);
`

var leiGiveRate = 30 // per giver per minute

var leiRate = &eventRate{got: map[string][]time.Time{}}

func (st *store) migrateLei() error {
	_, err := st.db.Exec(leiSchema)
	return err
}

type leiGrant struct {
	At      int64  `json:"at"`
	By      string `json:"by"`
	ByName  string `json:"byName,omitempty"`
	To      string `json:"to"`
	ToName  string `json:"toName,omitempty"`
	Amount  int64  `json:"amount"`
	Reason  string `json:"reason,omitempty"`
	Balance int64  `json:"balance"`
}

func (st *store) leiLog(limit int) []leiGrant {
	out := []leiGrant{}
	rows, err := st.db.Query(`SELECT l.at, l.by, COALESCE(a.name,''), l.to_uid, COALESCE(b.name,''), l.amount, l.reason, l.balance FROM lei_log l
 LEFT JOIN users a ON a.uid = l.by LEFT JOIN users b ON b.uid = l.to_uid ORDER BY l.id DESC LIMIT ?`, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var g leiGrant
		if rows.Scan(&g.At, &g.By, &g.ByName, &g.To, &g.ToName, &g.Amount, &g.Reason, &g.Balance) == nil {
			out = append(out, g)
		}
	}
	return out
}

var errLeiFull = errors.New("no room for another purse")

// grantLei moves uid's purse in space by d (never below 0), stamped by who gave it; the new balance.
func (b *bus) grantLei(space, uid string, d float64, by string) (float64, error) {
	b.fileMu.Lock()
	defer b.fileMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	sp, err := b.load(space)
	if err != nil {
		return 0, err
	}
	purses := sp.state["lei"]
	purse := decodeRecord(purses[uid])
	v, has := purse["v"].(float64)
	if !has {
		v = leiStart
	}
	v = max(0, v+d)
	purse["v"] = v
	purse["_by"], purse["_a"] = by, true
	raw, _ := json.Marshal(purse)
	if len(raw) > busMaxValue || (purses[uid] == nil && len(purses) >= busKinds["lei"]) || sp.size+len(raw)-len(purses[uid]) > busMaxSpace {
		return 0, errLeiFull
	}
	sp.set("lei", uid, raw)
	sp.dirty = true
	sp.broadcast(map[string]any{"t": "put", "kind": "lei", "id": uid, "v": json.RawMessage(raw), "from": by}, nil)
	return v, nil
}

func (s *server) leiOp(ss *session, t string, m map[string]any) bool {
	switch t {
	case "lei-give":
		s.leiGive(ss, m)
	case "lei-log":
		h := s.hub
		h.mu.Lock()
		ok := !ss.api && (h.may(ss, pGiveLei) || h.may(ss, pAdministrator))
		h.mu.Unlock()
		if !ok {
			ss.refuse(t, "role", "not allowed")
			return true
		}
		ss.send(map[string]any{"t": "lei-log", "items": s.store.leiLog(100)})
	default:
		return false
	}
	return true
}

func (s *server) leiGive(ss *session, m map[string]any) {
	const t = "lei-give"
	h := s.hub
	h.mu.Lock()
	ok := !ss.bot && h.may(ss, pGiveLei)
	h.mu.Unlock()
	if !ok {
		ss.refuse(t, "role", "only the server's owner, or whom the owner lets")
		return
	}
	uid := str(m, "uid")
	amount := i64(m, "amount")
	if f, isF := m["amount"].(float64); isF && f != float64(int64(f)) {
		amount = 0 // whole Lei only
	}
	reason := clip(strings.Join(strings.Fields(str(m, "reason")), " "), 200)
	switch {
	case !safeID.MatchString(uid) || (!s.store.isMember(spaceID, uid) && s.store.userName(uid) == ""):
		ss.refuse(t, "gone", "no such person on this server")
		return
	case amount == 0 || amount > int64(s.cfg.leiMax) || amount < -int64(s.cfg.leiMax):
		ss.refuse(t, "bad", "the amount is a whole number up to the server's limit")
		return
	case !leiRate.okN(ss.uid, leiGiveRate):
		ss.refuse(t, "rate", "slow down")
		return
	}
	if s.bus == nil {
		ss.refuse(t, "server", "no purses on this server")
		return
	}
	bal, err := s.bus.grantLei(mainSpace, uid, float64(amount), ss.uid)
	if err != nil {
		ss.refuse(t, "full", err.Error())
		return
	}
	now := time.Now().UnixMilli()
	if _, err := s.store.db.Exec(`INSERT INTO lei_log(at,by,to_uid,amount,reason,balance) VALUES(?,?,?,?,?,?)`, now, ss.uid, uid, amount, reason, int64(bal)); err != nil {
		log.Printf("lei: !!! grant not logged: %v", err)
	}
	log.Printf("lei: %s gave %+d Lei to %s (%q), balance %d", ss.uid, amount, uid, reason, int64(bal))
	for _, o := range h.sessionsOf(uid) {
		if !o.bot {
			o.send(map[string]any{"t": "lei-gift", "amount": amount, "by": ss.uid, "byName": ss.name, "reason": reason, "balance": int64(bal)})
		}
	}
	ss.send(map[string]any{"t": "done", "op": t, "uid": uid, "amount": amount, "balance": int64(bal)})
}

// okN: as ok, with its own number per minute.
func (r *eventRate) okN(uid string, n int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	kept := r.got[uid][:0]
	for _, t := range r.got[uid] {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= n {
		r.got[uid] = kept
		return false
	}
	r.got[uid] = append(kept, now)
	if len(r.got) > 10000 { // a flood of names: start over rather than grow
		r.got = map[string][]time.Time{uid: r.got[uid]}
	}
	return true
}
