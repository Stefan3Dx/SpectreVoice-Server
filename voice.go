package main

// Voice in channels: every app keeps one WebRTC connection to the server. Its microphone goes up once (Opus); the
// server forwards it to the others in the same channel, each as its own track (stream id "s<sid>", so an app
// knows whose voice it plays and can set its volume). The app makes the first offer; after that the server offers
// when people come and go, and the app when it changes its voice level or restarts ICE.
//
// The two can offer at once (a far app: its offer crosses ours on the way). Pion can't roll its own offer back, so
// ours is kept ("keep" on it): an app that gives way ("polite" on its offers, 0.9.1+) rolls its offer back, answers
// ours and offers again after; its crossed offer is ignored here. An app that keeps its own (older apps) gets an
// rtc-reset (a clean new connection) instead of an answer that can't be made.
//
//	<- {"t":"rtc","type":"offer","sdp":…,"polite":true}  -> {"t":"rtc","type":"answer","sdp":…}
//	-> {"t":"rtc","type":"offer","sdp":…,"keep":true}    <- {"t":"rtc","type":"answer","sdp":…}
//	<-> {"t":"ice","c":…,"mid":…,"mline":…}
//	-> {"t":"rtc-reset"}                      the connection failed: make a new one
//	-> {"t":"talk","sid":…,"on":bool}         who is talking (from the voice's level, RFC 6464)

import (
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

const (
	talkLevel = 55 // dBov below 0 at which the voice counts as talking (0 loudest, 127 silence)
	talkHold  = 350 * time.Millisecond
)

type voicePeer struct {
	ss *session
	pc *webrtc.PeerConnection
	// sig: one description change at a time on pc (the app's offer or answer, our offer, tracks added or
	// removed), so the server never offers while it answers. Taken before mu; never held into sync or negotiate.
	sig     sync.Mutex
	mu      sync.Mutex
	local   webrtc.TrackLocal         // this person's voice as the others get it
	upEnd   chan struct{}             // closed when the carry of local ends (a newer mic track waits for it)
	senders map[int]*webrtc.RTPSender // whose voice (sid) goes to this person through which sender
	ready   bool                      // the app's first offer is answered
	pending bool                      // a server offer waits for its answer
	again   bool                      // something changed meanwhile: offer again after it
	early   []webrtc.ICECandidateInit // candidates before the first offer arrived
	cands   int                       // candidates the app sent (capped: each one is work, and STUN sent out)
	said    bool                      // our first description went out (our candidates may follow it)
	held    []map[string]any          // our candidates made before that
	lastHot time.Time                 // the last loud packet
}

type voice struct {
	s      *server
	syncMu sync.Mutex // held through a whole sync
	mu     sync.Mutex
	peers  map[int]*voicePeer
	kick   chan struct{} // syncAllSoon's one waiting ask
}

func newVoice(s *server) *voice {
	v := &voice{s: s, peers: map[int]*voicePeer{}, kick: make(chan struct{}, 1)}
	go v.talkWatch()
	go func() {
		for range v.kick {
			v.syncAll()
		}
	}()
	return v
}

// syncAllSoon asks for a syncAll without waiting; many asks while one runs make one more, not many.
func (v *voice) syncAllSoon() {
	select {
	case v.kick <- struct{}{}:
	default:
	}
}

func (v *voice) peer(sid int) *voicePeer {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.peers[sid]
}

// signal takes the app's offer, answer or candidate.
func (v *voice) signal(ss *session, t string, m map[string]any) {
	p := v.peer(ss.sid)
	if t == "ice" {
		c := webrtc.ICECandidateInit{Candidate: str(m, "c")}
		if mid, ok := m["mid"].(string); ok {
			c.SDPMid = &mid
		}
		if ml, ok := m["mline"].(float64); ok {
			n := uint16(ml)
			c.SDPMLineIndex = &n
		}
		if c.Candidate == "" || len(c.Candidate) > maxCandLen {
			return
		}
		if p == nil {
			return
		}
		p.mu.Lock()
		p.cands++
		if p.cands > maxCands {
			p.mu.Unlock()
			return
		}
		if p.pc.RemoteDescription() == nil {
			if len(p.early) < maxEarly {
				p.early = append(p.early, c)
			}
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		_ = p.pc.AddICECandidate(c)
		return
	}
	desc := str(m, "sdp")
	switch str(m, "type") {
	case "offer":
		if p != nil && (p.dead() || p.otherConnection(desc)) {
			// The app's old connection is gone (it closed it, or it broke), or this offer comes from another
			// connection of the app's (another DTLS certificate: the old peer would take it and never connect)
			v.dropPeer(p)
			p = nil
		}
		if p == nil {
			var err error
			if p, err = v.open(ss); err != nil {
				ss.refuse("rtc", "server", err.Error())
				return
			}
		}
		polite, _ := m["polite"].(bool)
		if !v.answer(p, desc, polite) && p.wasReady() {
			// An offer a working connection can't take: a new connection of the app's (another DTLS and ICE).
			// It gets a fresh peer instead of no voice at all
			np, err := v.open(ss)
			if err != nil {
				ss.refuse("rtc", "server", err.Error())
				return
			}
			v.answer(np, desc, polite)
		}
	case "answer":
		if p == nil {
			return
		}
		p.sig.Lock()
		if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: desc}); err != nil {
			p.sig.Unlock()
			log.Printf("voice %d: answer: %v", ss.sid, err)
			v.reset(p)
			return
		}
		p.mu.Lock()
		p.pending = false
		again := p.again
		p.again = false
		p.mu.Unlock()
		p.sig.Unlock()
		if again {
			v.negotiate(p)
		}
	}
}

// open makes the server's side of an app's voice connection.
func (v *voice) open(ss *session) (*voicePeer, error) {
	v.mu.Lock()
	if mu := v.s.maxUsers(); mu > 0 && len(v.peers) >= mu*2+8 { // phone and PC each, plus the bots
		v.mu.Unlock()
		return nil, errors.New("voice is full")
	}
	v.mu.Unlock()
	pc, err := v.s.webrtcAPI().NewPeerConnection(webrtc.Configuration{ICEServers: v.s.ice})
	if err != nil {
		return nil, err
	}
	p := &voicePeer{ss: ss, pc: pc, senders: map[int]*webrtc.RTPSender{}}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ci := c.ToJSON()
		out := map[string]any{"t": "ice", "c": ci.Candidate}
		if ci.SDPMid != nil {
			out["mid"] = *ci.SDPMid
		}
		if ci.SDPMLineIndex != nil {
			out["mline"] = *ci.SDPMLineIndex
		}
		// Never before our answer: an app can't use a candidate for a connection it hasn't described yet
		p.mu.Lock()
		if !p.said {
			p.held = append(p.held, out)
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		ss.send(out)
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		if remote.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		v.carry(p, remote, recv)
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateFailed:
			log.Printf("voice %d: connection failed", ss.sid)
			v.reset(p)
		case webrtc.PeerConnectionStateClosed:
			// Closed from the app's side (DTLS close): a dead peer isn't kept, and the others stop getting its voice
			if v.peer(ss.sid) == p {
				v.dropPeer(p)
				v.s.hub.mu.Lock()
				ch := p.ss.ch
				v.s.hub.mu.Unlock()
				go v.sync(ch)
			}
		}
	})
	v.mu.Lock()
	if old := v.peers[ss.sid]; old != nil {
		v.mu.Unlock()
		_ = pc.Close()
		return old, nil
	}
	v.peers[ss.sid] = p
	v.mu.Unlock()
	return p, nil
}

// dead: the peer's connection is closed or failed (an offer can't go on it).
func (p *voicePeer) dead() bool {
	st := p.pc.ConnectionState()
	return st == webrtc.PeerConnectionStateClosed || st == webrtc.PeerConnectionStateFailed
}

// otherConnection: offer is from another of the app's connections than the one this peer talks to (its DTLS
// fingerprint differs: a connection keeps its certificate for life, renegotiations and ICE restarts too).
func (p *voicePeer) otherConnection(offer string) bool {
	rd := p.pc.RemoteDescription()
	if rd == nil {
		return false
	}
	a, b := sdpFingerprint(rd.SDP), sdpFingerprint(offer)
	return a != "" && b != "" && a != b
}

// sdpFingerprint: the first a=fingerprint of a description ("" when none).
func sdpFingerprint(desc string) string {
	_, rest, ok := strings.Cut(desc, "a=fingerprint:")
	if !ok {
		return ""
	}
	line, _, _ := strings.Cut(rest, "\n")
	return strings.ToLower(strings.TrimSpace(line))
}

// wasReady: the peer had answered the app's first offer (a connection that worked).
func (p *voicePeer) wasReady() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ready
}

// answer takes an offer from the app (its first, or a new one, e.g. after a network change); false when it
// couldn't be taken (the peer is dropped then). polite: the app gives way when our offers cross.
func (v *voice) answer(p *voicePeer, offer string, polite bool) bool {
	pc := p.pc
	p.sig.Lock()
	if pc.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		// Both offered at once. Pion can't roll ours back (have-local-offer -> rollback is refused), so ours stays
		p.sig.Unlock()
		if polite {
			// The app rolls its offer back, answers ours and offers again after: this one is dropped
			if os.Getenv("SV_DEBUG_VOICE") != "" {
				log.Printf("voice %d: offers crossed, the app gives way", p.ss.sid)
			}
			return true
		}
		// An app that keeps its own offer and ignores ours: neither can be answered on this connection. A clean
		// new one (the app's offer on a new peer would carry this connection's ICE and DTLS: it barely comes up)
		log.Printf("voice %d: offers crossed with an app that keeps its own: new connection", p.ss.sid)
		v.reset(p)
		return true
	}
	// A sane offer: the app's voice plus the media sections this connection has (an offer with hundreds of tracks
	// is refused). Transceivers stay once made (a voice that went is an inactive section, reused later), so the
	// limit counts them, not the voices sent now: else a re-offer after many people came and went is refused.
	most := min(max(len(pc.GetTransceivers())+4, 8), maxMlines)
	if n := mlines(offer); n == 0 || n > most {
		p.sig.Unlock()
		if p.wasReady() {
			// A working connection: the app makes a new one (a refusal alone would leave it without voice)
			log.Printf("voice %d: offer with %d media sections not taken (at most %d)", p.ss.sid, n, most)
			v.reset(p)
		} else {
			p.ss.refuse("rtc", "bad", "offer not taken")
			v.dropPeer(p)
		}
		return true
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		p.sig.Unlock()
		log.Printf("voice %d: offer: %v", p.ss.sid, err)
		// Not kept half made (its candidates would pile up for ever)
		v.dropPeer(p)
		if !p.wasReady() {
			p.ss.refuse("rtc", "bad", "offer not taken")
		}
		return false
	}
	ans, err := pc.CreateAnswer(nil)
	if err == nil {
		err = pc.SetLocalDescription(ans)
	}
	if err != nil {
		p.sig.Unlock()
		log.Printf("voice %d: answer: %v", p.ss.sid, err)
		v.reset(p)
		return true
	}
	p.mu.Lock()
	p.ss.send(map[string]any{"t": "rtc", "type": "answer", "sdp": pc.LocalDescription().SDP})
	held := p.held
	p.held, p.said = nil, true
	for _, c := range held {
		p.ss.send(c)
	}
	early := p.early
	p.early = nil
	// A new offer (an ICE restart) brings new candidates: the cap is per offer, not per connection's life
	p.cands = len(early)
	first := !p.ready
	p.ready = true
	again := p.again
	p.again = false
	p.mu.Unlock()
	for _, c := range early {
		_ = pc.AddICECandidate(c)
	}
	p.sig.Unlock()
	if first {
		v.s.hub.mu.Lock()
		ch := p.ss.ch
		v.s.hub.mu.Unlock()
		v.sync(ch)
	} else if again {
		v.negotiate(p)
	}
	return true
}

// carry forwards the app's voice to its local track, and watches its level for "talking".
func (v *voice) carry(p *voicePeer, remote *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
	sid := p.ss.sid
	local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, "a"+itoa(sid), "s"+itoa(sid))
	if err != nil {
		log.Printf("voice %d: track: %v", sid, err)
		return
	}
	levelID := 0
	for _, e := range recv.GetParameters().HeaderExtensions {
		if e.URI == sdp.AudioLevelURI {
			levelID = e.ID
		}
	}
	// One voice per connection: a second track waits until the first one ends (the app changed its microphone's
	// transceiver), and is carried then; two at once would each re-sync the channel
	for {
		p.mu.Lock()
		if p.local == nil {
			break
		}
		wait := p.upEnd
		p.mu.Unlock()
		if wait == nil || p.dead() {
			return
		}
		<-wait
	}
	p.local = local
	end := make(chan struct{})
	p.upEnd = end
	p.mu.Unlock()
	log.Printf("voice %d: up (%s)", sid, remote.Codec().MimeType)
	v.s.hub.mu.Lock()
	ch := p.ss.ch
	v.s.hub.mu.Unlock()
	v.sync(ch)
	defer func() {
		// The track ended: the others stop getting it (else they'd keep a silent voice, and a newer track of the
		// app's could never be carried)
		p.mu.Lock()
		mine := p.local == local
		if mine {
			p.local = nil
			p.upEnd = nil
		}
		p.mu.Unlock()
		close(end)
		if mine && v.peer(sid) == p {
			v.s.hub.mu.Lock()
			ch := p.ss.ch
			v.s.hub.mu.Unlock()
			v.sync(ch)
		}
	}()
	buf := make([]byte, 1500)
	var pkt rtp.Packet
	var lastErr time.Time
	for {
		n, _, err := remote.Read(buf)
		if err != nil {
			return
		}
		statVoiceIn.Add(int64(n))
		if pkt.Unmarshal(buf[:n]) != nil {
			continue
		}
		if levelID != 0 {
			if ext := pkt.GetExtension(uint8(levelID)); len(ext) > 0 && int(ext[0]&0x7f) <= talkLevel {
				p.mu.Lock()
				p.lastHot = time.Now()
				p.mu.Unlock()
			}
		}
		// The others negotiated their own extension ids: none are passed on
		pkt.Header.Extension = false
		pkt.Header.Extensions = nil
		// One listener's error isn't everyone's: keep forwarding (only the app's own track ending stops this)
		if err := local.WriteRTP(&pkt); err != nil && !errors.Is(err, io.ErrClosedPipe) && time.Since(lastErr) > 10*time.Second {
			lastErr = time.Now()
			log.Printf("voice %d: forward: %v", sid, err)
		}
	}
}

// talkWatch turns the voices' levels into "talking" for everyone, ten times a second.
func (v *voice) talkWatch() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		h := v.s.hub
		if h == nil {
			continue
		}
		now := time.Now()
		v.mu.Lock()
		hot := map[int]bool{}
		for sid, p := range v.peers {
			p.mu.Lock()
			hot[sid] = now.Sub(p.lastHot) < talkHold
			p.mu.Unlock()
		}
		v.mu.Unlock()
		h.mu.Lock()
		for _, ss := range h.sessions {
			// Someone silenced (or not let talk in a moderated channel) isn't shown talking either
			on := hot[ss.sid] && !ss.mic && h.maySpeak(ss)
			if on != ss.talk {
				ss.talk = on
				h.toComm(ss.comm, map[string]any{"t": "talk", "sid": ss.sid, "on": on})
			}
		}
		h.mu.Unlock()
	}
}

// sync makes everyone in the given channels hear exactly who they should: the others there, and anyone whispering
// to them. While someone whispers, everyone is looked at (a whisper reaches across channels).
func (v *voice) sync(chs ...int64) { v.syncSome(chs, false) }

// syncAll looks at everyone (a whisper started or stopped).
func (v *voice) syncAll() { v.syncSome(nil, true) }

type hearing struct {
	sid     int
	ch      int64
	whisper bool
	toSid   map[int]bool
	toCh    map[int64]bool
}

// hears: does a listener (sid, channel) get speaker sp's voice?
func (sp hearing) hears(sid int, ch int64) bool {
	if sp.sid == sid {
		return false
	}
	if sp.whisper {
		return sp.toSid[sid] || sp.toCh[ch]
	}
	return sp.ch == ch
}

func (v *voice) syncSome(chs []int64, all bool) {
	// One at a time: each works from a fresh look at who's where, so an older look can't undo a newer one
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	h := v.s.hub
	in := map[int64]bool{}
	for _, c := range chs {
		in[c] = true
	}
	h.mu.Lock()
	var speakers []hearing
	for _, ss := range h.sessions {
		if ss.whispering {
			all = true // a whisper reaches other channels: a change to it (muted now) must reach them too
		}
		if !h.maySpeak(ss) {
			continue
		}
		hr := hearing{sid: ss.sid, ch: ss.ch, whisper: ss.whispering}
		if ss.whispering {
			all = true
			hr.toSid, hr.toCh = map[int]bool{}, map[int64]bool{}
			for _, t := range ss.whisperTo {
				hr.toSid[t] = true
			}
			for _, c := range ss.whisperCh {
				hr.toCh[c] = true
			}
		}
		speakers = append(speakers, hr)
	}
	type listener struct {
		sid int
		ch  int64
	}
	var who []listener
	for _, ss := range h.sessions {
		if all || in[ss.ch] {
			who = append(who, listener{ss.sid, ss.ch})
		}
	}
	h.mu.Unlock()
	v.mu.Lock()
	tracks := map[int]webrtc.TrackLocal{}
	for sid, p := range v.peers {
		p.mu.Lock()
		tracks[sid] = p.local
		p.mu.Unlock()
	}
	var peers []*voicePeer
	var chOf []int64
	for _, l := range who {
		if p := v.peers[l.sid]; p != nil {
			peers = append(peers, p)
			chOf = append(chOf, l.ch)
		}
	}
	v.mu.Unlock()
	for i, p := range peers {
		want := map[int]webrtc.TrackLocal{}
		for _, sp := range speakers {
			if tracks[sp.sid] != nil && sp.hears(p.ss.sid, chOf[i]) {
				want[sp.sid] = tracks[sp.sid]
			}
		}
		v.reconcile(p, want)
	}
}

// reconcile adds and removes p's forwarded voices to match want, then offers the change.
func (v *voice) reconcile(p *voicePeer, want map[int]webrtc.TrackLocal) {
	if os.Getenv("SV_DEBUG_VOICE") != "" {
		log.Printf("voice %d: wants %d voices", p.ss.sid, len(want))
	}
	p.sig.Lock()
	p.mu.Lock()
	if !p.ready || p.pc == nil {
		p.mu.Unlock()
		p.sig.Unlock()
		return
	}
	changed := false
	for sid, snd := range p.senders {
		if want[sid] == nil || snd.Track() != want[sid] {
			_ = p.pc.RemoveTrack(snd)
			delete(p.senders, sid)
			changed = true
		}
	}
	for sid, tr := range want {
		if p.senders[sid] != nil {
			continue
		}
		snd, err := p.pc.AddTrack(tr)
		if err != nil {
			log.Printf("voice %d: add %d: %v", p.ss.sid, sid, err)
			continue
		}
		p.senders[sid] = snd
		go drainRTCP(snd)
		changed = true
	}
	p.mu.Unlock()
	p.sig.Unlock()
	if changed {
		v.negotiate(p)
	}
}

func drainRTCP(s *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := s.Read(buf); err != nil {
			return
		}
	}
}

// negotiate sends the app a new offer, or remembers to once the one out is answered.
func (v *voice) negotiate(p *voicePeer) {
	p.sig.Lock()
	defer p.sig.Unlock()
	p.mu.Lock()
	if p.pending || !p.ready {
		p.again = true
		p.mu.Unlock()
		return
	}
	if p.pc.SignalingState() != webrtc.SignalingStateStable {
		p.again = true
		p.mu.Unlock()
		return
	}
	p.pending = true
	p.mu.Unlock()
	offer, err := p.pc.CreateOffer(nil)
	if err == nil {
		err = p.pc.SetLocalDescription(offer)
	}
	if err != nil {
		log.Printf("voice %d: offer: %v", p.ss.sid, err)
		p.mu.Lock()
		p.pending = false
		p.mu.Unlock()
		return
	}
	p.ss.send(map[string]any{"t": "rtc", "type": "offer", "sdp": p.pc.LocalDescription().SDP, "keep": true})
}

// reset drops a broken voice connection; the app makes a new one.
func (v *voice) reset(p *voicePeer) {
	v.dropPeer(p)
	p.ss.send(map[string]any{"t": "rtc-reset"})
	v.s.hub.mu.Lock()
	ch := p.ss.ch
	v.s.hub.mu.Unlock()
	v.sync(ch)
}

// dropPeer closes this connection, and forgets it only if it is still the session's (a late failure of an old one
// mustn't close the new one).
func (v *voice) dropPeer(p *voicePeer) {
	v.mu.Lock()
	if v.peers[p.ss.sid] == p {
		delete(v.peers, p.ss.sid)
	}
	v.mu.Unlock()
	if p.pc != nil {
		_ = p.pc.Close()
	}
}

// mlines: how many media sections an SDP has (0: it doesn't parse).
func mlines(desc string) int {
	var sd sdp.SessionDescription
	if sd.UnmarshalString(desc) != nil {
		return 0
	}
	return len(sd.MediaDescriptions)
}

// Limits on what an app sends about its connection.
const (
	maxCandLen = 512
	maxCands   = 64
	maxEarly   = 32
	maxMlines  = 128 // media sections in an app's offer at most
)

// drop closes a session's voice connection (it left, or the connection broke).
func (v *voice) drop(ss *session) {
	v.mu.Lock()
	p := v.peers[ss.sid]
	if p != nil && p.ss == ss {
		delete(v.peers, ss.sid)
	}
	v.mu.Unlock()
	if p != nil && p.pc != nil {
		_ = p.pc.Close()
	}
}

// addBot gives the music bot a voice: a track the others get like a person's (it has no connection itself).
func (v *voice) addBot(ss *session, track webrtc.TrackLocal) {
	v.mu.Lock()
	v.peers[ss.sid] = &voicePeer{ss: ss, local: track, senders: map[int]*webrtc.RTPSender{}}
	v.mu.Unlock()
	v.s.hub.mu.Lock()
	ch := ss.ch
	v.s.hub.mu.Unlock()
	v.sync(ch)
}

// botHot: the bot sent sound now (it shows as talking).
func (v *voice) botHot(sid int) {
	v.mu.Lock()
	p := v.peers[sid]
	v.mu.Unlock()
	if p != nil {
		p.mu.Lock()
		p.lastHot = time.Now()
		p.mu.Unlock()
	}
}

func (v *voice) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.peers)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
