package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

func newTestServer(t *testing.T, mod func(*config)) (*server, *httptest.Server) {
	t.Helper()
	cfg := config{data: t.TempDir(), name: "Test", maxUsers: 50, udpMin: 40000, udpMax: 40400, maxRooms: 4, maxSubs: 10, maxSpaces: 4}
	if mod != nil {
		mod(&cfg)
	}
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.handler())
	t.Cleanup(func() { hs.Close(); s.store.close() })
	return s, hs
}

type tclient struct {
	t    *testing.T
	ws   *websocket.Conn
	key  *ecdsa.PrivateKey
	in   chan map[string]any
	wmu  sync.Mutex
	uid  string
	sid  int
	tok  string
	wel  map[string]any
	seen []map[string]any
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// dial connects and signs in; on refusal returns the error message instead.
func dialV(t *testing.T, hs *httptest.Server, key *ecdsa.PrivateKey, name, pass string) (*tclient, map[string]any) {
	t.Helper()
	return dialVWith(t, hs, key, name, map[string]any{"password": pass})
}

// dialVWith signs in with more in the hello (a password, the app's nonce).
func dialVWith(t *testing.T, hs *httptest.Server, key *ecdsa.PrivateKey, name string, extra map[string]any) (*tclient, map[string]any) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/ws"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ch map[string]any
	if err := ws.ReadJSON(&ch); err != nil || ch["t"] != "challenge" {
		t.Fatalf("challenge: %v %v", ch, err)
	}
	nonce, _ := base64.StdEncoding.DecodeString(ch["n"].(string))
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	h := sha256.Sum256(append([]byte(authPrefix), nonce...))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, h[:])
	hello := map[string]any{"t": "hello", "pub": base64.StdEncoding.EncodeToString(spki), "sig": base64.StdEncoding.EncodeToString(sig), "name": name, "client": "test/1"}
	for k, v := range extra {
		hello[k] = v
	}
	_ = ws.WriteJSON(hello)
	var w map[string]any
	if err := ws.ReadJSON(&w); err != nil {
		t.Fatal(err)
	}
	if w["t"] != "welcome" {
		_ = ws.Close()
		return nil, w
	}
	c := &tclient{t: t, ws: ws, key: key, in: make(chan map[string]any, 1024), uid: w["uid"].(string), sid: int(w["sid"].(float64)), tok: w["token"].(string), wel: w}
	go func() {
		defer close(c.in)
		for {
			var m map[string]any
			if ws.ReadJSON(&m) != nil {
				return
			}
			c.in <- m
		}
	}()
	t.Cleanup(func() { _ = ws.Close() })
	return c, nil
}

func (c *tclient) send(m map[string]any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.WriteJSON(m)
}

// wait gives the first message that matches, within 5 s.
func (c *tclient) wait(match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	for i, m := range c.seen {
		if match(m) {
			c.seen = append(c.seen[:i:i], c.seen[i+1:]...)
			return m
		}
	}
	deadline := time.After(8 * time.Second)
	for {
		select {
		case m, ok := <-c.in:
			if !ok {
				c.t.Fatal("connection closed")
			}
			if match(m) {
				return m
			}
			c.seen = append(c.seen, m)
		case <-deadline:
			c.t.Fatalf("timed out; seen %v", c.seen)
		}
	}
}

func isT(t string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["t"] == t }
}

func chanID(t *testing.T, wel map[string]any, name string) int64 {
	for _, c := range wel["channels"].([]any) {
		cm := c.(map[string]any)
		if cm["name"] == name {
			return int64(cm["id"].(float64))
		}
	}
	t.Fatalf("no channel %q", name)
	return 0
}

func TestSignInChannelsAndChat(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })

	a, _ := dialV(t, hs, ownerKey, "Ana", "")
	if a.wel["role"] != "owner" {
		t.Fatalf("owner role: %v", a.wel["role"])
	}
	b, _ := dialV(t, hs, newKey(t), "Bogdan", "")
	if b.wel["role"] != "" {
		t.Fatalf("member role: %v", b.wel["role"])
	}
	a.wait(func(m map[string]any) bool { return m["t"] == "member" && m["m"].(map[string]any)["name"] == "Bogdan" })
	lobby := chanID(t, a.wel, "Lobby")

	// A member may not make channels
	b.send(map[string]any{"t": "ch-save", "name": "Nope"})
	if e := b.wait(isT("error")); e["op"] != "ch-save" {
		t.Fatal(e)
	}
	// The owner makes a locked one with room for one
	a.send(map[string]any{"t": "ch-save", "name": "Secret", "password": "pw", "max": 1})
	c := a.wait(isT("channel"))["c"].(map[string]any)
	secret := int64(c["id"].(float64))
	if c["locked"] != true || c["max"].(float64) != 1 {
		t.Fatal(c)
	}
	b.wait(isT("channel"))

	b.send(map[string]any{"t": "join", "ch": secret, "password": "wrong"})
	if e := b.wait(isT("error")); e["why"] != "password" {
		t.Fatal(e)
	}
	b.send(map[string]any{"t": "join", "ch": secret, "password": "pw"})
	b.wait(func(m map[string]any) bool {
		return m["t"] == "member" && m["m"].(map[string]any)["uid"] == b.uid && int64(m["m"].(map[string]any)["ch"].(float64)) == secret
	})
	// Full now for a member; a third person can't come in
	d, _ := dialV(t, hs, newKey(t), "Dan", "")
	d.send(map[string]any{"t": "join", "ch": secret, "password": "pw"})
	if e := d.wait(isT("error")); e["why"] != "full" {
		t.Fatal(e)
	}

	// Channel chat: the locked channel's is only for who's in it (and admins)
	b.send(map[string]any{"t": "say", "to": "c:" + itoa(int(secret)), "text": "salut", "n": "x1"})
	got := b.wait(isT("msg"))
	if got["n"] != "x1" || got["m"].(map[string]any)["text"] != "salut" {
		t.Fatal(got)
	}
	a.wait(isT("msg")) // the owner reads it
	d.send(map[string]any{"t": "history", "to": "c:" + itoa(int(secret))})
	if e := d.wait(isT("error")); e["op"] != "history" {
		t.Fatal(e)
	}
	// Lobby chat and its history
	for i := 0; i < 3; i++ {
		a.send(map[string]any{"t": "say", "to": "c:" + itoa(int(lobby)), "text": "m" + itoa(i)})
	}
	for i := 0; i < 3; i++ {
		d.wait(isT("msg"))
	}
	d.send(map[string]any{"t": "history", "to": "c:" + itoa(int(lobby))})
	hist := d.wait(isT("history"))
	items := hist["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["text"] != "m0" || items[2].(map[string]any)["text"] != "m2" {
		t.Fatal(hist)
	}

	// Private chat, and the list of conversations
	d.send(map[string]any{"t": "say", "to": "u:" + b.uid, "text": "pss"})
	pm := b.wait(func(m map[string]any) bool { return m["t"] == "msg" && m["m"].(map[string]any)["text"] == "pss" })
	pmID := int64(pm["m"].(map[string]any)["id"].(float64))
	a.send(map[string]any{"t": "history", "to": "u:" + d.uid})
	if h := a.wait(isT("history")); len(h["items"].([]any)) != 0 {
		t.Fatal("someone else's private chat shown", h)
	}
	b.send(map[string]any{"t": "dms"})
	dms := b.wait(isT("dms"))["items"].([]any)
	if len(dms) != 1 || dms[0].(map[string]any)["with"] != d.uid || dms[0].(map[string]any)["name"] != "Dan" {
		t.Fatal(dms)
	}
	// Only the writer edits; the message comes back changed
	b.send(map[string]any{"t": "edit", "id": pmID, "text": "hack"})
	if e := b.wait(isT("error")); e["why"] != "owner" {
		t.Fatal(e)
	}
	d.send(map[string]any{"t": "edit", "id": pmID, "text": "psst"})
	ed := b.wait(func(m map[string]any) bool { return m["t"] == "msg" && m["m"].(map[string]any)["text"] == "psst" })
	if ed["m"].(map[string]any)["edit"] == nil {
		t.Fatal(ed)
	}

	// Kept across a restart
	msgs, _ := s.store.history(chanConv(lobby), 0, 10)
	if len(msgs) != 3 {
		t.Fatal(msgs)
	}

	// A mod moves; an admin bans; a banned person can't come back
	a.send(map[string]any{"t": "role", "uid": d.uid, "role": "mod"})
	if r := d.wait(isT("role")); r["role"] != "mod" {
		t.Fatal(r)
	}
	a.send(map[string]any{"t": "ban", "uid": b.uid, "why": "test"})
	if k := b.wait(isT("kicked")); k["ban"] != true {
		t.Fatal(k)
	}
	// Banned: they can't come back
	if back, e := dialV(t, hs, b.key, "Bogdan", ""); back != nil || e["why"] != "banned" {
		t.Fatal("banned person came back", e)
	}
}

func TestServerPassword(t *testing.T) {
	_, hs := newTestServer(t, func(c *config) { c.joinPass = "s3cret" })
	if _, e := dialV(t, hs, newKey(t), "X", "nope"); e == nil || e["why"] != "password" {
		t.Fatal(e)
	}
	if c, _ := dialV(t, hs, newKey(t), "Y", "s3cret"); c == nil {
		t.Fatal("refused with the right password")
	}
}

func TestBadSignature(t *testing.T) {
	_, hs := newTestServer(t, nil)
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/ws"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var ch map[string]any
	_ = ws.ReadJSON(&ch)
	k := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	h := sha256.Sum256([]byte(authPrefix + "another nonce"))
	sig, _ := ecdsa.SignASN1(rand.Reader, k, h[:])
	_ = ws.WriteJSON(map[string]any{"t": "hello", "pub": base64.StdEncoding.EncodeToString(spki), "sig": base64.StdEncoding.EncodeToString(sig), "name": "Z"})
	var r map[string]any
	_ = ws.ReadJSON(&r)
	if r["t"] != "error" || r["why"] != "auth" {
		t.Fatal(r)
	}
}

func TestToken(t *testing.T) {
	s := &server{secret: []byte("0123456789abcdef0123456789abcdef")}
	tok := s.makeToken("abc")
	if u, ok := s.checkToken(tok); !ok || u != "abc" {
		t.Fatal(u, ok)
	}
	if _, ok := s.checkToken(strings.Replace(tok, "abc", "abd", 1)); ok {
		t.Fatal("forged token taken")
	}
}

// voiceClient is an app's voice connection, as the phone makes it: one audio track up, the others' down.
type voiceClient struct {
	c     *tclient
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP
	mu    sync.Mutex
	got   map[string]int // packets received, by stream id ("s<sid>")
	talk  map[int]bool   // talk events seen (sid → on)
}

func newVoiceClient(t *testing.T, c *tclient) *voiceClient {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	_ = m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.AudioLevelURI}, webrtc.RTPCodecTypeAudio)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	tr, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "me")
	if _, err := pc.AddTrack(tr); err != nil {
		t.Fatal(err)
	}
	vc := &voiceClient{c: c, pc: pc, track: tr, got: map[string]int{}, talk: map[int]bool{}}
	pc.OnTrack(func(r *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		id := r.StreamID()
		buf := make([]byte, 1500)
		for {
			if _, _, err := r.Read(buf); err != nil {
				return
			}
			vc.mu.Lock()
			vc.got[id]++
			vc.mu.Unlock()
		}
	})
	pc.OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}
		j := ic.ToJSON()
		c.send(map[string]any{"t": "ice", "c": j.Candidate, "mid": *j.SDPMid, "mline": *j.SDPMLineIndex})
	})
	offer, _ := pc.CreateOffer(nil)
	_ = pc.SetLocalDescription(offer)
	c.send(map[string]any{"t": "rtc", "type": "offer", "sdp": offer.SDP})
	return vc
}

// pump answers the server's signalling for this client until stop.
func (vc *voiceClient) pump(stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case m, ok := <-vc.c.in:
			if !ok {
				return
			}
			switch m["t"] {
			case "rtc":
				desc := webrtc.SessionDescription{SDP: m["sdp"].(string)}
				if m["type"] == "answer" {
					desc.Type = webrtc.SDPTypeAnswer
					_ = vc.pc.SetRemoteDescription(desc)
					continue
				}
				desc.Type = webrtc.SDPTypeOffer
				_ = vc.pc.SetRemoteDescription(desc)
				ans, _ := vc.pc.CreateAnswer(nil)
				_ = vc.pc.SetLocalDescription(ans)
				vc.c.send(map[string]any{"t": "rtc", "type": "answer", "sdp": ans.SDP})
			case "ice":
				c := webrtc.ICECandidateInit{Candidate: m["c"].(string)}
				if mid, ok := m["mid"].(string); ok {
					c.SDPMid = &mid
				}
				_ = vc.pc.AddICECandidate(c)
			case "talk":
				if m["on"] == true {
					vc.mu.Lock()
					vc.talk[int(m["sid"].(float64))] = true
					vc.mu.Unlock()
				}
			}
		}
	}
}

func (vc *voiceClient) count(stream string) int {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	return vc.got[stream]
}

func TestVoiceForwardedInChannel(t *testing.T) {
	_, hs := newTestServer(t, nil)
	a, _ := dialV(t, hs, newKey(t), "A", "")
	b, _ := dialV(t, hs, newKey(t), "B", "")
	c, _ := dialV(t, hs, newKey(t), "C", "")
	afk := chanID(t, a.wel, "AFK")
	va, vb, vcl := newVoiceClient(t, a), newVoiceClient(t, b), newVoiceClient(t, c)
	stop := make(chan struct{})
	defer close(stop)
	go va.pump(stop)
	go vb.pump(stop)
	go vcl.pump(stop)
	// C goes to another channel: it must not hear A
	c.send(map[string]any{"t": "join", "ch": afk})

	// A speaks (Opus silence frames with a loud audio level)
	go func() {
		seq := uint16(0)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for i := 0; i < 1500; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			seq++
			p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte{0xf8, 0xff, 0xfe}}
			for _, e := range va.pc.GetSenders()[0].GetParameters().HeaderExtensions {
				if e.URI == sdp.AudioLevelURI {
					_ = p.SetExtension(uint8(e.ID), []byte{0x80 | 20}) // voice active, -20 dBov
				}
			}
			_ = va.track.WriteRTP(p)
		}
	}()
	want := "s" + itoa(a.sid)
	deadline := time.Now().Add(15 * time.Second)
	for vb.count(want) < 20 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := vb.count(want); n < 20 {
		t.Fatalf("B got %d packets of A's voice (A %s/%s, B %s)", n, va.pc.ConnectionState(), va.pc.SignalingState(), vb.pc.ConnectionState())
	}
	vb.mu.Lock()
	heard := vb.talk[a.sid]
	vb.mu.Unlock()
	if !heard {
		t.Fatal("B wasn't told A is talking")
	}
	if n := vcl.count(want); n != 0 {
		t.Fatalf("C, in another channel, got %d packets of A's voice", n)
	}
	// C comes back: now it hears A too
	lobby := chanID(t, a.wel, "Lobby")
	c.send(map[string]any{"t": "join", "ch": lobby})
	deadline = time.Now().Add(10 * time.Second)
	for vcl.count(want) < 10 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := vcl.count(want); n < 10 {
		t.Fatalf("C, back in the channel, got %d packets", n)
	}

	// Whisper: B goes to AFK, A whispers to B: B hears A, C (still with A in Lobby) doesn't
	b.send(map[string]any{"t": "join", "ch": afk})
	a.send(map[string]any{"t": "whisper", "on": true, "sids": []int{b.sid}})
	time.Sleep(1500 * time.Millisecond)
	b0, c0 := vb.count(want), vcl.count(want)
	time.Sleep(1500 * time.Millisecond)
	if n := vb.count(want) - b0; n < 20 {
		t.Fatalf("B, whispered to in another channel, got %d packets", n)
	}
	if n := vcl.count(want) - c0; n > 3 {
		t.Fatalf("C heard a whisper meant for B: %d packets", n)
	}
	// Whisper off: back to A's channel
	a.send(map[string]any{"t": "whisper", "on": false})
	time.Sleep(1500 * time.Millisecond)
	b0, c0 = vb.count(want), vcl.count(want)
	time.Sleep(1500 * time.Millisecond)
	if n := vcl.count(want) - c0; n < 20 {
		t.Fatalf("C, in A's channel after the whisper, got %d packets", n)
	}
	if n := vb.count(want) - b0; n > 3 {
		t.Fatalf("B still hears A after the whisper: %d packets", n)
	}
}
