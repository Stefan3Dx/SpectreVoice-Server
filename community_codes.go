package main

// Finding a community by its code. There's no public list of communities (privacy): you get in with an invite, or
// with the community's code ("K7Q2-M9XD", shown to its members). With the code, an open community lets you in
// at once; a closed one takes a request, which its moderators accept or turn down. The code says only the name and
// the icon; a new one can be made at any time (the old one stops working).
//
//	<- {"t":"community-find","code":C}            -> {"t":"community-found","community":{id,name,icon,open,code}}
//	<- {"t":"community-join","code":C}            open: you're in; closed: -> {"t":"community-requested","community":{…}}
//	<- {"t":"community-requests"}                 (kick) -> {"t":"community-requests","items":[{uid,name,at}]}
//	<- {"t":"community-answer","uid":U,"accept":B} (kick) in, or turned down; the person is told
//	<- {"t":"community-new-code"}                 (manage_community) a new code for the current community
//	-> {"t":"community-request","comm":ID}        to the moderators online: someone asks to come in

import (
	"crypto/rand"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	codeAlphabet   = "23456789ABCDEFGHJKMNPQRSTUVWXYZ" // no 0/O, 1/I/L
	codeLen        = 8
	maxRequests    = 200 // waiting per community
	findsPerMinute = 10
)

func newCommunityCode() string {
	b := make([]byte, codeLen)
	_, _ = rand.Read(b)
	out := make([]byte, 0, codeLen+1)
	for i, x := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, codeAlphabet[int(x)%len(codeAlphabet)])
	}
	return string(out)
}

// normCode: what a person typed, as stored ("k7q2m9xd" → "K7Q2-M9XD").
func normCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(codeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	c := b.String()
	if len(c) != codeLen {
		return ""
	}
	return c[:4] + "-" + c[4:]
}

func (st *store) freeCode() string {
	for {
		c := newCommunityCode()
		var n int
		_ = st.db.QueryRow(`SELECT COUNT(*) FROM communities WHERE code=?`, c).Scan(&n)
		if n == 0 {
			return c
		}
	}
}

// migrateCodes: the code column, a code for every community, and the requests' table.
func (st *store) migrateCodes() error {
	if !st.hasColumn("communities", "code") {
		if _, err := st.db.Exec(`ALTER TABLE communities ADD COLUMN code TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := st.db.Exec(`CREATE TABLE IF NOT EXISTS comm_requests(comm INTEGER NOT NULL, uid TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', at INTEGER NOT NULL, PRIMARY KEY(comm, uid))`); err != nil {
		return err
	}
	rows, err := st.db.Query(`SELECT id FROM communities WHERE code=''`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if _, err := st.db.Exec(`UPDATE communities SET code=? WHERE id=?`, st.freeCode(), id); err != nil {
			return err
		}
	}
	return nil
}

func (st *store) communityByCode(code string) (*community, error) {
	code = normCode(code)
	if code == "" {
		return nil, errors.New("bad code")
	}
	return scanCommunity(st.db.QueryRow(`SELECT `+commCols+` FROM communities WHERE code=?`, code))
}

func (st *store) addRequest(comm int64, uid, name string) error {
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM comm_requests WHERE comm=?`, comm).Scan(&n)
	if n >= maxRequests {
		return errors.New("this community has too many requests waiting")
	}
	_, err := st.db.Exec(`INSERT OR REPLACE INTO comm_requests(comm,uid,name,at) VALUES(?,?,?,?)`, comm, uid, name, time.Now().UnixMilli())
	return err
}

type joinRequest struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
	At   int64  `json:"at"`
}

func (st *store) requests(comm int64) []joinRequest {
	out := []joinRequest{}
	rows, err := st.db.Query(`SELECT uid,name,at FROM comm_requests WHERE comm=? ORDER BY at`, comm)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r joinRequest
		if rows.Scan(&r.UID, &r.Name, &r.At) == nil {
			out = append(out, r)
		}
	}
	return out
}

func (st *store) dropRequest(comm int64, uid string) bool {
	res, err := st.db.Exec(`DELETE FROM comm_requests WHERE comm=? AND uid=?`, comm, uid)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// finds: how often each person looked a code up (guessing codes is slowed down).
var finds = struct {
	sync.Mutex
	m map[string][]time.Time
}{m: map[string][]time.Time{}}

func allowFind(uid string) bool {
	finds.Lock()
	defer finds.Unlock()
	now := time.Now()
	kept := finds.m[uid][:0]
	for _, t := range finds.m[uid] {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= findsPerMinute {
		finds.m[uid] = kept
		return false
	}
	finds.m[uid] = append(kept, now)
	if len(finds.m) > 50000 {
		finds.m = map[string][]time.Time{}
	}
	return true
}

// publicView: what a code shows about a community.
func publicView(c *community) map[string]any {
	return map[string]any{"id": c.ID, "name": c.Name, "icon": c.Icon, "open": c.Open, "code": c.Code}
}

func (h *hub) codeOp(ss *session, t string, m map[string]any) bool {
	st := h.s.store
	switch t {
	case "community-find":
		if !allowFind(ss.uid) {
			ss.refuse(t, "slow", "too many tries; wait a minute")
			return true
		}
		c, err := st.communityByCode(str(m, "code"))
		if err != nil {
			ss.refuse(t, "unknown", "no community has that code")
			return true
		}
		ss.send(map[string]any{"t": "community-found", "community": publicView(c), "member": st.isMember(c.ID, ss.uid)})
	case "community-requests":
		h.mu.Lock()
		comm, ok := ss.comm, ss.comm != 0 && h.may(ss, pKick)
		h.mu.Unlock()
		if !ok {
			ss.refuse(t, "role", "not allowed")
			return true
		}
		ss.send(map[string]any{"t": "community-requests", "comm": comm, "items": st.requests(comm)})
	case "community-answer":
		h.mu.Lock()
		comm, ok := ss.comm, ss.comm != 0 && h.may(ss, pKick)
		h.mu.Unlock()
		uid := str(m, "uid")
		if !ok {
			ss.refuse(t, "role", "not allowed")
			return true
		}
		if !st.dropRequest(comm, uid) {
			ss.refuse(t, "gone", "that request is no longer there")
			return true
		}
		accept, _ := boolOf(m, "accept")
		c, _ := st.community(comm)
		if accept && !h.s.mods.banned(uid, comm) {
			if err := st.join(comm, uid); err != nil {
				ss.refuse(t, "server", err.Error())
				return true
			}
			log.Printf("community %d: %s let in by %s", comm, uid, ss.uid)
		}
		for _, o := range h.sessionsOf(uid) {
			o.send(map[string]any{"t": "community-answered", "accepted": accept, "community": publicView(c), "communities": h.myCommunities(uid)})
		}
		ss.send(map[string]any{"t": "community-requests", "comm": comm, "items": st.requests(comm)})
	case "community-new-code":
		h.mu.Lock()
		ok := ss.comm != 0 && h.may(ss, pManageCommunity)
		comm := ss.comm
		h.mu.Unlock()
		if !ok {
			ss.refuse(t, "role", "not allowed")
			return true
		}
		if _, err := st.db.Exec(`UPDATE communities SET code=? WHERE id=?`, st.freeCode(), comm); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		c, _ := st.community(comm)
		h.mu.Lock()
		h.forget(comm)
		h.toComm(comm, map[string]any{"t": "community-info", "community": c})
		h.mu.Unlock()
	default:
		return false
	}
	return true
}

// joinByCode: community-join with a code (communities_hub.go).
func (h *hub) joinByCode(ss *session, code, name string) (comm int64, requested bool, err error) {
	st := h.s.store
	if !allowFind(ss.uid) {
		return 0, false, errors.New("too many tries; wait a minute")
	}
	c, e := st.communityByCode(code)
	if e != nil {
		return 0, false, errors.New("no community has that code")
	}
	if h.s.mods.banned(ss.uid, c.ID) {
		return 0, false, errors.New("you are banned from that community")
	}
	if st.isMember(c.ID, ss.uid) || c.Open {
		return c.ID, false, nil
	}
	if err := st.addRequest(c.ID, ss.uid, name); err != nil {
		return 0, false, err
	}
	// The moderators online hear of it
	h.mu.Lock()
	for _, o := range h.sessions {
		if o.comm == c.ID && h.may(o, pKick) {
			o.send(map[string]any{"t": "community-request", "comm": c.ID, "uid": ss.uid, "name": name})
		}
	}
	h.mu.Unlock()
	ss.send(map[string]any{"t": "community-requested", "community": publicView(c)})
	return 0, true, nil
}
