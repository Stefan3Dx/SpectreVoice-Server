package main

// Streams (screen and camera) through the server: the streamer sends its picture once; the server forwards it to
// every viewer. A room is named by its streamer's uid (and an optional ":suffix"), so only that identity may
// publish it; anyone on the server may watch.
//
// Signalling is JSON over a WebSocket at /streams?token=…:
//
//	{"type":"publish","room":R,"sdp":OFFER}  -> {"type":"answer","sdp":…}
//	{"type":"subscribe","room":R,"dev":D}    -> {"type":"offer","sdp":…} ; reply {"type":"answer","sdp":…}
//	                                            (the offer once the stream has a track: a viewer that comes before
//	                                            the first frame waits for it; a new watch of the same person and
//	                                            device "dev", optional, replaces the old one)
//	{"type":"candidate","candidate":C,"mid":M,"mline":N} both ways
//	{"type":"layer","q":"h"|"m"|"l"|"auto"}   a viewer's layer choice (simulcast streams, see simulcast.go)
//	server -> {"type":"layer","layer":L,"q":Q,"layers":[…]} the layer sent to the viewer
//	server -> {"type":"ended"} when the room's streamer leaves, {"type":"error","message":…}
//
// GET /streams/status?room=R&token=… answers {"live":bool,"viewers":n,"bytes":b,"sfuBytes":t,"mode":…,"p2p":n}: live and
// viewers through the SFU, the media bytes the SFU got for R and for every stream since it started, and for a P2P
// stream (p2p.go) "mode":"p2p" with its peer-to-peer viewer count.

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type message struct {
	Type      string  `json:"type"`
	Room      string  `json:"room,omitempty"`
	SDP       string  `json:"sdp,omitempty"`
	Candidate string  `json:"candidate,omitempty"`
	Mid       *string `json:"mid,omitempty"`
	MLine     *uint16 `json:"mline,omitempty"`
	Message   string  `json:"message,omitempty"`
	Live      *bool   `json:"live,omitempty"`
	Viewers   *int    `json:"viewers,omitempty"`
	Title     string  `json:"title,omitempty"`
	Dev       string  `json:"dev,omitempty"` // subscribe: the app's device (optional): its new watch replaces its old one
	// Simulcast: the layer a viewer asks for ("h", "m", "l", "auto"), the one sent, and the ones the streamer has
	Q      string   `json:"q,omitempty"`
	Layer  string   `json:"layer,omitempty"`
	Layers []string `json:"layers,omitempty"`
	// /streams/status: media bytes the SFU got (this room, all), and a P2P stream's mode and viewers
	Bytes    *int64 `json:"bytes,omitempty"`
	SFUBytes *int64 `json:"sfuBytes,omitempty"`
	Mode     string `json:"mode,omitempty"`
	P2P      *int   `json:"p2p,omitempty"`
}

// statStreamIn: media bytes the SFU got from streamers (all rooms, since it started).
var statStreamIn atomic.Int64

// A WebSocket with one writer at a time.
type conn struct {
	ws   *websocket.Conn
	mu   sync.Mutex
	gone atomic.Bool // its handler has ended: the connection is closed
}

func (c *conn) send(m message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = c.ws.WriteJSON(m)
}

type subscriber struct {
	uid     string
	dev     string // the app's device, when it says ("": unknown)
	pc      *webrtc.PeerConnection
	c       *conn
	mu      sync.Mutex
	pending bool        // an offer is out, waiting for the answer
	again   bool        // tracks changed meanwhile: offer again after it
	offered atomic.Bool // its first offer went out (a viewer that came before the first frame waits for it)
	// Simulcast: what the viewer asked ("auto" or a rid), its layer switcher, the auto rule's memory, and what its
	// RTCP said since the last tick
	sw     atomic.Pointer[switcher]
	pick   picker
	linkMu sync.Mutex // guards asked, pick, link and sent
	asked  string
	link   linkSample
	sent   string // the layer last reported to the viewer
}

// wanted: what the viewer asked for ("auto" or a rid).
func (sub *subscriber) wanted() string {
	sub.linkMu.Lock()
	defer sub.linkMu.Unlock()
	return sub.asked
}

type room struct {
	name      string
	mu        sync.Mutex
	publisher *webrtc.PeerConnection
	pubConn   *conn
	tracks    []*webrtc.TrackLocalStaticRTP
	ssrcs     []webrtc.SSRC // the publisher's video SSRCs, for key frame requests
	subs      map[*subscriber]bool
	lastKey   time.Time                 // the last key frame asked of the streamer (asked at most twice a second)
	ended     bool                      // endRoom ran: no new viewers
	sim       *simulcast                // the layered picture of a simulcast streamer (nil: one picture, forwarded as is)
	keyAt     map[webrtc.SSRC]time.Time // per layer: the last key frame asked
	stop      chan struct{}             // closed when the room ends (stops the auto loop)
	bytes     atomic.Int64              // media the streamer sent us
	local     bool                      // the server's own TV (tv.go): no streamer, ffmpeg sends a key frame every 2 s
	localLive atomic.Bool               // the TV's picture is coming
	onViewers func()                    // told when a viewer comes or goes (the TV: who's watching, tvparty.go)
}

func (s *server) handleStreamStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	name := r.URL.Query().Get("room")
	s.mu.Lock()
	rm := s.rooms[name]
	s.mu.Unlock()
	live, viewers, bytes := false, 0, int64(0)
	if rm != nil {
		rm.mu.Lock()
		live = (rm.publisher != nil || (rm.local && rm.localLive.Load())) && (len(rm.tracks) > 0 || rm.sim != nil)
		viewers = len(rm.subs)
		rm.mu.Unlock()
		bytes = rm.bytes.Load()
	}
	all := statStreamIn.Load()
	out := message{Type: "status", Room: name, Live: &live, Viewers: &viewers, Bytes: &bytes, SFUBytes: &all}
	if n, ok := s.p2pViewers(name); ok {
		out.Mode, out.P2P = "p2p", &n
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *server) handleStreams(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.tokenUID(r)
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	// A few stream connections per person (each one a connection of ours, with its UDP ports)
	if !streamConns.take(uid) {
		http.Error(w, "too many stream connections", http.StatusTooManyRequests)
		return
	}
	defer streamConns.give(uid)
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	c := &conn{ws: ws}
	tokens, refill, cands := float64(streamBurst), time.Now(), 0
	var pc *webrtc.PeerConnection
	var sub *subscriber
	var rm *room
	defer func() {
		c.gone.Store(true)
		_ = ws.Close()
		if rm != nil {
			if sub != nil {
				rm.removeSub(sub)
			} else if pc != nil {
				s.endRoom(rm, pc)
			}
		}
		if pc != nil {
			_ = pc.Close()
		}
	}()
	// Keepalive: the reverse proxy shouldn't drop a quiet connection. A connection that stops answering is
	// closed, so its stream ends and the streamer can come back (a live one can't be taken over)
	_ = ws.SetReadDeadline(time.Now().Add(busReadTimeout))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(busReadTimeout)) })
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for range t.C {
			c.mu.Lock()
			err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			c.mu.Unlock()
			if err != nil {
				_ = ws.Close()
				return
			}
		}
	}()

	for {
		var m message
		if err := ws.ReadJSON(&m); err != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(busReadTimeout))
		// Signalling is a handful of messages: a flood closes the connection
		now := time.Now()
		tokens = min(streamBurst, tokens+now.Sub(refill).Seconds()*streamRate)
		refill = now
		if tokens < 1 {
			return
		}
		tokens--
		switch m.Type {
		case "publish":
			if pc != nil {
				c.send(message{Type: "error", Message: "already publishing or watching"})
				continue
			}
			if (m.Room != uid && !strings.HasPrefix(m.Room, uid+":")) || strings.HasPrefix(m.Room, tvRoomPrefix) {
				c.send(message{Type: "error", Message: "you may only stream as yourself"})
				return
			}
			var comm int64
			if s.hub != nil {
				var may bool
				if comm, may = s.hub.streamComm(uid); !may {
					c.send(message{Type: "error", Message: "not allowed to stream here"})
					return
				}
			}
			if n := mlines(m.SDP); n == 0 || n > maxStreamTracks {
				c.send(message{Type: "error", Message: "a stream has a picture and its sound"})
				return
			}
			rm, err = s.openRoom(m.Room, uid, c)
			if err != nil {
				rm = nil
				c.send(message{Type: "error", Message: err.Error()})
				return
			}
			pc, err = s.publish(rm, c, m.SDP)
			if err != nil {
				// The room was opened for this stream: it mustn't stay behind, taking a place and the name
				s.endRoom(rm, nil)
				rm = nil
				c.send(message{Type: "error", Message: err.Error()})
				return
			}
			if s.hub != nil {
				s.hub.setLive(rm.name, uid, clip(m.Title, 80), comm, true)
			}
		case "subscribe":
			if pc != nil {
				c.send(message{Type: "error", Message: "already publishing or watching"})
				continue
			}
			s.mu.Lock()
			rm = s.rooms[m.Room]
			s.mu.Unlock()
			// The server's TV starts its channel when someone comes to watch
			if rm == nil && m.Room == tvRoom && s.hub != nil && s.hub.mayWatch(uid, m.Room) {
				rm = s.tv.ensure()
			}
			if rm == nil || (s.hub != nil && !s.hub.mayWatch(uid, m.Room)) {
				c.send(message{Type: "ended"})
				return
			}
			sub, err = s.subscribe(rm, c, uid, clip(m.Dev, 64))
			if err != nil {
				c.send(message{Type: "error", Message: err.Error()})
				return
			}
			pc = sub.pc
		case "answer":
			if sub == nil {
				continue
			}
			if err := sub.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
				log.Printf("[%s] viewer answer: %v", rm.name, err)
				return
			}
			sub.mu.Lock()
			sub.pending = false
			again := sub.again
			sub.again = false
			sub.mu.Unlock()
			if again {
				rm.offerTo(sub)
			}
		case "layer":
			// Simulcast viewers: the layer wanted (a bad one is ignored)
			if sub != nil && validLayer(m.Q) {
				rm.chooseLayer(sub, m.Q)
			}
		case "candidate":
			if pc == nil || m.Candidate == "" || len(m.Candidate) > maxCandLen {
				continue
			}
			if cands++; cands > maxCands {
				continue
			}
			_ = pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: m.Candidate, SDPMid: m.Mid, SDPMLineIndex: m.MLine})
		}
	}
}

// openRoom makes the room c will stream to.
func (s *server) openRoom(name, uid string, c *conn) (*room, error) {
	if name == "" || len(name) > 200 {
		return nil, errors.New("bad room name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Two streams per person at most (a screen and a camera): the server's few aren't one person's
	mine := 0
	for k := range s.rooms {
		if k != name && (k == uid || strings.HasPrefix(k, uid+":")) {
			mine++
		}
	}
	mine += s.p2pCount(uid, name) // and its peer-to-peer ones (p2p.go)
	if mine >= 2 {
		return nil, errors.New("you already have two streams live")
	}
	if old := s.rooms[name]; old != nil {
		// Every app has the key, so a room name is no proof of who streams: while the streamer is still
		// connected, nobody else takes the room over. Once its connection is gone (a restart), it's let go.
		old.mu.Lock()
		owner := old.pubConn
		old.mu.Unlock()
		if owner != nil && !owner.gone.Load() {
			return nil, errors.New("this stream is already live")
		}
		go s.endRoom(old, nil)
	} else if len(s.rooms) >= s.cfg.maxRooms {
		return nil, errors.New("too many streams on this server")
	}
	rm := &room{name: name, subs: map[*subscriber]bool{}, pubConn: c, keyAt: map[webrtc.SSRC]time.Time{}, stop: make(chan struct{})}
	s.rooms[name] = rm
	return rm, nil
}

// endRoom closes a room when its publisher leaves ([only]: only if that's still its publisher).
func (s *server) endRoom(rm *room, only *webrtc.PeerConnection) {
	rm.mu.Lock()
	if only != nil && rm.publisher != only {
		rm.mu.Unlock()
		return
	}
	subs := rm.subs
	rm.subs = map[*subscriber]bool{}
	rm.ended = true
	pub := rm.publisher
	rm.publisher = nil
	rm.tracks = nil
	if rm.stop != nil {
		close(rm.stop)
		rm.stop = nil
	}
	rm.mu.Unlock()
	s.mu.Lock()
	gone := s.rooms[rm.name] == rm
	if gone {
		delete(s.rooms, rm.name)
	}
	s.mu.Unlock()
	if gone && s.hub != nil {
		s.hub.setLive(rm.name, "", "", 0, false)
	}
	for sub := range subs {
		// Each viewer on its own: a stuck one can't hold up the rest
		go func(sub *subscriber) {
			sub.c.send(message{Type: "ended"})
			_ = sub.pc.Close()
		}(sub)
	}
	if pub != nil && pub != only {
		_ = pub.Close()
	}
	if rm.onViewers != nil && len(subs) > 0 {
		rm.onViewers()
	}
	log.Printf("[%s] ended", rm.name)
}

func (s *server) newPC(c *conn) (*webrtc.PeerConnection, error) {
	pc, err := s.webrtcAPI().NewPeerConnection(webrtc.Configuration{ICEServers: s.ice})
	if err != nil {
		return nil, err
	}
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		ci := cand.ToJSON()
		c.send(message{Type: "candidate", Candidate: ci.Candidate, Mid: ci.SDPMid, MLine: ci.SDPMLineIndex})
	})
	return pc, nil
}

func (s *server) publish(rm *room, c *conn, offer string) (*webrtc.PeerConnection, error) {
	pc, err := s.newPC(c)
	if err != nil {
		return nil, err
	}
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if remote.RID() != "" && remote.Kind() == webrtc.RTPCodecTypeVideo {
			rm.readLayer(remote)
			return
		}
		local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, remote.ID(), "spectre")
		if err != nil {
			log.Printf("[%s] track: %v", rm.name, err)
			return
		}
		rm.mu.Lock()
		if len(rm.tracks) >= maxStreamTracks {
			rm.mu.Unlock()
			return
		}
		rm.tracks = append(rm.tracks, local)
		if remote.Kind() == webrtc.RTPCodecTypeVideo {
			rm.ssrcs = append(rm.ssrcs, remote.SSRC())
		}
		subs := make([]*subscriber, 0, len(rm.subs))
		for sub := range rm.subs {
			subs = append(subs, sub)
		}
		rm.mu.Unlock()
		log.Printf("[%s] %s track %s", rm.name, remote.Kind(), remote.Codec().MimeType)
		for _, sub := range subs {
			// Like the tracks a viewer gets when joining: its key frame requests go on to the streamer
			if sender, err := sub.pc.AddTrack(local); err == nil {
				go rm.forwardRTCP(sub, sender)
				go rm.offerTo(sub) // a viewer that's slow to take it doesn't hold up the stream
			}
		}
		buf := make([]byte, 1500)
		var lastErr time.Time
		for {
			n, _, err := remote.Read(buf)
			if err != nil {
				return
			}
			rm.bytes.Add(int64(n))
			statStreamIn.Add(int64(n))
			// One viewer's error isn't everyone's: keep forwarding
			if _, err := local.Write(buf[:n]); err != nil && !errors.Is(err, io.ErrClosedPipe) && time.Since(lastErr) > 10*time.Second {
				lastErr = time.Now()
				log.Printf("[%s] forward: %v", rm.name, err)
			}
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.endRoom(rm, pc)
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		_ = pc.Close()
		return nil, err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return nil, err
	}
	rm.mu.Lock()
	rm.publisher = pc
	rm.pubConn = c
	rm.mu.Unlock()
	c.send(message{Type: "answer", SDP: pc.LocalDescription().SDP})
	log.Printf("[%s] streamer in", rm.name)
	return pc, nil
}

// sameWatcher: [o] is the same person's watch from the same device as (uid, dev), which a new one replaces (one
// watch per person and device; an app that doesn't name its device has one per person).
func sameWatcher(o *subscriber, uid, dev string) bool {
	return uid != "" && o.uid == uid && (dev == "" || o.dev == "" || o.dev == dev)
}

// subscribe makes [uid]'s viewer of the room. Its own earlier watch from the same device (a connection that
// dropped and is still open here, for up to 75 s) is replaced. A viewer that comes before the streamer's first
// track (the stream is announced at the publish) waits in the room and is offered the tracks as they come.
func (s *server) subscribe(rm *room, c *conn, uid, dev string) (*subscriber, error) {
	rm.mu.Lock()
	n := 0
	for o := range rm.subs {
		if !sameWatcher(o, uid, dev) {
			n++
		}
	}
	if n >= s.cfg.maxSubs {
		rm.mu.Unlock()
		return nil, errors.New("this stream has as many viewers as it may")
	}
	tracks := append([]*webrtc.TrackLocalStaticRTP(nil), rm.tracks...)
	sim := rm.sim
	rm.mu.Unlock()
	pc, err := s.newPC(c)
	if err != nil {
		return nil, err
	}
	sub := &subscriber{pc: pc, c: c, uid: uid, dev: dev, asked: "auto"}
	for _, t := range tracks {
		sender, err := pc.AddTrack(t)
		if err != nil {
			_ = pc.Close()
			return nil, err
		}
		go rm.forwardRTCP(sub, sender)
	}
	if sim != nil {
		if err := rm.addSwitcher(sub, sim); err != nil {
			_ = pc.Close()
			return nil, err
		}
	}
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateConnected {
			rm.keyFrame()
		}
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			rm.removeSub(sub)
		}
	})
	if subscribeGap != nil {
		subscribeGap(rm)
	}
	// The room may have ended meanwhile (its streamer left): not joined to a dead room
	rm.mu.Lock()
	if rm.ended {
		rm.mu.Unlock()
		_ = pc.Close()
		return nil, errors.New("the stream ended")
	}
	// Tracks (or the first simulcast layer) that came while this viewer was being made weren't given to it by
	// OnTrack/readLayer (it wasn't among the viewers yet): it takes them here, once
	var late []*webrtc.TrackLocalStaticRTP
	for _, t := range rm.tracks {
		if !slices.Contains(tracks, t) {
			late = append(late, t)
		}
	}
	var lateSim *simulcast
	if sim == nil {
		lateSim = rm.sim
	}
	var replaced []*subscriber
	for o := range rm.subs {
		if sameWatcher(o, uid, dev) {
			replaced = append(replaced, o)
			delete(rm.subs, o)
		}
	}
	rm.subs[sub] = true
	n = len(rm.subs)
	roomSim := rm.sim
	rm.mu.Unlock()
	if rm.onViewers != nil {
		rm.onViewers()
	}
	for _, o := range replaced {
		rm.dropReplaced(o, roomSim)
	}
	for _, t := range late {
		if sender, err := pc.AddTrack(t); err == nil {
			go rm.forwardRTCP(sub, sender)
		}
	}
	if lateSim != nil {
		_ = rm.addSwitcher(sub, lateSim)
	}
	if len(tracks) == 0 && len(late) == 0 && sim == nil && lateSim == nil {
		// Nothing to offer yet: an offer without media has no ICE in it, and the viewer's answer would be refused.
		// OnTrack / readLayer give it the tracks (it's in rm.subs now) and offer them
		log.Printf("[%s] viewer in (%d), waiting for the stream's first frame", rm.name, n)
		return sub, nil
	}
	log.Printf("[%s] viewer in (%d)", rm.name, n)
	rm.offerTo(sub)
	return sub, nil
}

// dropReplaced ends a viewer that a newer watch of the same person and device replaced (already out of rm.subs).
func (rm *room) dropReplaced(o *subscriber, sim *simulcast) {
	if sim != nil {
		sim.mu.Lock()
		delete(sim.viewers, o)
		sim.mu.Unlock()
	}
	log.Printf("[%s] viewer %s replaced by its new watch", rm.name, o.uid)
	go func() {
		// Told, in case it's still there (not "already watching": an app would subscribe again on that)
		o.c.send(message{Type: "error", Message: "you're watching this stream on another connection now"})
		_ = o.pc.Close()
		_ = o.c.ws.Close()
	}()
}

// viewerConnectWait: a viewer must be connected this long after its first offer, else it's let go.
const viewerConnectWait = 30 * time.Second

// subscribeGap: tests run it while a viewer is being made (a track arriving just then).
var subscribeGap func(*room)

// offerTo sends a viewer a (new) offer, or remembers to once the one out is answered.
func (rm *room) offerTo(sub *subscriber) {
	sub.mu.Lock()
	if sub.pending {
		sub.again = true
		sub.mu.Unlock()
		return
	}
	sub.pending = true
	sub.mu.Unlock()
	if sub.offered.CompareAndSwap(false, true) {
		// A viewer that never connects doesn't keep its connection (and ports) for ever
		time.AfterFunc(viewerConnectWait, func() {
			if sub.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
				rm.removeSub(sub)
				_ = sub.pc.Close()
			}
		})
	}
	offer, err := sub.pc.CreateOffer(nil)
	if err == nil {
		err = sub.pc.SetLocalDescription(offer)
	}
	if err != nil {
		log.Printf("[%s] offer: %v", rm.name, err)
		// Not stuck waiting for an answer that will never come
		sub.mu.Lock()
		sub.pending = false
		sub.mu.Unlock()
		return
	}
	sub.c.send(message{Type: "offer", SDP: sub.pc.LocalDescription().SDP})
}

// forwardRTCP passes a viewer's key frame requests on to the streamer and keeps what its reports say about the
// link (loss, bandwidth, NACKs) for the layer choice.
func (rm *room) forwardRTCP(sub *subscriber, sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p := p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				rm.keyFrameFor(sub)
			case *rtcp.ReceiverReport:
				if sub.sw.Load() != nil {
					for _, r := range p.Reports {
						sub.linkMu.Lock()
						sub.link.lossPct = float64(r.FractionLost) * 100 / 256
						sub.linkMu.Unlock()
					}
				}
			case *rtcp.ReceiverEstimatedMaximumBitrate:
				if sub.sw.Load() != nil {
					sub.linkMu.Lock()
					sub.link.rembKbps = int(p.Bitrate / 1000)
					sub.linkMu.Unlock()
				}
			case *rtcp.TransportLayerNack:
				if sub.sw.Load() != nil {
					n := 0
					for _, pair := range p.Nacks {
						n += len(pair.PacketList())
					}
					sub.linkMu.Lock()
					sub.link.nacks += n
					sub.linkMu.Unlock()
				}
			}
		}
	}
}

// keyFrameFor asks the streamer for a key frame where [sub] needs one: its layer, or the whole picture.
func (rm *room) keyFrameFor(sub *subscriber) {
	if sw := sub.sw.Load(); sub != nil && sw != nil {
		cur, want := sw.current()
		if want != "" && want != cur {
			cur = want
		}
		if l := rm.layerOf(cur); l != nil {
			rm.keyFrameOn(l.ssrc)
			return
		}
	}
	rm.keyFrame()
}

func (rm *room) keyFrame() {
	rm.mu.Lock()
	pub := rm.publisher
	ssrcs := append([]webrtc.SSRC(nil), rm.ssrcs...)
	// Many viewers joining at once each ask for one: the streamer's encoder gets one key frame for all
	if time.Since(rm.lastKey) < 500*time.Millisecond {
		rm.mu.Unlock()
		return
	}
	rm.lastKey = time.Now()
	rm.mu.Unlock()
	if pub == nil {
		return
	}
	for _, ssrc := range ssrcs {
		_ = pub.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}})
	}
}

// keyFrameOn asks the streamer for a key frame of one layer (at most twice a second per layer).
func (rm *room) keyFrameOn(ssrc webrtc.SSRC) {
	rm.mu.Lock()
	pub := rm.publisher
	if time.Since(rm.keyAt[ssrc]) < 500*time.Millisecond {
		rm.mu.Unlock()
		return
	}
	rm.keyAt[ssrc] = time.Now()
	rm.mu.Unlock()
	if pub != nil {
		_ = pub.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}})
	}
}

// ====== Simulcast in the room ======

// layerOf: the room's layer [rid] (nil without simulcast or that layer).
func (rm *room) layerOf(rid string) *layer {
	rm.mu.Lock()
	sim := rm.sim
	rm.mu.Unlock()
	if sim == nil {
		return nil
	}
	sim.mu.RLock()
	defer sim.mu.RUnlock()
	return sim.layers[rid]
}

// readLayer takes one layer of a simulcast streamer: the first one makes the room layered (every viewer gets its
// own switched track), then its packets go to each viewer's switcher.
func (rm *room) readLayer(remote *webrtc.TrackRemote) {
	rid := remote.RID()
	rm.mu.Lock()
	if rm.ended {
		// A layer of a room that already ended: nothing to forward it to (and no auto loop to start, it would
		// never stop)
		rm.mu.Unlock()
		return
	}
	sim := rm.sim
	first := sim == nil
	if first {
		sim = &simulcast{codec: remote.Codec().RTPCodecCapability, id: remote.ID(), layers: map[string]*layer{}, viewers: map[*subscriber]*switcher{}}
		rm.sim = sim
	}
	sim.mu.Lock()
	if len(sim.layers) >= maxStreamTracks {
		sim.mu.Unlock()
		rm.mu.Unlock()
		return
	}
	l := &layer{rid: rid, ssrc: remote.SSRC()}
	l.last.Store(time.Now().UnixNano())
	sim.layers[rid] = l
	sim.mu.Unlock()
	rm.ssrcs = append(rm.ssrcs, remote.SSRC())
	subs := make([]*subscriber, 0, len(rm.subs))
	for sub := range rm.subs {
		subs = append(subs, sub)
	}
	stop := rm.stop
	rm.mu.Unlock()
	log.Printf("[%s] video layer %q %s", rm.name, rid, remote.Codec().MimeType)
	if first {
		for _, sub := range subs {
			if err := rm.addSwitcher(sub, sim); err == nil {
				go rm.offerTo(sub)
			}
		}
		go rm.autoLoop(sim, stop)
	} else {
		// Viewers that waited for this layer (asked before it came) get it at its first key frame
		for _, sub := range subs {
			rm.tellLayer(sub, false)
		}
	}
	for {
		p, _, err := remote.ReadRTP()
		if err != nil {
			return
		}
		now := time.Now()
		l.bytes.Add(int64(len(p.Payload) + 12))
		rm.bytes.Add(int64(len(p.Payload) + 12))
		statStreamIn.Add(int64(len(p.Payload) + 12))
		l.last.Store(now.UnixNano())
		sim.mu.RLock()
		views := make([]*subscriber, 0, len(sim.viewers))
		for sub := range sim.viewers {
			views = append(views, sub)
		}
		sim.mu.RUnlock()
		for i, sub := range views {
			pkt := p
			if i < len(views)-1 {
				cp := *p
				pkt = &cp
			}
			if sub.sw.Load().push(rid, pkt) {
				rm.tellLayer(sub, true)
			}
		}
	}
}

// addSwitcher gives a viewer its own video track, fed by one layer at a time.
func (rm *room) addSwitcher(sub *subscriber, sim *simulcast) error {
	track, err := webrtc.NewTrackLocalStaticRTP(sim.codec, sim.id, "spectre")
	if err != nil {
		return err
	}
	sender, err := sub.pc.AddTrack(track)
	if err != nil {
		return err
	}
	sw := newSwitcher(track, sim.codec.MimeType)
	sub.sw.Store(sw)
	go rm.forwardRTCP(sub, sender)
	sim.mu.Lock()
	sim.viewers[sub] = sw
	sim.mu.Unlock()
	rm.chooseLayer(sub, sub.wanted())
	return nil
}

// chooseLayer applies a viewer's request: a rid (the nearest sending one), or auto (the best, then the rule).
func (rm *room) chooseLayer(sub *subscriber, q string) {
	rm.mu.Lock()
	sim := rm.sim
	rm.mu.Unlock()
	sub.linkMu.Lock()
	sub.asked = q
	if q == "auto" {
		sub.pick = picker{}
	}
	sub.linkMu.Unlock()
	sw := sub.sw.Load()
	if sim == nil || sw == nil {
		return
	}
	// Auto starts from the best layer and steps down when the link can't take it
	now := time.Now()
	want := "h"
	if q != "auto" {
		want = q
	}
	rm.switchTo(sub, sim.nearest(want, now))
}

// switchTo makes a viewer's switcher move to [rid] (a key frame of it is asked of the streamer).
func (rm *room) switchTo(sub *subscriber, rid string) {
	sw := sub.sw.Load()
	if rid == "" || sw == nil {
		return
	}
	if sw.choose(rid) {
		if l := rm.layerOf(rid); l != nil {
			rm.keyFrameOn(l.ssrc)
		}
	}
	rm.tellLayer(sub, false)
}

// tellLayer sends the viewer its layer status when it changed ([force]: even if not).
func (rm *room) tellLayer(sub *subscriber, force bool) {
	rm.mu.Lock()
	sim := rm.sim
	rm.mu.Unlock()
	sw := sub.sw.Load()
	if sim == nil || sw == nil {
		return
	}
	cur, _ := sw.current()
	sub.linkMu.Lock()
	asked := sub.asked
	changed := sub.sent != cur+"/"+asked
	sub.sent = cur + "/" + asked
	sub.linkMu.Unlock()
	if !changed && !force {
		return
	}
	sub.c.send(message{Type: "layer", Layer: cur, Q: asked, Layers: sim.rids()})
}

// autoLoop, once a second: measures each layer's rate and moves the auto viewers by what their links say.
func (rm *room) autoLoop(sim *simulcast, stop chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			rm.autoTick(sim, now, now.Sub(last).Seconds())
			last = now
		}
	}
}

// autoTick: one second of autoLoop (dt: the seconds since the last).
func (rm *room) autoTick(sim *simulcast, now time.Time, dt float64) {
	var infos []layerInfo
	for _, l := range sim.ordered() {
		b := l.bytes.Swap(0)
		if dt > 0 {
			k := int64(float64(b) * 8 / 1000 / dt)
			old := l.kbps.Load()
			if old > 0 {
				k = (old + k) / 2
			}
			l.kbps.Store(k)
		}
		if l.alive(now) {
			infos = append(infos, layerInfo{rid: l.rid, kbps: int(l.kbps.Load())})
		}
	}
	sim.mu.RLock()
	views := make([]*subscriber, 0, len(sim.viewers))
	for sub := range sim.viewers {
		views = append(views, sub)
	}
	sim.mu.RUnlock()
	for _, sub := range views {
		sub.linkMu.Lock()
		s := sub.link
		asked := sub.asked
		sub.link.nacks = 0
		sub.linkMu.Unlock()
		sw := sub.sw.Load()
		if asked != "auto" {
			// A layer asked for that wasn't sending yet: taken as soon as it is
			if cur, _ := sw.current(); cur != asked {
				rm.switchTo(sub, sim.nearest(asked, now))
			}
			continue
		}
		cur, want := sw.current()
		if want != "" && want != cur {
			if sim.sending(want, now) {
				continue // a switch is on its way: judge it once it's done
			}
			// The layer it waits for stopped (the encoder dropped it, short of bandwidth or CPU) before its key
			// frame came: back to what is sending, or a viewer that has nothing yet would stay black for good
			target := cur
			if !sim.sending(cur, now) {
				target = sim.nearest(want, now)
			}
			rm.switchTo(sub, target)
			continue
		}
		sub.linkMu.Lock()
		next := sub.pick.step(cur, infos, s, now)
		sub.linkMu.Unlock()
		if next != cur {
			log.Printf("[%s] viewer %s: layer %s → %s (loss %.0f%%, est %d kbit/s, nacks %d)", rm.name, sub.uid, cur, next, s.lossPct, s.rembKbps, s.nacks)
			rm.switchTo(sub, next)
		}
	}
}

func (rm *room) removeSub(sub *subscriber) {
	rm.mu.Lock()
	_, was := rm.subs[sub]
	delete(rm.subs, sub)
	n := len(rm.subs)
	sim := rm.sim
	rm.mu.Unlock()
	if sim != nil {
		sim.mu.Lock()
		delete(sim.viewers, sub)
		sim.mu.Unlock()
	}
	if was {
		_ = sub.pc.Close()
		log.Printf("[%s] viewer out (%d)", rm.name, n)
		if rm.onViewers != nil {
			rm.onViewers()
		}
	}
}

// Limits on the streams' signalling: messages a second (and a burst), tracks a stream may have.
const (
	streamRate      = 20
	streamBurst     = 60
	maxStreamTracks = 4
)

// streamConns: /streams connections per person.
var streamConns = &connCount{max: 8}

type connCount struct {
	mu  sync.Mutex
	max int
	n   map[string]int
}

func (c *connCount) take(uid string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	if c.n[uid] >= c.max {
		return false
	}
	c.n[uid]++
	return true
}

func (c *connCount) give(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[uid] <= 1 {
		delete(c.n, uid)
	} else {
		c.n[uid]--
	}
}
