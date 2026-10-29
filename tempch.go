package main

// Temporary voice channels: anyone with "temp_channel" (on @everyone by default) makes one for themselves, is moved
// into it and is its channel admin (renames it, sets its password and limit, takes people out of it, deletes it).
// It goes by itself once nobody has been in it for SV_TEMP_EMPTY seconds (60).
//
//	<- {"t":"temp-create","name":…,"password"?:…,"max"?:N}   -> "channel" (temp:true, owner) to everyone, then
//	                                                            the creator's "member" in it and {"t":"done","op":"temp-create","id"}
//	<- {"t":"temp-edit","id","name"?,"password"?,"max"?}       its creator, or manage_channels
//	<- {"t":"temp-del","id"}                                   its creator, or manage_channels (ch-del does it too)
//	ch-kick: its creator may take people out of it (not those who may kick)
//
// They live only in memory: never in the channels table, so a restart has none; their ids are far above the
// table's (tempBase + a millisecond clock), so one is never taken for a saved channel, and what was kept under
// their id (chat, channel bans) is cleared when they go and again at start. They don't count as channels for the
// channel management (the 500 limit, parents, the default channel), and ch-save doesn't touch them.
// Limits: SV_TEMP_PER_USER (1) at once per person, SV_TEMP_MAX (20) on the server, 3 made per person per 5 min.

import (
	"log"
	"time"
)

// tempBase: every temporary channel's id is above it (2^42, still exact in JSON's doubles).
const tempBase int64 = 1 << 42

// Vars so tests can change them.
var (
	tempTick       = 2 * time.Second
	tempRateN      = 3
	tempRateWindow = 5 * time.Minute
)

// isTempID: an id of a temporary channel (kept or not).
func isTempID(id int64) bool { return id >= tempBase }

// dropTempLeftovers: what a temporary channel left in the database before a stop (its chat); at start.
func (st *store) dropTempLeftovers() {
	_, _ = st.db.Exec(`DELETE FROM messages WHERE conv GLOB 'c:[0-9]*' AND CAST(substr(conv, 3) AS INTEGER) >= ?`, tempBase)
}

// tempAdmin: ss made the temporary channel c (its channel admin); h.mu held.
func (h *hub) tempAdmin(ss *session, c *channel) bool {
	return c != nil && c.Temp && c.Owner != "" && c.Owner == ss.uid
}

// tempCount: temporary channels now, all and uid's; h.mu held.
func (h *hub) tempCount(uid string) (all, mine int) {
	for _, c := range h.chans {
		if c.Temp {
			all++
			if c.Owner == uid {
				mine++
			}
		}
	}
	return
}

// tempRate: uid may make another one now (a few per window); h.mu held.
func (h *hub) tempRate(uid string) bool {
	now := time.Now()
	kept := h.tempMade[uid][:0]
	for _, at := range h.tempMade[uid] {
		if now.Sub(at) < tempRateWindow {
			kept = append(kept, at)
		}
	}
	if len(kept) >= tempRateN {
		h.tempMade[uid] = kept
		return false
	}
	h.tempMade[uid] = append(kept, now)
	for u, l := range h.tempMade { // forgotten after the window
		if len(l) > 0 && now.Sub(l[len(l)-1]) >= tempRateWindow {
			delete(h.tempMade, u)
		}
	}
	return true
}

func (h *hub) tempOp(ss *session, t string, m map[string]any) bool {
	switch t {
	case "temp-create":
		h.tempCreate(ss, m)
	case "temp-edit":
		h.tempEdit(ss, m)
	case "temp-del":
		id := i64(m, "id")
		h.mu.Lock()
		c := h.chans[id]
		ok := c != nil && c.Temp && c.Comm == ss.comm && (h.tempAdmin(ss, c) || h.may(ss, pManageChannels))
		h.mu.Unlock()
		if !ok {
			ss.refuse(t, "role", "not allowed")
			return true
		}
		h.tempDelete(id, "by "+ss.uid)
		ss.send(map[string]any{"t": "done", "op": t, "id": id})
	default:
		return false
	}
	return true
}

func (h *hub) tempCreate(ss *session, m map[string]any) {
	const t = "temp-create"
	name := cleanName(str(m, "name"))
	if name == "" {
		ss.refuse(t, "bad", "a channel needs a name")
		return
	}
	maxU := int(i64(m, "max"))
	if maxU < 0 || maxU > 99 {
		ss.refuse(t, "bad", "the limit is 0 (none) to 99")
		return
	}
	h.mu.Lock()
	allowed := ss.comm != 0 && !ss.bot && h.may(ss, pTempChannel)
	all, mine := h.tempCount(ss.uid)
	switch {
	case !allowed:
		h.mu.Unlock()
		ss.refuse(t, "role", "not allowed")
		return
	case mine >= h.s.cfg.tempPerUser:
		h.mu.Unlock()
		ss.refuse(t, "many", "you have a temporary channel already")
		return
	case all >= h.s.cfg.tempMax:
		h.mu.Unlock()
		ss.refuse(t, "full", "the server has as many temporary channels as it takes")
		return
	case !h.tempRate(ss.uid):
		h.mu.Unlock()
		ss.refuse(t, "rate", "slow down")
		return
	}
	h.mu.Unlock()
	// bcrypt outside the hub's lock
	hash, err := hashPass(str(m, "password"))
	if err != nil {
		ss.refuse(t, "bad", err.Error())
		return
	}
	h.mu.Lock()
	// Counted again: another one may have been made meanwhile
	all, mine = h.tempCount(ss.uid)
	if mine >= h.s.cfg.tempPerUser || all >= h.s.cfg.tempMax || h.sessions[ss.sid] != ss {
		h.mu.Unlock()
		ss.refuse(t, "many", "you have a temporary channel already")
		return
	}
	id := max(h.tempNext+1, tempBase+time.Now().UnixMilli())
	h.tempNext = id
	c := &channel{ID: id, Comm: ss.comm, Name: name, Pos: 100000, MaxUsers: maxU, Temp: true, Owner: ss.uid, pass: hash}
	c.HasPass = hash != ""
	h.chans[id] = c
	h.toComm(c.Comm, map[string]any{"t": "channel", "c": *c})
	old := ss.ch
	ss.ch, ss.talk, ss.talker = id, false, false
	h.memberChanged(ss)
	h.mu.Unlock()
	log.Printf("temporary channel %d %q made by %s", id, name, ss.uid)
	ss.send(map[string]any{"t": "done", "op": t, "id": id})
	h.s.voice.sync(old, id)
	h.s.music.joined(ss, id)
}

func (h *hub) tempEdit(ss *session, m map[string]any) {
	const t = "temp-edit"
	id := i64(m, "id")
	h.mu.Lock()
	c := h.chans[id]
	ok := c != nil && c.Temp && c.Comm == ss.comm && (h.tempAdmin(ss, c) || h.may(ss, pManageChannels))
	h.mu.Unlock()
	if !ok {
		ss.refuse(t, "role", "not allowed")
		return
	}
	var hash *string
	if p, ok := m["password"].(string); ok {
		hp, err := hashPass(p)
		if err != nil {
			ss.refuse(t, "bad", err.Error())
			return
		}
		hash = &hp
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c = h.chans[id]
	if c == nil || !c.Temp {
		ss.refuse(t, "gone", "no such channel")
		return
	}
	nc := *c
	if v, ok := m["name"].(string); ok {
		if n := cleanName(v); n != "" {
			nc.Name = n
		}
	}
	if _, ok := m["max"]; ok {
		if v := int(i64(m, "max")); v >= 0 && v <= 99 {
			nc.MaxUsers = v
		}
	}
	if hash != nil {
		nc.pass = *hash
		nc.HasPass = nc.pass != ""
	}
	h.chans[id] = &nc
	h.toComm(nc.Comm, map[string]any{"t": "channel", "c": nc})
	ss.send(map[string]any{"t": "done", "op": t, "id": id})
}

// tempDelete takes a temporary channel away (anyone still in it goes to the default channel), with what was kept
// under its id.
func (h *hub) tempDelete(id int64, why string) {
	h.mu.Lock()
	c := h.chans[id]
	if c == nil || !c.Temp {
		h.mu.Unlock()
		return
	}
	def := h.defaultChannel(c.Comm)
	var moved []*session
	for _, o := range h.sessions {
		if o.ch == id {
			o.ch, o.talk, o.talker = def, false, false
			moved = append(moved, o)
		}
	}
	delete(h.chans, id)
	h.toComm(c.Comm, map[string]any{"t": "channel-del", "id": id})
	for _, o := range moved {
		h.memberChanged(o)
	}
	h.mu.Unlock()
	_, _ = h.s.store.db.Exec(`DELETE FROM messages WHERE conv=?`, chanConv(id))
	if h.s.mods != nil {
		h.s.mods.dropScope([]string{"ch-ban"}, id)
		_, _ = h.s.store.db.Exec(`DELETE FROM mods WHERE kind='ch-ban' AND ch=?`, id)
	}
	log.Printf("temporary channel %d %q gone (%s)", id, c.Name, why)
	if len(moved) > 0 {
		h.s.voice.sync(id, def)
	}
}

// tempLoop deletes the temporary channels empty for SV_TEMP_EMPTY seconds.
func (h *hub) tempLoop(every time.Duration) {
	for range time.Tick(every) {
		h.tempSweep(time.Now())
	}
}

func (h *hub) tempSweep(now time.Time) {
	empty := time.Duration(max(h.s.cfg.tempEmpty, 1)) * time.Second
	var gone []int64
	h.mu.Lock()
	for id, c := range h.chans {
		if !c.Temp {
			continue
		}
		if h.countIn(id) > 0 {
			c.emptySince = time.Time{}
			continue
		}
		if c.emptySince.IsZero() {
			c.emptySince = now
		}
		if now.Sub(c.emptySince) >= empty {
			gone = append(gone, id)
		}
	}
	h.mu.Unlock()
	for _, id := range gone {
		h.tempDelete(id, "empty")
	}
}
