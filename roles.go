package main

// Moderation, on the community's permissions (community.go):
//
//	<- {"t":"kick","sid":…,"why":…}                                  (kick)
//	<- {"t":"ban","uid":…,"minutes":0 (for ever),"why":…}            (ban) out of the community
//	<- {"t":"unban","uid":…}                                         (ban)
//	<- {"t":"ch-ban","uid":…,"ch":…}  <- {"t":"ch-unban","uid":…,"ch":…}   (channel_ban)
//	<- {"t":"mute","uid":…,"minutes":…}  <- {"t":"unmute","uid":…}  (mute_chat) no writing in the community
//	<- {"t":"server-ban","uid":…,"minutes":…,"why":…}  <- {"t":"server-unban","uid":…}   (the server's staff)
//	<- {"t":"role","uid":…,"role":"admin"|"mod"|""}                  (manage_roles) the preset roles, for older apps
//	<- {"t":"bans"}  -> {"t":"bans","items":[…]}                      (any of the above) the community's
//
// Kept in the store's "mods" table. Its "ch" is the scope: the community for "ban" and "mute", the channel for
// "ch-ban", 0 for "sban" (the whole server).

import (
	"crypto/subtle"
	"log"
	"slices"
	"sync"
	"time"
)

type ban struct {
	UID   string `json:"uid"`
	Name  string `json:"name,omitempty"`
	Kind  string `json:"kind"` // "ban", "mute", "ch-ban", "sban"
	Ch    int64  `json:"ch,omitempty"`
	Until int64  `json:"until,omitempty"` // ms; 0: for ever
	Why   string `json:"why,omitempty"`
	By    string `json:"by"`
}

type mods struct {
	s    *server
	mu   sync.Mutex
	list []ban
}

func constEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func newMods(s *server) *mods {
	m := &mods{s: s}
	_, _ = s.store.db.Exec(`CREATE TABLE IF NOT EXISTS mods(uid TEXT NOT NULL, kind TEXT NOT NULL, ch INTEGER NOT NULL DEFAULT 0, until INTEGER NOT NULL DEFAULT 0, why TEXT NOT NULL DEFAULT '', by TEXT NOT NULL DEFAULT '', PRIMARY KEY(uid,kind,ch))`)
	// Before communities, "ban" and "mute" were for the whole server, which was the first community
	_, _ = s.store.db.Exec(`UPDATE mods SET ch=1 WHERE ch=0 AND kind IN ('ban','mute')`)
	// Ended bans and mutes aren't kept (with their reasons) for ever
	_, _ = s.store.db.Exec(`DELETE FROM mods WHERE until>0 AND until<?`, time.Now().UnixMilli())
	rows, err := s.store.db.Query(`SELECT uid,kind,ch,until,why,by FROM mods`)
	if err == nil {
		for rows.Next() {
			var b ban
			if rows.Scan(&b.UID, &b.Kind, &b.Ch, &b.Until, &b.Why, &b.By) == nil {
				m.list = append(m.list, b)
			}
		}
		rows.Close()
	}
	go m.prune()
	return m
}

// prune drops ended bans and mutes, hourly.
func (m *mods) prune() {
	for range time.Tick(time.Hour) {
		now := time.Now().UnixMilli()
		m.mu.Lock()
		kept := m.list[:0]
		for _, b := range m.list {
			if b.Until == 0 || b.Until > now {
				kept = append(kept, b)
			}
		}
		clear(m.list[len(kept):])
		m.list = kept
		m.mu.Unlock()
		_, _ = m.s.store.db.Exec(`DELETE FROM mods WHERE until>0 AND until<?`, now)
	}
}

func (m *mods) active(uid, kind string, scope int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UnixMilli()
	for _, b := range m.list {
		if b.UID == uid && b.Kind == kind && b.Ch == scope && (b.Until == 0 || b.Until > now) {
			return true
		}
	}
	return false
}

// banned: out of comm (or, comm 0, out of the whole server). A server ban counts everywhere.
func (m *mods) banned(uid string, comm int64) bool {
	return m.active(uid, "sban", 0) || (comm != 0 && m.active(uid, "ban", comm))
}

func (m *mods) muted(uid string, comm int64) bool       { return m.active(uid, "mute", comm) }
func (m *mods) voiceMuted(uid string, comm int64) bool  { return m.active(uid, "vmute", comm) }
func (m *mods) channelBanned(uid string, ch int64) bool { return m.active(uid, "ch-ban", ch) }

func (m *mods) put(b ban) {
	m.mu.Lock()
	kept := m.list[:0]
	for _, o := range m.list {
		if !(o.UID == b.UID && o.Kind == b.Kind && o.Ch == b.Ch) {
			kept = append(kept, o)
		}
	}
	m.list = append(kept, b)
	m.mu.Unlock()
	_, err := m.s.store.db.Exec(`INSERT OR REPLACE INTO mods(uid,kind,ch,until,why,by) VALUES(?,?,?,?,?,?)`, b.UID, b.Kind, b.Ch, b.Until, b.Why, b.By)
	if err != nil {
		log.Printf("mods: %v", err)
	}
}

func (m *mods) remove(uid, kind string, scope int64) {
	m.mu.Lock()
	kept := m.list[:0]
	for _, o := range m.list {
		if !(o.UID == uid && o.Kind == kind && o.Ch == scope) {
			kept = append(kept, o)
		}
	}
	m.list = kept
	m.mu.Unlock()
	_, _ = m.s.store.db.Exec(`DELETE FROM mods WHERE uid=? AND kind=? AND ch=?`, uid, kind, scope)
}

// dropScope forgets kinds at the given scopes (a deleted community or channel); the rows are deleted by the store.
func (m *mods) dropScope(kinds []string, scopes ...int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.list[:0]
	for _, o := range m.list {
		if !(slices.Contains(kinds, o.Kind) && slices.Contains(scopes, o.Ch)) {
			kept = append(kept, o)
		}
	}
	clear(m.list[len(kept):])
	m.list = kept
}

func until(minutes int64) int64 {
	if minutes <= 0 {
		return 0
	}
	return time.Now().Add(time.Duration(minutes) * time.Minute).UnixMilli()
}

// dispatch handles the moderation messages; false when t isn't one.
func (m *mods) dispatch(ss *session, t string, msg map[string]any) bool {
	h := m.s.hub
	need := map[string]perm{"kick": pKick, "ch-ban": pChannelBan, "ch-unban": pChannelBan, "mute": pMuteChat, "unmute": pMuteChat,
		"ban": pBan, "unban": pBan, "role": pManageRoles, "voice-mute": pMuteVoice, "voice-unmute": pMuteVoice}
	p, ok := need[t]
	staffOnly := t == "server-ban" || t == "server-unban"
	if !ok && !staffOnly && t != "bans" {
		return false
	}
	h.mu.Lock()
	comm := ss.comm
	var allowed bool
	switch {
	case staffOnly:
		allowed = h.staff(ss.uid)
	case t == "bans":
		allowed = comm != 0 && h.permsOf(comm, ss.uid)&(pBan|pMuteChat|pChannelBan) != 0
	default:
		allowed = comm != 0 && h.may(ss, p)
	}
	h.mu.Unlock()
	if !allowed {
		ss.refuse(t, "role", "not allowed")
		return true
	}
	uid := str(msg, "uid")
	why := clip(str(msg, "why"), 200)
	switch t {
	case "bans":
		chans := map[int64]bool{}
		h.mu.Lock()
		for id, c := range h.chans {
			if c.Comm == comm {
				chans[id] = true
			}
		}
		h.mu.Unlock()
		m.mu.Lock()
		var items []ban
		for _, b := range m.list {
			if ((b.Kind == "ban" || b.Kind == "mute" || b.Kind == "vmute") && b.Ch == comm) || (b.Kind == "ch-ban" && chans[b.Ch]) {
				items = append(items, b)
			}
		}
		m.mu.Unlock()
		for i := range items {
			items[i].Name = m.s.store.userName(items[i].UID)
		}
		if items == nil {
			items = []ban{}
		}
		ss.send(map[string]any{"t": "bans", "items": items})
		return true
	case "kick":
		h.mu.Lock()
		o := h.sessions[int(i64(msg, "sid"))]
		var ok bool
		if o != nil {
			ok = o.comm == comm && h.outranks(ss, o.uid)
		}
		h.mu.Unlock()
		if o == nil {
			return true
		}
		if !ok {
			ss.refuse(t, "role", "they rank as high as you")
			return true
		}
		o.send(map[string]any{"t": "kicked", "by": ss.name, "why": why})
		log.Printf("kick: %s by %s (%s)", o.uid, ss.uid, why)
		time.AfterFunc(300*time.Millisecond, o.close)
		return true
	case "role":
		// The older apps' simple roles, as the community's preset roles
		nr := str(msg, "role")
		if !safeID.MatchString(uid) || (nr != "" && nr != "mod" && nr != "admin") {
			ss.refuse(t, "bad", "bad role")
			return true
		}
		h.mu.Lock()
		a := h.access(comm)
		var mod, admin int64
		for _, r := range a.roles {
			switch r.Name {
			case "Moderator":
				mod = r.ID
			case "Admin":
				admin = r.ID
			}
		}
		var ids []any
		for _, id := range a.member[uid] {
			if id != mod && id != admin {
				ids = append(ids, float64(id))
			}
		}
		h.mu.Unlock()
		switch nr {
		case "mod":
			ids = append(ids, float64(mod))
		case "admin":
			ids = append(ids, float64(admin))
		}
		h.setMemberRoles(ss, uid, ids)
		return true
	}
	if !safeID.MatchString(uid) {
		ss.refuse(t, "bad", "bad uid")
		return true
	}
	h.mu.Lock()
	ok = h.outranks(ss, uid)
	if t == "ch-ban" || t == "ch-unban" {
		c := h.chans[i64(msg, "ch")]
		ok = ok && c != nil && c.Comm == comm
	}
	if staffOnly {
		ok = !h.staff(uid) || h.s.store.role(ss.uid) == roleOwner
	}
	h.mu.Unlock()
	// Every action on someone (lifting one included): only on people ranked lower, never on oneself (a muted
	// moderator can't unmute themselves), and channel bans only in this community's channels
	if !ok {
		ss.refuse(t, "role", "they rank as high as you")
		return true
	}
	switch t {
	case "ban":
		m.put(ban{UID: uid, Kind: "ban", Ch: comm, Until: until(i64(msg, "minutes")), Why: why, By: ss.uid})
		_ = m.s.store.leave(comm, uid)
		for _, o := range h.sessionsOf(uid) {
			if o.comm == comm {
				o.send(map[string]any{"t": "kicked", "by": ss.name, "why": why, "ban": true, "community": comm})
			}
		}
		h.afterRemoval(comm, uid, "banned")
		m.s.bus.kick(spaceOf(comm), uid)
	case "unban":
		m.remove(uid, "ban", comm)
	case "server-ban":
		m.put(ban{UID: uid, Kind: "sban", Until: until(i64(msg, "minutes")), Why: why, By: ss.uid})
		for _, o := range h.sessionsOf(uid) {
			o.send(map[string]any{"t": "kicked", "by": ss.name, "why": why, "ban": true})
			time.AfterFunc(300*time.Millisecond, o.close)
		}
		m.s.bus.kick("", uid)
	case "server-unban":
		m.remove(uid, "sban", 0)
	case "mute":
		m.put(ban{UID: uid, Kind: "mute", Ch: comm, Until: until(i64(msg, "minutes")), Why: why, By: ss.uid})
	case "unmute":
		m.remove(uid, "mute", comm)
	case "ch-ban":
		ch := i64(msg, "ch")
		m.put(ban{UID: uid, Kind: "ch-ban", Ch: ch, Until: until(i64(msg, "minutes")), Why: why, By: ss.uid})
		var moved []*session
		h.mu.Lock()
		def := h.defaultChannel(comm)
		for _, o := range h.sessions {
			if o.uid == uid && o.ch == ch {
				o.ch = def
				o.talk = false
				h.memberChanged(o)
				moved = append(moved, o)
			}
		}
		h.mu.Unlock()
		for _, o := range moved {
			o.send(map[string]any{"t": "moved", "ch": def, "by": ss.name})
		}
		if len(moved) > 0 {
			m.s.voice.sync(ch, def)
		}
	case "ch-unban":
		m.remove(uid, "ch-ban", i64(msg, "ch"))
	case "voice-mute", "voice-unmute":
		on := t == "voice-mute"
		if on {
			m.put(ban{UID: uid, Kind: "vmute", Ch: comm, Until: until(i64(msg, "minutes")), Why: why, By: ss.uid})
		} else {
			m.remove(uid, "vmute", comm)
		}
		var chs []int64
		h.mu.Lock()
		for _, o := range h.sessions {
			if o.uid == uid && o.comm == comm {
				o.vmuted = on
				h.memberChanged(o)
				chs = append(chs, o.ch)
			}
		}
		h.mu.Unlock()
		m.s.voice.sync(chs...)
	}
	log.Printf("%s: %s by %s", t, uid, ss.uid)
	ss.send(map[string]any{"t": "done", "op": t, "uid": uid})
	return true
}
