package main

// Streams (screen and camera) through the server: the streamer sends its picture once; the server forwards it to
// every viewer. A room is named by its streamer's uid (and an optional ":suffix"), so only that identity may
// publish it; anyone on the server may watch.
//
// Signalling is JSON over a WebSocket at /streams?token=…:
//
//	{"type":"publish","room":R,"sdp":OFFER}  -> {"type":"answer","sdp":…}
//	{"type":"subscribe","room":R}            -> {"type":"offer","sdp":…} ; reply {"type":"answer","sdp":…}
//	{"type":"candidate","candidate":C,"mid":M,"mline":N} both ways
//	server -> {"type":"ended"} when the room's streamer leaves, {"type":"error","message":…}
//
// GET /streams/status?room=R&token=… answers {"live":bool,"viewers":n}.

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
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
}

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
	pc      *webrtc.PeerConnection
	c       *conn
	mu      sync.Mutex
	pending bool // an offer is out, waiting for the answer
	again   bool // tracks changed meanwhile: offer again after it
}

type room struct {
	name      string
	mu        sync.Mutex
	publisher *webrtc.PeerConnection
	pubConn   *conn
	tracks    []*webrtc.TrackLocalStaticRTP
	ssrcs     []webrtc.SSRC // the publisher's video SSRCs, for key frame requests
	subs      map[*subscriber]bool
	lastKey   time.Time // the last key frame asked of the streamer (asked at most twice a second)
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
	live, viewers := false, 0
	if rm != nil {
		rm.mu.Lock()
		live = rm.publisher != nil && len(rm.tracks) > 0
		viewers = len(rm.subs)
		rm.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(message{Type: "status", Room: name, Live: &live, Viewers: &viewers})
}

func (s *server) handleStreams(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.tokenUID(r)
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	c := &conn{ws: ws}
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
		switch m.Type {
		case "publish":
			if pc != nil {
				c.send(message{Type: "error", Message: "already publishing or watching"})
				continue
			}
			if m.Room != uid && !strings.HasPrefix(m.Room, uid+":") {
				c.send(message{Type: "error", Message: "you may only stream as yourself"})
				return
			}
			rm, err = s.openRoom(m.Room, c)
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
				s.hub.setLive(rm.name, uid, clip(m.Title, 80), true)
			}
		case "subscribe":
			if pc != nil {
				c.send(message{Type: "error", Message: "already publishing or watching"})
				continue
			}
			s.mu.Lock()
			rm = s.rooms[m.Room]
			s.mu.Unlock()
			if rm == nil {
				c.send(message{Type: "ended"})
				return
			}
			sub, err = s.subscribe(rm, c)
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
		case "candidate":
			if pc == nil || m.Candidate == "" {
				continue
			}
			_ = pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: m.Candidate, SDPMid: m.Mid, SDPMLineIndex: m.MLine})
		}
	}
}

// openRoom makes the room c will stream to.
func (s *server) openRoom(name string, c *conn) (*room, error) {
	if name == "" || len(name) > 200 {
		return nil, errors.New("bad room name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	rm := &room{name: name, subs: map[*subscriber]bool{}, pubConn: c}
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
	pub := rm.publisher
	rm.publisher = nil
	rm.tracks = nil
	rm.mu.Unlock()
	s.mu.Lock()
	gone := s.rooms[rm.name] == rm
	if gone {
		delete(s.rooms, rm.name)
	}
	s.mu.Unlock()
	if gone && s.hub != nil {
		s.hub.setLive(rm.name, "", "", false)
	}
	for sub := range subs {
		sub.c.send(message{Type: "ended"})
		_ = sub.pc.Close()
	}
	if pub != nil && pub != only {
		_ = pub.Close()
	}
	log.Printf("[%s] ended", rm.name)
}

func (s *server) newPC(c *conn) (*webrtc.PeerConnection, error) {
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{ICEServers: s.ice})
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
		local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, remote.ID(), "spectre")
		if err != nil {
			log.Printf("[%s] track: %v", rm.name, err)
			return
		}
		rm.mu.Lock()
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
				go rm.forwardRTCP(sender)
				rm.offerTo(sub)
			}
		}
		buf := make([]byte, 1500)
		for {
			n, _, err := remote.Read(buf)
			if err != nil {
				return
			}
			if _, err := local.Write(buf[:n]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				return
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

func (s *server) subscribe(rm *room, c *conn) (*subscriber, error) {
	rm.mu.Lock()
	if len(rm.subs) >= s.cfg.maxSubs {
		rm.mu.Unlock()
		return nil, errors.New("this stream has as many viewers as it may")
	}
	tracks := append([]*webrtc.TrackLocalStaticRTP(nil), rm.tracks...)
	rm.mu.Unlock()
	pc, err := s.newPC(c)
	if err != nil {
		return nil, err
	}
	sub := &subscriber{pc: pc, c: c}
	for _, t := range tracks {
		sender, err := pc.AddTrack(t)
		if err != nil {
			_ = pc.Close()
			return nil, err
		}
		go rm.forwardRTCP(sender)
	}
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateConnected {
			rm.keyFrame()
		}
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			rm.removeSub(sub)
		}
	})
	rm.mu.Lock()
	rm.subs[sub] = true
	n := len(rm.subs)
	rm.mu.Unlock()
	log.Printf("[%s] viewer in (%d)", rm.name, n)
	rm.offerTo(sub)
	return sub, nil
}

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
	offer, err := sub.pc.CreateOffer(nil)
	if err == nil {
		err = sub.pc.SetLocalDescription(offer)
	}
	if err != nil {
		log.Printf("[%s] offer: %v", rm.name, err)
		return
	}
	sub.c.send(message{Type: "offer", SDP: sub.pc.LocalDescription().SDP})
}

// forwardRTCP passes a viewer's key frame requests on to the streamer.
func (rm *room) forwardRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				rm.keyFrame()
			}
		}
	}
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

func (rm *room) removeSub(sub *subscriber) {
	rm.mu.Lock()
	_, was := rm.subs[sub]
	delete(rm.subs, sub)
	n := len(rm.subs)
	rm.mu.Unlock()
	if was {
		_ = sub.pc.Close()
		log.Printf("[%s] viewer out (%d)", rm.name, n)
	}
}
