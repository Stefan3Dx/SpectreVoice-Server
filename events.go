package main

// Scheduled events: those with "events" (moderators and admins by default) put an event on the server's calendar:
// a title, a description, when it starts (and optionally ends), optionally a channel. Everyone sees the list,
// says "interested" or "going", and is told N minutes before (SV_EVENT_LEAD, 15) and at the start: in the app
// while connected, by push otherwise (only where the person turned push on). Anyone may mute these notices.
// Times are milliseconds since 1970 (UTC); the apps show them in their own time zone.
//
//	<- {"t":"events"}  -> {"t":"events","items":[…],"muted":bool,"lead":N,"may":bool}
//	                       upcoming and happening now (cancelled ones until their start), and the last few past
//	<- {"t":"event-save","id"?,"title","about","start","end"?,"ch"?}   events; an edit by its maker or manage_community
//	<- {"t":"event-del","id"}                                         cancel: its maker or manage_community
//	<- {"t":"event-rsvp","id","kind":"going"|"interested"|""}
//	<- {"t":"event-mute","on":bool}                                   no notices about events for me
//	-> {"t":"event","e":{…}} to everyone when one is made, changed, cancelled or its counts change ("mine" only
//	   to the one it concerns)
//	-> {"t":"event-notice","kind":"soon"|"start","e":{…}}

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const eventSchema = `
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY, title TEXT NOT NULL, about TEXT NOT NULL DEFAULT '', start INTEGER NOT NULL, end_at INTEGER NOT NULL DEFAULT 0, ch INTEGER NOT NULL DEFAULT 0, by TEXT NOT NULL, created INTEGER NOT NULL, cancelled INTEGER NOT NULL DEFAULT 0, told INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS events_start ON events(start);
CREATE TABLE IF NOT EXISTS event_rsvp(event INTEGER NOT NULL, uid TEXT NOT NULL, kind TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY(event, uid));
CREATE TABLE IF NOT EXISTS event_mute(uid TEXT PRIMARY KEY, at INTEGER NOT NULL);
`

// Vars so tests can change them.
var (
	eventTick      = 15 * time.Second
	eventMaxAhead  = 366 * 24 * time.Hour
	eventMaxLong   = 7 * 24 * time.Hour
	eventUpcoming  = 200 // upcoming events at once
	eventPastShown = 5
	eventSaveRate  = 10 // saves per person per minute
)

// told bits
const (
	toldSoon  = 1
	toldStart = 2
)

type event struct {
	ID         int64   `json:"id"`
	Title      string  `json:"title"`
	About      string  `json:"about,omitempty"`
	Start      int64   `json:"start"`
	End        int64   `json:"end,omitempty"`
	Ch         int64   `json:"ch,omitempty"`
	By         string  `json:"by"`
	ByName     string  `json:"byName,omitempty"`
	Created    int64   `json:"created"`
	Cancelled  bool    `json:"cancelled,omitempty"`
	Going      int     `json:"going"`
	Interested int     `json:"interested"`
	Mine       *string `json:"mine,omitempty"`
	told       int
}

func (st *store) migrateEvents() error {
	_, err := st.db.Exec(eventSchema)
	return err
}

const eventCols = `id,title,about,start,end_at,ch,by,created,cancelled,told`

func scanEvent(row interface{ Scan(...any) error }) (*event, error) {
	e := &event{}
	var c int
	if err := row.Scan(&e.ID, &e.Title, &e.About, &e.Start, &e.End, &e.Ch, &e.By, &e.Created, &c, &e.told); err != nil {
		return nil, err
	}
	e.Cancelled = c == 1
	return e, nil
}

func (st *store) event(id int64) (*event, error) {
	e, err := scanEvent(st.db.QueryRow(`SELECT `+eventCols+` FROM events WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	st.eventCounts(e)
	return e, nil
}

// eventCounts fills who's going and interested, and the maker's name.
func (st *store) eventCounts(e *event) {
	_ = st.db.QueryRow(`SELECT COUNT(CASE WHEN kind='going' THEN 1 END), COUNT(CASE WHEN kind='interested' THEN 1 END) FROM event_rsvp WHERE event=?`, e.ID).Scan(&e.Going, &e.Interested)
	e.ByName = st.userName(e.By)
}

func (st *store) eventsQuery(q string, args ...any) ([]*event, error) {
	rows, err := st.db.Query(`SELECT `+eventCols+` FROM events `+q, args...)
	if err != nil {
		return nil, err
	}
	var out []*event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	for _, e := range out {
		st.eventCounts(e)
	}
	return out, rows.Err()
}

// eventList: upcoming and now (cancelled ones until they'd have started), then the last few past, for uid.
func (st *store) eventList(uid string, now time.Time) ([]*event, error) {
	ms := now.UnixMilli()
	// Now: started, and not ended (no end: for 2 hours)
	cur, err := st.eventsQuery(`WHERE (start > ? AND (cancelled=0 OR start > ?)) OR (cancelled=0 AND start <= ? AND (end_at > ? OR (end_at = 0 AND start > ?))) ORDER BY start, id LIMIT ?`,
		ms, ms, ms, ms, ms-2*3600_000, eventUpcoming+50)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for _, e := range cur {
		seen[e.ID] = true
	}
	past, err := st.eventsQuery(`WHERE cancelled=0 AND start <= ? AND start > ? ORDER BY start DESC, id DESC LIMIT ?`, ms, ms-30*24*3600_000, eventPastShown+50)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, e := range past {
		if !seen[e.ID] && n < eventPastShown {
			cur = append(cur, e)
			n++
		}
	}
	mine := map[int64]string{}
	if rows, err := st.db.Query(`SELECT event, kind FROM event_rsvp WHERE uid=?`, uid); err == nil {
		for rows.Next() {
			var id int64
			var k string
			if rows.Scan(&id, &k) == nil {
				mine[id] = k
			}
		}
		rows.Close()
	}
	for _, e := range cur {
		k := mine[e.ID]
		e.Mine = &k
	}
	return cur, nil
}

func (st *store) eventMuted(uid string) bool {
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM event_mute WHERE uid=?`, uid).Scan(&n)
	return n > 0
}

// eventRate: saves per person in the last minute.
type eventRate struct {
	mu  sync.Mutex
	got map[string][]time.Time
}

var evRate = &eventRate{got: map[string][]time.Time{}}

func (r *eventRate) ok(uid string) bool { return r.okN(uid, eventSaveRate) }

func (s *server) eventOp(ss *session, t string, m map[string]any) bool {
	switch t {
	case "events":
		s.sendEvents(ss)
	case "event-save":
		s.eventSave(ss, m)
	case "event-del":
		s.eventCancel(ss, i64(m, "id"))
	case "event-rsvp":
		s.eventRSVP(ss, i64(m, "id"), str(m, "kind"))
	case "event-mute":
		on, _ := boolOf(m, "on")
		if on {
			_, _ = s.store.db.Exec(`INSERT OR IGNORE INTO event_mute(uid,at) VALUES(?,?)`, ss.uid, time.Now().UnixMilli())
		} else {
			_, _ = s.store.db.Exec(`DELETE FROM event_mute WHERE uid=?`, ss.uid)
		}
		ss.send(map[string]any{"t": "done", "op": t, "muted": on})
	default:
		return false
	}
	return true
}

func (s *server) sendEvents(ss *session) {
	items, err := s.store.eventList(ss.uid, time.Now())
	if err != nil {
		ss.refuse("events", "server", err.Error())
		return
	}
	if items == nil {
		items = []*event{}
	}
	h := s.hub
	h.mu.Lock()
	may := h.may(ss, pEvents)
	manage := h.may(ss, pManageCommunity)
	h.mu.Unlock()
	ss.send(map[string]any{"t": "events", "items": items, "muted": s.store.eventMuted(ss.uid), "lead": max(s.cfg.eventLead, 0), "may": may, "manage": manage})
}

// eventMayEdit: its maker (while they may make events) or a manager; h.mu held.
func (h *hub) eventMayEdit(ss *session, e *event) bool {
	return (e.By == ss.uid && h.may(ss, pEvents)) || h.may(ss, pManageCommunity)
}

func (s *server) eventSave(ss *session, m map[string]any) {
	const t = "event-save"
	h := s.hub
	id := i64(m, "id")
	var old *event
	if id != 0 {
		var err error
		if old, err = s.store.event(id); err != nil {
			ss.refuse(t, "gone", "no such event")
			return
		}
	}
	h.mu.Lock()
	allowed := !ss.api && ((old == nil && h.may(ss, pEvents)) || (old != nil && h.eventMayEdit(ss, old)))
	ch := i64(m, "ch")
	chOK := ch == 0 || (h.chans[ch] != nil && h.chans[ch].Comm == ss.comm && !h.chans[ch].Temp && h.chans[ch].Kind != "spacer")
	h.mu.Unlock()
	if !allowed {
		ss.refuse(t, "role", "not allowed")
		return
	}
	if !evRate.ok(ss.uid) {
		ss.refuse(t, "rate", "slow down")
		return
	}
	title := strings.Join(strings.Fields(str(m, "title")), " ")
	title = clip(title, 100)
	about := clip(strings.TrimSpace(str(m, "about")), 2000)
	start, end := i64(m, "start"), i64(m, "end")
	now := time.Now()
	switch {
	case title == "":
		ss.refuse(t, "bad", "an event needs a title")
		return
	case !chOK:
		ss.refuse(t, "bad", "no such channel")
		return
	case start <= 0 || (old == nil && start < now.Add(-time.Hour).UnixMilli()) || start > now.Add(eventMaxAhead).UnixMilli():
		ss.refuse(t, "bad", "the start must be within the next year")
		return
	case end != 0 && (end <= start || end > start+eventMaxLong.Milliseconds()):
		ss.refuse(t, "bad", "the end must be after the start, within a week")
		return
	}
	if old == nil {
		var n int
		_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM events WHERE start > ? AND cancelled=0`, now.UnixMilli()).Scan(&n)
		if n >= eventUpcoming {
			ss.refuse(t, "full", "the server has as many upcoming events as it takes")
			return
		}
		res, err := s.store.db.Exec(`INSERT INTO events(title,about,start,end_at,ch,by,created) VALUES(?,?,?,?,?,?,?)`, title, about, start, end, ch, ss.uid, now.UnixMilli())
		if err != nil {
			ss.refuse(t, "server", err.Error())
			return
		}
		id, _ = res.LastInsertId()
		log.Printf("event %d %q made by %s for %s", id, title, ss.uid, time.UnixMilli(start).UTC().Format(time.RFC3339))
	} else {
		// A new start is told again
		told := old.told
		if start != old.Start {
			told = 0
		}
		if _, err := s.store.db.Exec(`UPDATE events SET title=?, about=?, start=?, end_at=?, ch=?, told=? WHERE id=?`, title, about, start, end, ch, told, id); err != nil {
			ss.refuse(t, "server", err.Error())
			return
		}
		log.Printf("event %d %q changed by %s", id, title, ss.uid)
	}
	s.eventChanged(id, ss)
	ss.send(map[string]any{"t": "done", "op": t, "id": id})
}

func (s *server) eventCancel(ss *session, id int64) {
	const t = "event-del"
	e, err := s.store.event(id)
	if err != nil {
		ss.refuse(t, "gone", "no such event")
		return
	}
	h := s.hub
	h.mu.Lock()
	ok := !ss.api && h.eventMayEdit(ss, e)
	h.mu.Unlock()
	if !ok {
		ss.refuse(t, "role", "not allowed")
		return
	}
	_, _ = s.store.db.Exec(`UPDATE events SET cancelled=1 WHERE id=?`, id)
	log.Printf("event %d %q cancelled by %s", id, e.Title, ss.uid)
	s.eventChanged(id, nil)
	ss.send(map[string]any{"t": "done", "op": t, "id": id})
}

func (s *server) eventRSVP(ss *session, id int64, kind string) {
	const t = "event-rsvp"
	if kind != "" && kind != "going" && kind != "interested" {
		ss.refuse(t, "bad", "going, interested or nothing")
		return
	}
	e, err := s.store.event(id)
	if err != nil || e.Cancelled {
		ss.refuse(t, "gone", "no such event")
		return
	}
	if kind == "" {
		_, err = s.store.db.Exec(`DELETE FROM event_rsvp WHERE event=? AND uid=?`, id, ss.uid)
	} else {
		_, err = s.store.db.Exec(`INSERT INTO event_rsvp(event,uid,kind,at) VALUES(?,?,?,?) ON CONFLICT(event,uid) DO UPDATE SET kind=excluded.kind, at=excluded.at`, id, ss.uid, kind, time.Now().UnixMilli())
	}
	if err != nil {
		ss.refuse(t, "server", err.Error())
		return
	}
	s.eventChanged(id, ss)
}

// eventChanged tells everyone the event as it is now (who asked: with their own answer).
func (s *server) eventChanged(id int64, who *session) {
	e, err := s.store.event(id)
	if err != nil {
		return
	}
	var mine string
	if who != nil {
		_ = s.store.db.QueryRow(`SELECT kind FROM event_rsvp WHERE event=? AND uid=?`, id, who.uid).Scan(&mine)
	}
	h := s.hub
	h.mu.Lock()
	h.broadcast(map[string]any{"t": "event", "e": e}, func(o *session) bool { return o.comm == spaceID && !o.bot && (who == nil || o.uid != who.uid) })
	h.mu.Unlock()
	if who != nil {
		e.Mine = &mine
		for _, o := range h.sessionsOf(who.uid) {
			o.send(map[string]any{"t": "event", "e": e})
		}
	}
}

// eventLoop tells people about the events starting soon and now.
func (s *server) eventLoop() {
	for range time.Tick(eventTick) {
		s.eventTell(time.Now())
	}
}

// eventTell: the notices due at now. An event whose moment passed while the server was off (more than 10 minutes
// ago) is marked told without a notice: nobody wants "starting now" an hour late.
func (s *server) eventTell(now time.Time) {
	ms := now.UnixMilli()
	lead := int64(max(s.cfg.eventLead, 0)) * 60_000
	type due struct {
		e    *event
		kind string
	}
	var list []due
	evs, err := s.store.eventsQuery(`WHERE cancelled=0 AND told & ? = 0 AND start <= ? AND start > ? ORDER BY start`, toldStart, ms+lead, ms-24*3600_000)
	if err != nil {
		return
	}
	for _, e := range evs {
		switch {
		case e.Start <= ms:
			_, _ = s.store.db.Exec(`UPDATE events SET told = told | ? WHERE id=?`, toldSoon|toldStart, e.ID)
			if ms-e.Start <= 10*60_000 {
				list = append(list, due{e, "start"})
			}
		case lead > 0 && e.told&toldSoon == 0:
			_, _ = s.store.db.Exec(`UPDATE events SET told = told | ? WHERE id=?`, toldSoon, e.ID)
			list = append(list, due{e, "soon"})
		}
	}
	for _, d := range list {
		s.eventNotice(d.e, d.kind)
	}
}

func (s *server) eventNotice(e *event, kind string) {
	muted := map[string]bool{}
	if rows, err := s.store.db.Query(`SELECT uid FROM event_mute`); err == nil {
		for rows.Next() {
			var u string
			if rows.Scan(&u) == nil {
				muted[u] = true
			}
		}
		rows.Close()
	}
	h := s.hub
	h.mu.Lock()
	online := map[string]bool{}
	h.broadcast(map[string]any{"t": "event-notice", "kind": kind, "e": e}, func(o *session) bool {
		if o.bot || o.comm != spaceID {
			return false
		}
		online[o.uid] = true
		return !muted[o.uid]
	})
	h.mu.Unlock()
	log.Printf("event %d %q: %s told", e.ID, e.Title, kind)
	p := s.push
	if p == nil {
		return
	}
	// Push: only those who turned it on have a token here; not to the muted, not to who's connected
	rows, err := s.store.db.Query(`SELECT DISTINCT uid FROM push_tokens LIMIT 5000`)
	if err != nil {
		return
	}
	var uids []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil && !muted[u] && !online[u] && !s.mods.banned(u, spaceID) && !s.mods.banned(u, 0) {
			uids = append(uids, u)
		}
	}
	rows.Close()
	when := "now"
	if kind == "soon" {
		when = fmt.Sprintf("in %d min", max((e.Start-time.Now().UnixMilli()+30_000)/60_000, 1))
	}
	for _, u := range uids {
		p.notify(u, s.pushData(map[string]string{"kind": "event", "title": e.Title, "body": when, "ev": fmt.Sprint(e.ID), "when": kind}))
	}
}
