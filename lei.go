package main

// Lei, the play money: one purse per person in a space (bus kind "lei", id = the uid, bus.go). Only the server
// changes a purse; apps never write one, and new apps never add to one. Old apps (before 0.10.1) still add, bounded,
// while SV_LEI_LEGACY is on (the default); with SV_LEI_LEGACY=0 that is refused too (why "old"). Lei move through:
//
//   - the owner's grants: the server's owner, and those the owner gave "give_lei" (being an administrator doesn't
//     include it), add Lei to anyone's purse or take some away, with a reason (below; on the hub's connection);
//   - the daily bonus and gifts between people (bus "lei", below), the shop ("buy", shop.go);
//   - the game tables' pots (pots.go) and the games the server deals (house.go).
//
//	<- {"t":"lei-give","uid","amount":±N,"reason"?}   -> {"t":"done","op":"lei-give","uid","amount","balance"}
//	                                                     the recipient's apps: {"t":"lei-gift","amount","by","byName","reason","balance"}
//	<- {"t":"lei-log"}   (give_lei, administrator)   -> {"t":"lei-log","items":[{at,by,byName,to,toName,amount,reason,balance}]}
//
//	bus <- {"t":"lei","op":"daily","day":"YYYY-MM-DD","name"}   the day's bonus (max(100, 100-v)), once a day: "day"
//	                                                            within a day of the server's (UTC) and after the last
//	                                                            one ("d" in the purse)
//	bus <- {"t":"lei","op":"send","to","n","ref"}               a gift: 1..100000 Lei of one's own to another member,
//	                                                            all or nothing, 10 a minute; the recipient's apps get
//	                                                            {"t":"lei-got","from","n"}
//	    -> {"t":"error","op":"lei","why":"bad|day|once|funds|member|rate|full",…}   with the "ref" it had
//
// Every grant is kept in the lei_log table (who, to whom, how much, why) and shown in the admin panel.
// SV_LEI_MAX (100000) is the most one grant moves; 30 grants per minute per giver.
//
// An app from before all this still adds to purses itself: refused, and its person is told once a run (on the hub,
// as an announcement) to update.

import (
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
	return sp.movePurse(uid, d, by)
}

// Gifts: the most one moves, and how many one person sends a minute.
const (
	leiSendMax   = 100_000
	leiDaily     = 100 // Lei.DAILY
	leiOldNotice = "Update Spectre Voice to play for Lei on this server."
)

var (
	leiSendRate    = 10
	leiSendLimiter = &eventRate{got: map[string][]time.Time{}}
)

// busLei: the daily bonus and gifts (see the top of the file).
func (b *bus) busLei(sp *busSpace, c *busClient, m map[string]any) {
	op, _ := m["op"].(string)
	refuse := func(why, msg string) { c.refuse(m, "lei", why, msg) }
	if !safeID.MatchString(c.uid) {
		refuse("bad", "no uid")
		return
	}
	switch op {
	case "daily":
		day, _ := m["day"].(string)
		d, err := time.Parse("2006-01-02", day)
		now := time.Now().UTC()
		today, _ := time.Parse("2006-01-02", now.Format("2006-01-02"))
		if err != nil || d.Sub(today) > 24*time.Hour || today.Sub(d) > 24*time.Hour {
			refuse("day", "not today")
			return
		}
		name := clip(strings.Join(strings.Fields(str(m, "name")), " "), 40)
		b.mu.Lock()
		defer b.mu.Unlock()
		v, rec := sp.purseOf(c.uid)
		if last, _ := rec["d"].(string); last >= day {
			refuse("once", "already given today")
			return
		}
		rec["v"] = v + max(leiDaily, leiRefill-v)
		rec["d"] = day
		if name != "" {
			rec["n"] = name
		}
		if err := sp.writePurse(c.uid, rec, c.uid); err != nil {
			refuse("full", err.Error())
		}
	case "send":
		to, _ := m["to"].(string)
		n, ok := wholeLei(m["n"], 1, leiSendMax)
		switch {
		case !ok:
			refuse("bad", "a whole number of Lei, 1 to 100000")
			return
		case to == c.uid || !safeID.MatchString(to) || (b.member != nil && !b.member(sp.name, to)):
			refuse("member", "not someone on this server")
			return
		case !leiSendLimiter.okN(c.uid, leiSendRate):
			refuse("rate", "slow down")
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if have, _ := sp.purseOf(c.uid); have < float64(n) {
			refuse("funds", "not that many Lei")
			return
		}
		if _, recipient := sp.purseOf(to); recipient["v"] == nil && len(sp.state["lei"]) >= busKinds["lei"] {
			refuse("full", errLeiFull.Error())
			return
		}
		if _, err := sp.movePurse(c.uid, -float64(n), c.uid); err != nil {
			refuse("full", err.Error())
			return
		}
		if _, err := sp.movePurse(to, float64(n), c.uid); err != nil {
			_, _ = sp.movePurse(c.uid, float64(n), c.uid) // not given: back
			refuse("full", err.Error())
			return
		}
		got := map[string]any{"t": "lei-got", "from": c.uid, "n": n}
		if ref, ok := m["ref"].(string); ok && len(ref) <= 64 {
			got["ref"] = ref
		}
		for o := range sp.clients {
			if o.uid == to {
				o.send(got)
			}
		}
		log.Printf("lei: %s sent %d Lei to %s", c.uid, n, to)
	default:
		refuse("bad", "unknown op")
	}
}

// oldApp: c's app added to a purse itself (an app from before the pots): told once a run to update.
func (b *bus) oldApp(uid string) {
	if b.notify == nil {
		return
	}
	b.mu.Lock()
	if b.notified == nil {
		b.notified = map[string]bool{}
	}
	told := b.notified[uid]
	b.notified[uid] = true
	b.mu.Unlock()
	if !told {
		b.notify(uid, leiOldNotice)
	}
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
