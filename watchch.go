package main

// Streams are watched from the streamer's channel: a person may watch someone's stream (through the SFU or peer to
// peer) only while one of their sessions is in the voice channel the streamer is in. Leaving the channel (moving,
// being moved, disconnecting for good) ends one's watches of streams that aren't in the new channel; a streamer
// moving drops the viewers who aren't in the new one. The server's TV (tv:main) isn't a person's stream: anyone on
// the server may watch it from anywhere.
//
// The live list still names every stream of the space, each with its streamer's channel ("ch"): the apps show
// Watch only for the streams of their own channel. When a streamer moves, its entry is told again with the new
// channel.
//
// A session that drops (a network blip, a reconnect) keeps its watches for watchGrace: a person with no session
// at all counts as still in the channel that long.

import (
	"log"
	"time"
)

// watchGrace: how long a viewer with no session (reconnecting) keeps its watches (hub.watchGrace, for the tests).
const watchGrace = 20 * time.Second

// followDelay: channel changes are looked at this long after one (several at once are taken together).
var followDelay = 150 * time.Millisecond

// channelsOf: the voice channels uid's sessions are in (bots aside); h.mu held.
func (h *hub) channelsOf(uid string) map[int64]bool {
	out := map[int64]bool{}
	for _, o := range h.sessions {
		if o.uid == uid && !o.bot && o.ch != 0 {
			out[o.ch] = true
		}
	}
	return out
}

// streamerChannels: the channel(s) ls's streamer is in: a P2P stream's own session's, an SFU stream's device's (its
// publish named it), else its person's; h.mu held.
func (h *hub) streamerChannels(ls liveStream) map[int64]bool {
	if ls.Mode == "p2p" && ls.SID != 0 {
		if o := h.sessions[ls.SID]; o != nil && o.ch != 0 {
			return map[int64]bool{o.ch: true}
		}
	}
	if ls.Mode != "p2p" && ls.Dev != "" {
		// That device only: the person's other sessions (a phone in the lobby) aren't where the stream is. None
		// (that app reconnecting) is no channel: nothing ends meanwhile, nor does the stream move to another's
		out := map[int64]bool{}
		for _, o := range h.sessions {
			if o.uid == ls.UID && !o.bot && o.dev == ls.Dev && o.ch != 0 {
				out[o.ch] = true
			}
		}
		return out
	}
	return h.channelsOf(ls.UID)
}

// streamChannel: the one channel shown for ls (its streamer's; 0 none); h.mu held.
func (h *hub) streamChannel(ls liveStream) int64 {
	var best int64
	for c := range h.streamerChannels(ls) {
		if best == 0 || c < best {
			best = c
		}
	}
	return best
}

// inStreamChannel: uid is in ls's streamer's channel; h.mu held. A streamer with no session now (reconnecting)
// doesn't end anything (its stream ends on its own if it's gone); with [grace], neither does a viewer that left
// lately (it comes back with its watches).
func (h *hub) inStreamChannel(uid string, ls liveStream, grace bool) bool {
	if ls.Room == tvRoom {
		return true
	}
	sc := h.streamerChannels(ls)
	if len(sc) == 0 {
		return true
	}
	vc := h.channelsOf(uid)
	if len(vc) == 0 {
		at, ok := h.leftAt[uid]
		return grace && ok && time.Since(at) < h.watchGrace
	}
	for c := range vc {
		if sc[c] {
			return true
		}
	}
	return false
}

// inChannelOf: uid is in room's streamer's channel (true for a room not live or the TV).
func (h *hub) inChannelOf(uid, room string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ls, ok := h.live[room]
	return !ok || h.inStreamChannel(uid, ls, false)
}

// channelsChanged: someone's channel (or session) changed; the streams follow a moment later. Any lock may be held.
func (h *hub) channelsChanged() {
	if h.followPending.CompareAndSwap(false, true) {
		time.AfterFunc(followDelay, func() {
			h.followPending.Store(false)
			h.followChannels()
		})
	}
}

// followChannels: each live stream's channel is told again if it moved, and the watches of people no longer in
// their stream's channel end (SFU subscriptions and P2P pairs).
func (h *hub) followChannels() {
	h.mu.Lock()
	for uid, at := range h.leftAt {
		if time.Since(at) > 2*h.watchGrace {
			delete(h.leftAt, uid)
		}
	}
	live := make(map[string]liveStream, len(h.live))
	for room, ls := range h.live {
		if room != tvRoom {
			if ch := h.streamChannel(ls); ch != 0 && ch != ls.Ch {
				ls.Ch = ch
				h.live[room] = ls
				h.toComm(ls.Comm, map[string]any{"t": "live", "on": true, "s": ls})
			}
		}
		live[room] = ls
	}
	h.mu.Unlock()

	// Through the SFU
	h.s.mu.Lock()
	rooms := make([]*room, 0, len(h.s.rooms))
	for _, rm := range h.s.rooms {
		if !rm.local && rm.name != tvRoom {
			rooms = append(rooms, rm)
		}
	}
	h.s.mu.Unlock()
	for _, rm := range rooms {
		ls, ok := live[rm.name]
		if !ok {
			continue
		}
		rm.mu.Lock()
		subs := make([]*subscriber, 0, len(rm.subs))
		for sub := range rm.subs {
			subs = append(subs, sub)
		}
		rm.mu.Unlock()
		for _, sub := range subs {
			h.mu.Lock()
			in := h.inStreamChannel(sub.uid, ls, true)
			h.mu.Unlock()
			if !in {
				log.Printf("[%s] viewer %s isn't in the streamer's channel: its watch ends", rm.name, sub.uid)
				sub.c.send(message{Type: "ended", Why: "channel"})
				rm.removeSub(sub)
				go func(sub *subscriber) { _ = sub.c.ws.Close() }(sub)
			}
		}
	}

	// Peer to peer
	p := h.s.p2p
	if p == nil {
		return
	}
	type pair struct {
		room          string
		viewer, owner int
		ls            liveStream
	}
	var pairs []pair
	p.mu.Lock()
	for name, r := range p.rooms {
		ls, ok := live[name]
		if !ok {
			ls = liveStream{Room: name, UID: r.uid, Mode: "p2p", SID: r.sid}
		}
		for v := range r.viewers {
			pairs = append(pairs, pair{name, v, r.sid, ls})
		}
	}
	p.mu.Unlock()
	var out []pair
	h.mu.Lock()
	for _, x := range pairs {
		if o := h.sessions[x.viewer]; o != nil && !h.inStreamChannel(o.uid, x.ls, true) {
			out = append(out, x)
		}
	}
	h.mu.Unlock()
	if len(out) == 0 {
		return
	}
	p.mu.Lock()
	for _, x := range out {
		if r := p.rooms[x.room]; r != nil && r.sid == x.owner {
			delete(r.viewers, x.viewer)
		}
	}
	p.mu.Unlock()
	h.mu.Lock()
	for _, x := range out {
		log.Printf("[%s] P2P viewer sid %d isn't in the streamer's channel: its copy ends", x.room, x.viewer)
		if o := h.sessions[x.viewer]; o != nil {
			o.send(map[string]any{"t": "p2p-bye", "from": x.owner, "room": x.room, "why": "channel"})
		}
		if o := h.sessions[x.owner]; o != nil {
			o.send(map[string]any{"t": "p2p-bye", "from": x.viewer, "room": x.room, "why": "channel"})
		}
	}
	h.mu.Unlock()
}
