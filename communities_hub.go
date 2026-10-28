package main

// The hub's side of communities and roles:
//
//	<- {"t":"enter","c":ID}                          go into one of your communities -> {"t":"community",…}
//	<- {"t":"communities"}                           -> {"t":"communities","items":[…]}  (yours)
//	<- {"t":"discover"}                              -> {"t":"discover","items":[]}  (no public list: by code, community_codes.go)
//	<- {"t":"community-join","invite":CODE | "code":C} by invite, or by the community's code (open: in; closed: a request)
//	<- {"t":"community-create","name":…,"open":bool} (when the server allows) a new one, yours
//	<- {"t":"community-save","name","about","icon","open"}   (manage_community) the current one
//	<- {"t":"community-leave","c":ID}                leave one (not the owner)
//	<- {"t":"community-delete","c":ID}               the owner (or server staff) deletes it
//	<- {"t":"role-save","id"?,"name","color","pos","perms":[…]}   (manage_roles) below your own place
//	<- {"t":"role-del","id"}                         (manage_roles)
//	<- {"t":"member-roles","uid","roles":[ids]}      (manage_roles) roles below yours, on people below you
//
// "community" (and the welcome) carries: community, communities, roles, perms, channels, members.

import (
	"log"
	"strings"
)

// pickCommunity: where a connection starts: the one asked for, else the last one, else the first joined, else the
// main one if it's open (joining it); 0 when there's none.
func (h *hub) pickCommunity(uid string, want int64) int64 {
	st := h.s.store
	if want != 0 && st.isMember(want, uid) {
		return want
	}
	if last := st.lastCommunity(uid); last != 0 && st.isMember(last, uid) {
		return last
	}
	if list, _ := st.communitiesOf(uid); len(list) > 0 {
		return list[0].ID
	}
	if c, err := st.community(1); err == nil && c.Open && !h.s.mods.banned(uid, 1) {
		if st.join(1, uid) == nil {
			return 1
		}
	}
	return 0
}

// enterLocked puts ss into comm (0: none) and gives what the app needs to show it; h.mu held. Whoever was with ss
// before is told it left, those in comm that it came.
func (h *hub) enterLocked(ss *session, comm int64) map[string]any {
	return h.enterWith(ss, comm, nil)
}

// enterPre: what enterLocked reads from the database, read before taking h.mu (the database has one connection: a
// slow query mustn't hold the whole hub).
type enterPre struct {
	comms []community
	roles []crole
}

func (h *hub) preEnter(uid string, comm int64) *enterPre {
	p := &enterPre{comms: h.myCommunities(uid)}
	if comm != 0 {
		p.roles, _ = h.s.store.rolesOf(comm)
	}
	return p
}

func (h *hub) enterWith(ss *session, comm int64, pre *enterPre) map[string]any {
	old, oldCh := ss.comm, ss.ch
	if old != 0 && old != comm {
		h.toComm(old, map[string]any{"t": "member-left", "sid": ss.sid, "uid": ss.uid})
	}
	ss.comm = comm
	ss.ch = 0
	ss.talk = false
	ss.whispering, ss.whisperTo, ss.whisperCh = false, nil, nil
	var comms []community
	if pre != nil {
		comms = pre.comms
	} else {
		comms = h.myCommunities(ss.uid)
	}
	out := map[string]any{"communities": comms, "staff": h.staff(ss.uid), "live": h.liveList(comm)}
	if comm == 0 {
		ss.role, ss.roles = roleMember, nil
		out["community"] = nil
		out["channels"] = []*channel{}
		out["members"] = []member{}
		out["roles"] = []crole{}
		out["perms"] = []string{}
		return out
	}
	a := h.access(comm)
	ss.role, ss.roles = h.legacyRole(comm, ss.uid), a.member[ss.uid]
	ss.vmuted, ss.talker = h.s.mods.voiceMuted(ss.uid, comm), false
	ss.ch = h.defaultChannel(comm)
	members := make([]member, 0)
	for _, o := range h.sessions {
		if o.comm == comm {
			members = append(members, o.member())
		}
	}
	var rs []crole
	if pre != nil {
		rs = pre.roles
	} else {
		rs, _ = h.s.store.rolesOf(comm)
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
	newCh := ss.ch // read here, under h.mu
	go func() {
		h.s.store.setLastCommunity(ss.uid, comm)
		if oldCh != 0 {
			h.s.voice.sync(oldCh, newCh)
		}
	}()
	return out
}

// enter: the app moves ss into another of its communities.
func (h *hub) enter(ss *session, comm int64) {
	if comm == 0 || !h.s.store.isMember(comm, ss.uid) {
		ss.refuse("enter", "gone", "not a member of that community")
		return
	}
	if h.s.mods.banned(ss.uid, comm) {
		ss.refuse("enter", "banned", "you are banned from that community")
		return
	}
	pre := h.preEnter(ss.uid, comm)
	h.mu.Lock()
	payload := h.enterWith(ss, comm, pre)
	payload["t"] = "community"
	ss.send(payload)
	h.mu.Unlock()
	h.s.voice.syncAll()
}

func (h *hub) myCommunities(uid string) []community {
	list, err := h.s.store.communitiesOf(uid)
	if err != nil || list == nil {
		return []community{}
	}
	return list
}

func (h *hub) communityOp(ss *session, t string, m map[string]any) bool {
	st := h.s.store
	switch t {
	case "enter":
		h.enter(ss, i64(m, "c"))
	case "communities":
		ss.send(map[string]any{"t": "communities", "items": h.myCommunities(ss.uid)})
	case "discover":
		// No public list of communities any more (privacy): they're found by their code
		ss.send(map[string]any{"t": "discover", "items": []community{}})
	case "community-join":
		comm := i64(m, "c")
		if code := str(m, "invite"); code != "" {
			// A banned person doesn't use up a place of the invite
			if in, err := st.invite(code); err == nil && h.s.mods.banned(ss.uid, in.Comm) {
				ss.refuse(t, "banned", "you are banned from that community")
				return true
			}
			inv, err := h.s.useInvite(code)
			if err != nil {
				ss.refuse(t, "invite", err.Error())
				return true
			}
			comm = inv.Comm
			if inv.Channel != 0 {
				defer func() { h.join(ss, inv.Channel, "") }()
			}
		} else if code := str(m, "code"); code != "" {
			h.mu.Lock()
			name := ss.name
			h.mu.Unlock()
			c, requested, err := h.joinByCode(ss, code, name)
			if err != nil {
				ss.refuse(t, "code", err.Error())
				return true
			}
			if requested {
				return true
			}
			comm = c
		} else if h.s.mods.banned(ss.uid, comm) {
			ss.refuse(t, "banned", "you are banned from that community")
			return true
		} else if _, err := st.community(comm); err != nil || !st.isMember(comm, ss.uid) {
			// By id only for its members (back into one, open or closed); newcomers by invite or code
			ss.refuse(t, "closed", "use the community's code or an invite")
			return true
		}
		if h.s.mods.banned(ss.uid, comm) {
			ss.refuse(t, "banned", "you are banned from that community")
			return true
		}
		if err := st.join(comm, ss.uid); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		log.Printf("community %d: %s joined", comm, ss.uid)
		h.enter(ss, comm)
	case "community-create":
		name := cleanName(str(m, "name"))
		if name == "" {
			ss.refuse(t, "bad", "a community needs a name")
			return true
		}
		staff := h.staff(ss.uid)
		if !staff && h.s.cfg.createCommunities != "anyone" {
			ss.refuse(t, "role", "only the server's admins make communities here")
			return true
		}
		if !staff && st.ownedBy(ss.uid) >= h.s.limits().OwnCommunities {
			ss.refuse(t, "full", "you have too many communities")
			return true
		}
		open, _ := boolOf(m, "open")
		c, err := st.createCommunity(name, ss.uid, open)
		if err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		chs, _ := st.channels()
		h.mu.Lock()
		for _, ch := range chs {
			if ch.Comm == c.ID {
				h.chans[ch.ID] = ch
			}
		}
		h.mu.Unlock()
		log.Printf("community %d %q made by %s", c.ID, c.Name, ss.uid)
		h.enter(ss, c.ID)
	case "community-save":
		h.mu.Lock()
		if ss.comm == 0 || !h.may(ss, pManageCommunity) {
			h.mu.Unlock()
			ss.refuse(t, "role", "not allowed")
			return true
		}
		c := *h.access(ss.comm).comm
		h.mu.Unlock()
		if n := cleanName(str(m, "name")); n != "" {
			c.Name = n
		}
		if v, ok := m["about"].(string); ok {
			c.About = clip(strings.TrimSpace(v), 1000)
		}
		if v, ok := m["icon"].(string); ok && (v == "" || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "/f/")) {
			c.Icon = clip(v, 500)
		}
		if v, ok := boolOf(m, "open"); ok {
			c.Open = v
		}
		if err := st.saveCommunity(&c); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		h.mu.Lock()
		h.forget(c.ID)
		h.toComm(c.ID, map[string]any{"t": "community-info", "community": c})
		h.mu.Unlock()
	case "community-leave":
		comm := i64(m, "c")
		c, err := st.community(comm)
		if err != nil {
			return true
		}
		if c.Owner == ss.uid {
			ss.refuse(t, "owner", "the owner can't leave (give it to someone, or delete it)")
			return true
		}
		_ = st.leave(comm, ss.uid)
		h.afterRemoval(comm, ss.uid, "")
	case "community-delete":
		comm := i64(m, "c")
		c, err := st.community(comm)
		if err != nil {
			return true
		}
		if c.Owner != ss.uid && !h.staff(ss.uid) {
			ss.refuse(t, "role", "only the owner deletes a community")
			return true
		}
		if comm == 1 {
			ss.refuse(t, "bad", "the server's first community stays")
			return true
		}
		h.mu.Lock()
		var inside []*session
		for _, o := range h.sessions {
			if o.comm == comm {
				inside = append(inside, o)
			}
		}
		var chs []int64
		for id, ch := range h.chans {
			if ch.Comm == comm {
				delete(h.chans, id)
				chs = append(chs, id)
			}
		}
		h.forget(comm)
		h.mu.Unlock()
		h.s.music.stopIn(comm)
		h.s.chfiles.dropChannels(chs) // before the store: it reads the channels' rows
		if err := st.deleteCommunity(comm); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		// Nothing of it stays behind for a later community with the same id
		if h.s.mods != nil {
			h.s.mods.dropScope([]string{"ban", "mute", "vmute"}, comm)
			h.s.mods.dropScope([]string{"ch-ban"}, chs...)
		}
		h.s.bus.drop(spaceOf(comm))
		log.Printf("community %d deleted by %s", comm, ss.uid)
		for _, o := range inside {
			h.mu.Lock()
			if h.sessions[o.sid] != o || o.comm != comm {
				// Gone meanwhile (or moved on by itself): not brought back as a ghost
				h.mu.Unlock()
				continue
			}
			payload := h.enterLocked(o, h.firstOther(o.uid, comm))
			payload["t"] = "community"
			o.send(payload)
			h.mu.Unlock()
		}
		ss.send(map[string]any{"t": "communities", "items": h.myCommunities(ss.uid)})
		h.s.voice.syncAll()
	case "role-save":
		h.saveRole(ss, m)
	case "role-del":
		h.mu.Lock()
		comm := ss.comm // read once, under the lock
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

// firstOther: another community of uid's than gone (0: none).
func (h *hub) firstOther(uid string, gone int64) int64 {
	for _, c := range h.myCommunities(uid) {
		if c.ID != gone {
			return c.ID
		}
	}
	return 0
}

// afterRemoval: uid isn't in comm any more (left, banned): its sessions there go elsewhere.
func (h *hub) afterRemoval(comm int64, uid, why string) {
	h.mu.Lock()
	var moved []*session
	for _, o := range h.sessions {
		if o.uid == uid && o.comm == comm && o.api {
			// A bot has no other community to go to
			go o.close()
		} else if o.uid == uid && o.comm == comm {
			payload := h.enterLocked(o, h.firstOther(uid, comm))
			payload["t"] = "community"
			if why != "" {
				payload["why"] = why
			}
			o.send(payload)
			moved = append(moved, o)
		} else if o.uid == uid {
			o.send(map[string]any{"t": "communities", "items": h.myCommunities(uid)})
		}
	}
	h.refreshRoles(comm)
	h.mu.Unlock()
	if len(moved) > 0 {
		h.s.voice.syncAll()
	}
}

// rolesChanged: the roles of comm changed: everyone there gets the list again, and their own.
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
	comm := ss.comm // read once, under the lock (a ban may move this session meanwhile)
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
	log.Printf("community %d: role %d %q saved by %s", comm, r.ID, r.Name, ss.uid)
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
	comm := ss.comm // read once, under the lock (a ban may move this session meanwhile)
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
	log.Printf("community %d: %s's roles %v (by %s)", comm, uid, next, ss.uid)
	h.mu.Lock()
	h.refreshRoles(comm)
	h.mu.Unlock()
	ss.send(map[string]any{"t": "done", "op": t, "uid": uid})
}
