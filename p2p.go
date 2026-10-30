package main

// P2P streams ("P2P only" in the apps, and the fallback of "Server + P2P" when the SFU can't take a stream): the
// picture goes straight from the streamer's app to each viewer's (one copy per viewer, DTLS-SRTP end to end; the
// server's TURN, when set, only relays the encrypted packets for strict NATs). The server carries no media: it only
// passes the signalling on over /v1/ws, between sessions that are allowed to talk about that stream.
//
//	<- {"t":"p2p-live","room":R,"on":true,"title":…}  the streamer: R (its uid, or "<uid>:<suffix>") is live P2P from
//	                                                  this session; again to change the title; "on":false ends it
//	-> {"t":"live","on":true,"s":{"room","uid","title","since","comm","mode":"p2p","sid"}} to the community, as any
//	                                                  stream ("mode" tells the apps to ask "sid" instead of the SFU)
//	<- {"t":"p2p-join","sid":S,"room":R}              a viewer asks streamer S; S gets {"t":"p2p-join","from":V,"uid","name","room"}
//	<- {"t":"p2p-offer","sid":V,"room":R,"sdp":…}      streamer → viewer V (only one that joined)
//	<- {"t":"p2p-answer","sid":S,"room":R,"sdp":…}     viewer → streamer
//	<- {"t":"p2p-ice","sid":X,"room":R,"candidate","mid","mline"}  either way
//	<- {"t":"p2p-bye","sid":X,"room":R,"why":…}        either way ("full": the streamer has as many as it takes)
//
// Each is delivered as the same message with "from" (the sender's session) instead of "sid"; nothing is kept. The
// rules: the same permission as streaming through the SFU (streamComm) and the same visibility as watching
// (mayWatch), a viewer must have joined before anything else passes between the two, at most p2pMaxViewers viewers
// a stream, two live streams a person (SFU and P2P together), size limits and a per-session budget.

import (
	"strings"
	"sync"
	"time"
)

// p2pMaxViewers: the server's ceiling for one P2P stream (a var for the tests); the apps stop earlier, by their
// upload budget.
var p2pMaxViewers = 16

const (
	p2pMaxRooms   = 500      // P2P streams on a server at once (they cost it next to nothing)
	p2pMaxSDP     = 32 << 10 // an offer or answer
	p2pMaxPerPair = 2 * maxCands
	p2pRate       = 20.0  // signalling messages a second per session…
	p2pBurst      = 150.0 // …with bursts (a streamer offering several viewers at once)
	p2pJoinRate   = 1.0   // joins a second per session
	p2pJoinBurst  = 6.0
)

type p2pPair struct{ cands int }

type p2pRoom struct {
	name    string
	sid     int // the streamer's session
	uid     string
	comm    int64
	viewers map[int]*p2pPair // by the viewer's session
}

type p2pBucket struct {
	tokens, joins float64
	at            time.Time
}

type p2pState struct {
	mu      sync.Mutex
	rooms   map[string]*p2pRoom
	buckets map[int]*p2pBucket
}

func newP2P() *p2pState {
	return &p2pState{rooms: map[string]*p2pRoom{}, buckets: map[int]*p2pBucket{}}
}

// take spends one message (and a join when [join]) of sid's budget; p.mu held.
func (p *p2pState) take(sid int, join bool) bool {
	now := time.Now()
	b := p.buckets[sid]
	if b == nil {
		b = &p2pBucket{tokens: p2pBurst, joins: p2pJoinBurst, at: now}
		p.buckets[sid] = b
	}
	dt := now.Sub(b.at).Seconds()
	b.at = now
	b.tokens = min(p2pBurst, b.tokens+dt*p2pRate)
	b.joins = min(p2pJoinBurst, b.joins+dt*p2pJoinRate)
	if b.tokens < 1 || (join && b.joins < 1) {
		return false
	}
	b.tokens--
	if join {
		b.joins--
	}
	return true
}

// p2pOp handles one P2P message from ss.
func (h *hub) p2pOp(ss *session, t string, m map[string]any) {
	if ss.bot || ss.api {
		ss.refuse(t, "bot", "not for bots")
		return
	}
	p := h.s.p2p
	if p == nil {
		ss.refuse(t, "bad", "unknown message")
		return
	}
	p.mu.Lock()
	ok := p.take(ss.sid, t == "p2p-join")
	p.mu.Unlock()
	if !ok {
		ss.refuse(t, "rate", "slow down")
		return
	}
	room := str(m, "room")
	if room == "" || len(room) > 200 {
		ss.refuse(t, "bad", "bad room")
		return
	}
	switch t {
	case "p2p-live":
		on, _ := boolOf(m, "on")
		if on {
			h.p2pStart(ss, room, clip(str(m, "title"), 80))
		} else {
			h.p2pEnd(ss, room)
		}
		return
	case "p2p-join":
		h.p2pJoin(ss, room, int(i64(m, "sid")))
		return
	}
	h.p2pPass(ss, t, room, int(i64(m, "sid")), m)
}

// p2pStart announces room as live P2P from ss (or updates its title).
func (h *hub) p2pStart(ss *session, room, title string) {
	if room != ss.uid && !strings.HasPrefix(room, ss.uid+":") {
		ss.refuse("p2p-live", "bad", "you may only stream as yourself")
		return
	}
	comm, may := h.streamComm(ss.uid)
	h.mu.Lock()
	here := ss.comm
	h.mu.Unlock()
	if !may || comm != here {
		ss.refuse("p2p-live", "role", "not allowed to stream here")
		return
	}
	p := h.s.p2p
	// Two live streams a person, the SFU's and these together (the same room switching over counts once)
	mine := map[string]bool{}
	h.s.mu.Lock()
	for k, rm := range h.s.rooms {
		if k != room && !isCallRoom(k) && (k == ss.uid || strings.HasPrefix(k, ss.uid+":")) {
			rm.mu.Lock()
			if rm.publisher != nil {
				mine[k] = true
			}
			rm.mu.Unlock()
		}
	}
	h.s.mu.Unlock()
	p.mu.Lock()
	for k, r := range p.rooms {
		if k != room && r.uid == ss.uid {
			mine[k] = true
		}
	}
	old := p.rooms[room]
	switch {
	case old != nil && old.sid != ss.sid && h.sessionAlive(old.sid):
		p.mu.Unlock()
		ss.refuse("p2p-live", "taken", "this stream is already live")
		return
	case len(mine) >= 2:
		p.mu.Unlock()
		ss.refuse("p2p-live", "full", "you already have two streams live")
		return
	case old == nil && len(p.rooms) >= p2pMaxRooms:
		p.mu.Unlock()
		ss.refuse("p2p-live", "full", "too many streams on this server")
		return
	}
	var dropped map[int]*p2pPair
	if old != nil && old.sid != ss.sid {
		dropped = old.viewers // left by a session that's gone: its viewers start again
		old = nil
	}
	if old == nil {
		p.rooms[room] = &p2pRoom{name: room, sid: ss.sid, uid: ss.uid, comm: comm, viewers: map[int]*p2pPair{}}
	}
	p.mu.Unlock()
	h.mu.Lock()
	for v := range dropped {
		if o := h.sessions[v]; o != nil {
			o.send(map[string]any{"t": "p2p-bye", "from": ss.sid, "room": room, "why": "ended"})
		}
	}
	since := time.Now().UnixMilli()
	if was, ok := h.live[room]; ok && was.Mode == "p2p" && was.UID == ss.uid {
		since = was.Since
	}
	ls := liveStream{Room: room, UID: ss.uid, Title: title, Since: since, Comm: comm, Mode: "p2p", SID: ss.sid, Ch: ss.ch}
	h.live[room] = ls
	h.toComm(comm, map[string]any{"t": "live", "on": true, "s": ls})
	h.mu.Unlock()
}

// sessionAlive: sid is connected now.
func (h *hub) sessionAlive(sid int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[sid] != nil
}

// p2pEnd ends ss's P2P stream room.
func (h *hub) p2pEnd(ss *session, room string) {
	p := h.s.p2p
	p.mu.Lock()
	r := p.rooms[room]
	if r == nil || r.sid != ss.sid {
		p.mu.Unlock()
		return
	}
	delete(p.rooms, room)
	p.mu.Unlock()
	h.p2pClosed(r, "ended")
}

// p2pClosed tells a room's viewers it ended and takes it off the live list (unless the SFU has it now).
func (h *hub) p2pClosed(r *p2pRoom, why string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ls, ok := h.live[r.name]
	if ok && (ls.Mode != "p2p" || ls.SID != r.sid) {
		why = "moved" // the stream goes on another way (through the SFU now): the viewers follow the live list
	}
	for v := range r.viewers {
		if o := h.sessions[v]; o != nil {
			o.send(map[string]any{"t": "p2p-bye", "from": r.sid, "room": r.name, "why": why})
		}
	}
	if ok && ls.Mode == "p2p" && ls.SID == r.sid {
		delete(h.live, r.name)
		h.toComm(ls.Comm, map[string]any{"t": "live", "on": false, "s": liveStream{Room: r.name}})
	}
}

// p2pJoin: ss asks streamer sid for room.
func (h *hub) p2pJoin(ss *session, room string, sid int) {
	if sid == ss.sid {
		ss.refuse("p2p-join", "bad", "that's your own stream")
		return
	}
	if !h.mayWatch(ss.uid, room) {
		why := "ended"
		h.mu.Lock()
		if ls, ok := h.live[room]; ok && !h.inStreamChannel(ss.uid, ls, false) {
			why = "channel" // live, but in another channel: join it to watch
		}
		h.mu.Unlock()
		ss.send(map[string]any{"t": "p2p-bye", "from": sid, "room": room, "why": why})
		return
	}
	p := h.s.p2p
	p.mu.Lock()
	r := p.rooms[room]
	if r == nil || r.sid != sid {
		p.mu.Unlock()
		ss.send(map[string]any{"t": "p2p-bye", "from": sid, "room": room, "why": "ended"})
		return
	}
	if r.viewers[ss.sid] == nil && len(r.viewers) >= p2pMaxViewers {
		p.mu.Unlock()
		ss.send(map[string]any{"t": "p2p-bye", "from": sid, "room": room, "why": "full"})
		return
	}
	r.viewers[ss.sid] = &p2pPair{} // joining again starts the pair over
	p.mu.Unlock()
	h.mu.Lock()
	o := h.sessions[sid]
	h.mu.Unlock()
	if o == nil {
		return
	}
	o.send(map[string]any{"t": "p2p-join", "from": ss.sid, "uid": ss.uid, "name": ss.name, "room": room})
}

// p2pPass forwards an offer, answer, candidate or bye between a streamer and a viewer that joined.
func (h *hub) p2pPass(ss *session, t, room string, to int, m map[string]any) {
	out := map[string]any{"t": t, "from": ss.sid, "room": room}
	switch t {
	case "p2p-offer", "p2p-answer":
		sdp := str(m, "sdp")
		if sdp == "" || len(sdp) > p2pMaxSDP {
			ss.refuse(t, "bad", "bad description")
			return
		}
		out["sdp"] = sdp
	case "p2p-ice":
		c := str(m, "candidate")
		if c == "" || len(c) > maxCandLen {
			return
		}
		out["candidate"] = c
		if mid := str(m, "mid"); mid != "" && len(mid) <= 32 {
			out["mid"] = mid
		}
		if _, ok := m["mline"]; ok {
			out["mline"] = min(max(i64(m, "mline"), 0), 64)
		}
	case "p2p-bye":
		if why := str(m, "why"); why != "" {
			out["why"] = clip(why, 32)
		}
	}
	p := h.s.p2p
	p.mu.Lock()
	r := p.rooms[room]
	var pair *p2pPair
	if r != nil {
		switch {
		case ss.sid == r.sid && t != "p2p-answer": // the streamer to one of its viewers
			pair = r.viewers[to]
		case to == r.sid && t != "p2p-offer": // a viewer to its streamer
			pair = r.viewers[ss.sid]
		}
	}
	if pair == nil {
		p.mu.Unlock()
		if t != "p2p-bye" && t != "p2p-ice" {
			ss.send(map[string]any{"t": "p2p-bye", "from": to, "room": room, "why": "gone"})
		}
		return
	}
	if t == "p2p-ice" {
		if pair.cands++; pair.cands > p2pMaxPerPair {
			p.mu.Unlock()
			return
		}
	}
	if t == "p2p-bye" {
		if ss.sid == r.sid {
			delete(r.viewers, to)
		} else {
			delete(r.viewers, ss.sid)
		}
	}
	p.mu.Unlock()
	h.mu.Lock()
	o := h.sessions[to]
	h.mu.Unlock()
	if o != nil {
		o.send(out)
	}
}

// p2pGone: ss left the server; its P2P streams end and the streamers it watched are told.
func (h *hub) p2pGone(ss *session) {
	p := h.s.p2p
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.buckets, ss.sid)
	var ended []*p2pRoom
	type note struct {
		to   int
		room string
	}
	var tell []note
	for k, r := range p.rooms {
		if r.sid == ss.sid {
			delete(p.rooms, k)
			ended = append(ended, r)
			continue
		}
		if r.viewers[ss.sid] != nil {
			delete(r.viewers, ss.sid)
			tell = append(tell, note{r.sid, r.name})
		}
	}
	p.mu.Unlock()
	for _, r := range ended {
		h.p2pClosed(r, "ended")
	}
	if len(tell) > 0 {
		h.mu.Lock()
		for _, n := range tell {
			if o := h.sessions[n.to]; o != nil {
				o.send(map[string]any{"t": "p2p-bye", "from": ss.sid, "room": n.room, "why": "left"})
			}
		}
		h.mu.Unlock()
	}
}

// p2pViewers: how many viewers room has peer to peer now (for /streams/status).
func (s *server) p2pViewers(room string) (int, bool) {
	if s.p2p == nil {
		return 0, false
	}
	s.p2p.mu.Lock()
	defer s.p2p.mu.Unlock()
	r := s.p2p.rooms[room]
	if r == nil {
		return 0, false
	}
	return len(r.viewers), true
}

// p2pCount: uid's P2P streams other than room (for the two-a-person rule of the SFU's openRoom).
func (s *server) p2pCount(uid, room string) int {
	if s.p2p == nil {
		return 0
	}
	s.p2p.mu.Lock()
	defer s.p2p.mu.Unlock()
	n := 0
	for k, r := range s.p2p.rooms {
		if k != room && r.uid == uid {
			n++
		}
	}
	return n
}
