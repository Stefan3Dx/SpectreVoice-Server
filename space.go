package main

// One server is one space, as on TeamSpeak: whoever gets in (the server password or an invite that skips it, no
// ban) is in it, with its channels, roles, server chat ("g:1"), bots, music bot and files. Inside, the space is
// community 1 of the store (community.go): servers from before keep their main community as the space; other
// communities they had stay in the database untouched but hidden (see migrateOneSpace).
//
//	<- {"t":"server-save",…}                               settings.go: name, description, icon, password
//	<- {"t":"role-save","id"?,"name","color","pos","perms":[…]}   (manage_roles) below your own place
//	<- {"t":"role-del","id"}                               (manage_roles)
//	<- {"t":"member-roles","uid","roles":[ids]}            (manage_roles) roles below yours, on people below you
//	<- {"t":"community-save",…}                            older apps: as server-save
//	<- {"t":"invite-use","invite":CODE}                    an invite opened while on its server: counted, its
//	                                                       channel joined (older apps: community-join; a new
//	                                                       connection gives the invite in the hello)
//	<- enter, communities, discover, community-*           older apps: ignored
//
// The welcome carries: server, community (the space, for older apps), roles, perms, channels, members.

import (
	"log"
	"time"
)

// spaceID: the store's community that is the server's space.
const spaceID int64 = 1

// goneOps: the messages of several communities per server; older apps may still send them: nothing happens.
var goneOps = map[string]bool{
	"enter": true, "communities": true, "discover": true, "community-create": true, "community-leave": true,
	"community-delete": true, "community-find": true, "community-requests": true, "community-answer": true,
	"community-new-code": true,
}

// enterPre: what enterWith reads from the database, read before taking h.mu (the database has one connection: a
// slow query mustn't hold the whole hub).
type enterPre struct {
	roles  []crole
	wantCh int64 // the channel the app asks to be back in (a reconnect), if still allowed
}

func (h *hub) preEnter() *enterPre {
	p := &enterPre{}
	p.roles, _ = h.s.store.rolesOf(spaceID)
	return p
}

// enterWith puts a new session into the space and gives what the app needs to show it; h.mu held. Those already
// there are told it came.
func (h *hub) enterWith(ss *session, pre *enterPre) map[string]any {
	comm := spaceID
	ss.comm = comm
	ss.ch = 0
	ss.talk = false
	ss.whispering, ss.whisperTo, ss.whisperCh = false, nil, nil
	out := map[string]any{"staff": h.staff(ss.uid), "live": h.liveList(comm)}
	a := h.access(comm)
	ss.role, ss.roles = h.legacyRole(comm, ss.uid), a.member[ss.uid]
	ss.vmuted, ss.talker = h.s.mods.voiceMuted(ss.uid, comm), false
	ss.ch = h.defaultChannel(comm)
	if pre != nil && pre.wantCh != 0 && h.mayLandIn(ss, pre.wantCh) {
		ss.ch = pre.wantCh
	}
	members := make([]member, 0)
	for _, o := range h.sessions {
		if o.comm == comm {
			members = append(members, o.member())
		}
	}
	var rs []crole
	if pre != nil {
		rs = pre.roles
	}
	if rs == nil {
		rs = []crole{}
	}
	cm := *a.comm
	out["community"] = cm
	out["channels"] = h.sortedChannels(comm)
	out["members"] = members
	out["roles"] = rs
	out["perms"] = permList(h.permsOf(comm, ss.uid))
	out["role"] = ss.role
	out["space"] = spaceOf(comm)
	h.broadcast(map[string]any{"t": "member", "m": ss.member()}, func(o *session) bool { return o.comm == comm && o != ss })
	return out
}

// mayLandIn: a reconnecting app goes straight back into ch when it could join it now without a password (a locked
// one asks again); h.mu held.
func (h *hub) mayLandIn(ss *session, ch int64) bool {
	c := h.chans[ch]
	if c == nil || c.Comm != ss.comm || c.Kind != "" {
		return false
	}
	if c.pass != "" && !h.may(ss, pBypassLocks) {
		return false
	}
	if c.MaxUsers > 0 && h.countIn(ch) >= c.MaxUsers && !h.may(ss, pBypassLocks) {
		return false
	}
	return !h.s.mods.channelBanned(ss.uid, ch) || h.may(ss, pChannelBan)
}

func (h *hub) communityOp(ss *session, t string, m map[string]any) bool {
	st := h.s.store
	if goneOps[t] {
		return true
	}
	switch t {
	case "server-save", "community-save":
		h.serverSave(ss, t, m)
	case "invite-use", "community-join":
		// An invite opened while already on its server (older apps: community-join): counted, into its channel
		code := str(m, "invite")
		if code == "" {
			return true
		}
		inv, err := h.s.useInvite(code)
		if err != nil {
			ss.refuse(t, "invite", err.Error())
			return true
		}
		if inv.Channel != 0 {
			h.join(ss, inv.Channel, "")
		}
	case "role-save":
		h.saveRole(ss, m)
	case "role-del":
		h.mu.Lock()
		comm := ss.comm
		id := i64(m, "id")
		r, ok := h.access(comm).roles[id]
		allowed := ok && h.may(ss, pManageRoles) && !r.Everyone && r.Pos < h.rank(comm, ss.uid)
		h.mu.Unlock()
		if !allowed {
			ss.refuse(t, "role", "not allowed")
			return true
		}
		if err := st.deleteRole(comm, id); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		h.rolesChanged(comm)
	case "member-roles":
		h.setMemberRoles(ss, str(m, "uid"), m["roles"])
	default:
		return false
	}
	return true
}

// afterRemoval: uid isn't on the server any more (banned): its sessions are closed (the "kicked" notice went
// first), and everyone's roles are sent again.
func (h *hub) afterRemoval(comm int64, uid, why string) {
	_ = why
	h.mu.Lock()
	var gone []*session
	for _, o := range h.sessions {
		if o.uid == uid && o.comm == comm {
			gone = append(gone, o)
		}
	}
	h.refreshRoles(comm)
	h.mu.Unlock()
	for _, o := range gone {
		o.closeSoon()
	}
}

// rolesChanged: the roles changed: everyone gets the list again, and their own.
func (h *hub) rolesChanged(comm int64) {
	rs, _ := h.s.store.rolesOf(comm)
	if rs == nil {
		rs = []crole{}
	}
	h.mu.Lock()
	h.toComm(comm, map[string]any{"t": "roles", "items": rs})
	h.refreshRoles(comm)
	h.mu.Unlock()
}

func (h *hub) saveRole(ss *session, m map[string]any) {
	const t = "role-save"
	h.mu.Lock()
	comm := ss.comm
	if comm == 0 || !h.may(ss, pManageRoles) {
		h.mu.Unlock()
		ss.refuse(t, "role", "not allowed")
		return
	}
	a := h.access(comm)
	mine := h.permsOf(comm, ss.uid)
	top := h.rank(comm, ss.uid)
	id := i64(m, "id")
	r := crole{Comm: comm}
	if id != 0 {
		old, ok := a.roles[id]
		if !ok || (!old.Everyone && old.Pos >= top) {
			h.mu.Unlock()
			ss.refuse(t, "role", "that role is above yours")
			return
		}
		r = old
	}
	h.mu.Unlock()
	if n := cleanName(str(m, "name")); n != "" && !r.Everyone {
		r.Name = n
	}
	if r.Name == "" {
		ss.refuse(t, "bad", "a role needs a name")
		return
	}
	if v, ok := m["color"].(float64); ok {
		r.Color = int64(v)
	}
	if v, ok := m["pos"].(float64); ok && !r.Everyone {
		r.Pos = max(1, int(v))
	}
	if !r.Everyone && r.Pos >= top {
		r.Pos = top - 1
	}
	if !r.Everyone && r.Pos < 1 {
		ss.refuse(t, "role", "no room below your own role")
		return
	}
	if v, ok := m["perms"]; ok {
		// Nobody gives what they don't have
		r.p = permOf(v) & mine
	}
	if err := h.s.store.saveRole(&r); err != nil {
		ss.refuse(t, "server", err.Error())
		return
	}
	log.Printf("role %d %q saved by %s", r.ID, r.Name, ss.uid)
	h.rolesChanged(comm)
}

func (h *hub) setMemberRoles(ss *session, uid string, v any) {
	const t = "member-roles"
	if !safeID.MatchString(uid) {
		ss.refuse(t, "bad", "bad uid")
		return
	}
	var want []int64
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if f, ok := x.(float64); ok {
				want = append(want, int64(f))
			}
		}
	}
	h.mu.Lock()
	comm := ss.comm
	if comm == 0 || !h.may(ss, pManageRoles) || !h.s.store.isMember(comm, uid) {
		h.mu.Unlock()
		ss.refuse(t, "role", "not allowed")
		return
	}
	if uid != ss.uid && !h.outranks(ss, uid) {
		h.mu.Unlock()
		ss.refuse(t, "role", "they rank as high as you")
		return
	}
	a := h.access(comm)
	top := h.rank(comm, ss.uid)
	// Roles at or above one's own place stay as they are; the rest are what was asked
	var next []int64
	for _, id := range a.member[uid] {
		if r, ok := a.roles[id]; ok && r.Pos >= top {
			next = append(next, id)
		}
	}
	for _, id := range want {
		if r, ok := a.roles[id]; ok && !r.Everyone && r.Pos < top {
			next = append(next, id)
		}
	}
	h.mu.Unlock()
	if err := h.s.store.setMemberRoles(comm, uid, next); err != nil {
		ss.refuse(t, "server", err.Error())
		return
	}
	log.Printf("%s's roles %v (by %s)", uid, next, ss.uid)
	h.mu.Lock()
	h.refreshRoles(comm)
	h.mu.Unlock()
	ss.send(map[string]any{"t": "done", "op": t, "uid": uid})
}

// closeSoon: the connection goes a moment after what was just sent to it.
func (ss *session) closeSoon() { time.AfterFunc(300*time.Millisecond, ss.close) }
