package main

// The server's people, online or not (a Discord-like member list), read markers for private chats (so the PC and
// the phone agree on what's unread), and blocking.
//
//	<- {"t":"members","q":…,"offset":N,"limit":N,"offline":bool}
//	   -> {"t":"members","q","offline","offset","total","online","items":[{uid,name,role,roles,online,away,awayMsg,bot,seen?}]}
//	                                                   online people first, then the others, by name; "seen" (last
//	                                                   seen, ms) only when the server shows it (SV_MEMBERS_LAST_SEEN)
//	<- {"t":"dm-read","with":uid,"id":N}            -> the person's sessions: {"t":"dm-read","with","id"}
//	   "dms" items carry "read" (the last id read) and "unread" (messages from them after it)
//	<- {"t":"block","uid"} / {"t":"unblock","uid"}  -> the person's sessions: {"t":"blocks","items":[uid…]}
//	<- {"t":"blocks"}                               -> {"t":"blocks","items":[uid…]}
//
// Who's listed: everyone who has ever come in (memberships of the space) and is still here: not server-banned, not
// erased (wipe drops the membership), not someone the asker blocked. A block works both ways for private messages,
// pokes, whispers to the person and calls (hub.blocked, calls.go's callBlockedHook); the blocked person isn't told (their message is refused as "blocked").

import (
	"sort"
	"strings"
	"time"
)

const (
	membersPage     = 100
	membersPageMax  = 200
	peopleRate      = 5.0
	peopleBurst     = 20.0
	maxBlocks       = 1000
	dmsUnreadCapped = 999 // a count shown as 999+ past this
)

func (st *store) migratePeople() error {
	_, err := st.db.Exec(`CREATE TABLE IF NOT EXISTS dm_reads(uid TEXT NOT NULL, conv TEXT NOT NULL, last INTEGER NOT NULL, PRIMARY KEY(uid, conv));
CREATE TABLE IF NOT EXISTS blocks(uid TEXT NOT NULL, other TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY(uid, other));
CREATE INDEX IF NOT EXISTS blocks_other ON blocks(other);`)
	return err
}

// Calls (calls.go) stop where one blocked the other.
func init() {
	callBlockedHook = func(s *server, a, b string) bool { return s.store.hasBlocked(a, b) }
}

// hasBlocked: a blocked b.
func (st *store) hasBlocked(a, b string) bool {
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE uid=? AND other=?`, a, b).Scan(&n)
	return n > 0
}

// blockedBetween: one of a, b blocked the other.
func (st *store) blockedBetween(a, b string) bool {
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE (uid=? AND other=?) OR (uid=? AND other=?)`, a, b, b, a).Scan(&n)
	return n > 0
}

// blocked: a and b may not reach each other privately (messages, pokes, calls): one blocked the other. For any
// feature that puts two people in touch.
func (h *hub) blocked(a, b string) bool {
	return a != b && h.s.store.blockedBetween(a, b)
}

func (st *store) blocksOf(uid string) []string {
	rows, err := st.db.Query(`SELECT other FROM blocks WHERE uid=? ORDER BY at`, uid)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var o string
		if rows.Scan(&o) == nil {
			out = append(out, o)
		}
	}
	return out
}

// touchSeen: uid was here until now (their last session left).
func (st *store) touchSeen(uid string) {
	_, _ = st.db.Exec(`UPDATE users SET seen=? WHERE uid=?`, time.Now().UnixMilli(), uid)
}

// dmUnread fills in what uid has read of each private chat, and how many of the other's messages came after.
func (st *store) dmUnread(uid string, list []dmSummary) {
	for i := range list {
		conv := dmConv(uid, list[i].With)
		var last int64
		_ = st.db.QueryRow(`SELECT last FROM dm_reads WHERE uid=? AND conv=?`, uid, conv).Scan(&last)
		var n int
		_ = st.db.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM messages WHERE conv=? AND id>? AND from_uid<>? AND deleted=0 LIMIT ?)`, conv, last, uid, dmsUnreadCapped+1).Scan(&n)
		list[i].Read, list[i].Unread = last, n
		list[i].Blocked = st.blockedBetween(uid, list[i].With)
	}
}

type dirPerson struct {
	UID     string  `json:"uid"`
	Name    string  `json:"name"`
	Role    string  `json:"role,omitempty"`
	Roles   []int64 `json:"roles,omitempty"`
	Online  bool    `json:"online,omitempty"`
	Away    bool    `json:"away,omitempty"`
	AwayMsg string  `json:"awayMsg,omitempty"`
	Bot     bool    `json:"bot,omitempty"`
	Seen    int64   `json:"seen,omitempty"`
}

// directory: the space's people for asker, filtered by q (in the name), online first.
func (h *hub) directory(asker, q string) (all []dirPerson, online int, err error) {
	st := h.s.store
	rows, err := st.db.Query(`SELECT u.uid, u.name, u.role, u.seen FROM memberships m JOIN users u ON u.uid=m.uid WHERE m.comm=?`, spaceID)
	if err != nil {
		return nil, 0, err
	}
	var people []dirPerson
	for rows.Next() {
		var p dirPerson
		if rows.Scan(&p.UID, &p.Name, &p.Role, &p.Seen) == nil {
			people = append(people, p)
		}
	}
	rows.Close()
	hidden := map[string]bool{}
	for _, o := range st.blocksOf(asker) {
		hidden[o] = true
	}
	roles := map[string][]int64{}
	if rr, err := st.db.Query(`SELECT uid, role FROM member_roles WHERE comm=? ORDER BY role`, spaceID); err == nil {
		for rr.Next() {
			var u string
			var r int64
			if rr.Scan(&u, &r) == nil {
				roles[u] = append(roles[u], r)
			}
		}
		rr.Close()
	}
	bots := map[string]bool{}
	if br, err := st.db.Query(`SELECT uid FROM bots`); err == nil {
		for br.Next() {
			var u string
			if br.Scan(&u) == nil {
				bots[u] = true
			}
		}
		br.Close()
	}
	type live struct {
		away bool
		msg  string
	}
	on := map[string]live{}
	h.mu.Lock()
	for _, o := range h.sessions {
		if o.bot && !o.api {
			continue // the music bot isn't a member
		}
		l, seen := on[o.uid]
		// Away only when every session of theirs is away
		if !seen {
			l = live{away: o.away, msg: o.awayMsg}
		} else if !o.away {
			l = live{}
		}
		on[o.uid] = l
	}
	h.mu.Unlock()
	q = strings.ToLower(strings.TrimSpace(q))
	showSeen := h.s.cfg.membersSeen
	for _, p := range people {
		if hidden[p.UID] || h.s.mods.banned(p.UID, spaceID) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(p.Name), q) {
			continue
		}
		p.Roles, p.Bot = roles[p.UID], bots[p.UID]
		if l, ok := on[p.UID]; ok {
			p.Online, p.Away, p.AwayMsg, p.Seen = true, l.away, l.msg, 0
			online++
		} else if !showSeen {
			p.Seen = 0
		}
		all = append(all, p)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Online != b.Online {
			return a.Online
		}
		if la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name); la != lb {
			return la < lb
		}
		return a.UID < b.UID
	})
	return all, online, nil
}

// peopleOp: the member list, read markers, blocks; false when t isn't one of them.
func (h *hub) peopleOp(ss *session, t string, m map[string]any) bool {
	switch t {
	case "members", "dm-read", "block", "unblock", "blocks":
	default:
		return false
	}
	if ss.api {
		ss.refuse(t, "bot", "not for bots")
		return true
	}
	if !h.staff(ss.uid) && !ss.people.take(peopleRate, peopleBurst) {
		ss.refuse(t, "rate", "slow down")
		return true
	}
	st := h.s.store
	switch t {
	case "members":
		all, online, err := h.directory(ss.uid, clip(str(m, "q"), 40))
		if err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		// Only the offline ones (the apps list the online people from their sessions, live)
		offline := m["offline"] == true
		if offline {
			kept := all[:0]
			for _, p := range all {
				if !p.Online {
					kept = append(kept, p)
				}
			}
			all = kept
		}
		off, lim := int(i64(m, "offset")), int(i64(m, "limit"))
		if lim <= 0 {
			lim = membersPage
		}
		lim = min(lim, membersPageMax)
		off = max(0, min(off, len(all)))
		page := all[off:min(len(all), off+lim)]
		if page == nil {
			page = []dirPerson{}
		}
		ss.send(map[string]any{"t": "members", "q": str(m, "q"), "offline": offline, "offset": off, "total": len(all), "online": online, "items": page, "seenShown": h.s.cfg.membersSeen})
	case "dm-read":
		with := str(m, "with")
		id := i64(m, "id")
		if !uidRe.MatchString(with) || with == ss.uid || id <= 0 {
			ss.refuse(t, "bad", "bad conversation")
			return true
		}
		conv := dmConv(ss.uid, with)
		if _, err := st.db.Exec(`INSERT INTO dm_reads(uid,conv,last) VALUES(?,?,?) ON CONFLICT(uid,conv) DO UPDATE SET last=MAX(last, excluded.last)`, ss.uid, conv, id); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		var last int64
		_ = st.db.QueryRow(`SELECT last FROM dm_reads WHERE uid=? AND conv=?`, ss.uid, conv).Scan(&last)
		h.mu.Lock()
		for _, o := range h.sessions {
			if o.uid == ss.uid && o != ss {
				o.send(map[string]any{"t": "dm-read", "with": with, "id": last})
			}
		}
		h.mu.Unlock()
	case "block", "unblock":
		other := str(m, "uid")
		if !uidRe.MatchString(other) || other == ss.uid {
			ss.refuse(t, "bad", "bad person")
			return true
		}
		var err error
		if t == "block" {
			var n int
			_ = st.db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE uid=?`, ss.uid).Scan(&n)
			if n >= maxBlocks {
				ss.refuse(t, "full", "too many blocked")
				return true
			}
			_, err = st.db.Exec(`INSERT OR IGNORE INTO blocks(uid,other,at) VALUES(?,?,?)`, ss.uid, other, time.Now().UnixMilli())
		} else {
			_, err = st.db.Exec(`DELETE FROM blocks WHERE uid=? AND other=?`, ss.uid, other)
		}
		if err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		list := st.blocksOf(ss.uid)
		h.mu.Lock()
		for _, o := range h.sessions {
			if o.uid == ss.uid {
				o.send(map[string]any{"t": "blocks", "items": list})
			}
		}
		h.mu.Unlock()
	case "blocks":
		ss.send(map[string]any{"t": "blocks", "items": st.blocksOf(ss.uid)})
	}
	return true
}
