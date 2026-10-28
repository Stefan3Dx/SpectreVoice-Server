package main

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// A viewer's track that remembers what came out of the switcher.
func testSwitcher(t *testing.T) (*switcher, *webrtc.TrackLocalStaticRTP) {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "v", "s")
	if err != nil {
		t.Fatal(err)
	}
	return newSwitcher(track, webrtc.MimeTypeVP8), track
}

// vp8 makes a VP8 packet of [rid]'s stream: key or not, with the layer's own numbering.
func vp8(seq uint16, ts uint32, key bool) *rtp.Packet {
	b := byte(0x01) // inter frame
	if key {
		b = 0x00
	}
	// Descriptor: S=1, PID=0, then the first payload byte
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 1}, Payload: []byte{0x10, b, 0, 0}}
}

func TestSwitcherContinuity(t *testing.T) {
	sw, _ := testSwitcher(t)
	// Nothing wanted yet: nothing goes
	if sw.push("h", vp8(1, 1000, true)) {
		t.Fatal("switched with nothing wanted")
	}
	if !sw.choose("h") {
		t.Fatal("h should be a pending switch")
	}
	// An inter frame can't start it; a key frame does
	sw.push("h", vp8(2, 1000, false))
	if cur, _ := sw.current(); cur != "" {
		t.Fatal("started on an inter frame")
	}
	if !sw.push("h", vp8(3, 4000, true)) {
		t.Fatal("didn't start at the key frame")
	}
	for i := uint16(4); i < 10; i++ {
		sw.push("h", vp8(i, 4000+uint32(i-3)*3000, false))
		sw.push("l", vp8(500+i, 77000+uint32(i)*3000, i%3 == 0)) // the other layer is dropped
	}
	if sw.lastSeq != 9 || sw.lastTS != 4000+6*3000 {
		t.Fatalf("after h: seq %d ts %d", sw.lastSeq, sw.lastTS)
	}
	// Down to l: waits for l's key frame, then continues the numbers
	sw.choose("l")
	sw.push("h", vp8(10, 4000+7*3000, false))
	sw.push("l", vp8(600, 99000, false))
	if cur, _ := sw.current(); cur != "h" {
		t.Fatal("switched on an inter frame of l")
	}
	p := vp8(601, 99000+3000, true)
	if !sw.push("l", p) {
		t.Fatal("didn't switch at l's key frame")
	}
	if p.SequenceNumber != 11 {
		t.Fatalf("sequence not continuous: %d", p.SequenceNumber)
	}
	if p.Timestamp != 4000+7*3000+3000 {
		t.Fatalf("timestamp not continuous: %d", p.Timestamp)
	}
	if p.Header.Extension || len(p.Header.Extensions) != 0 {
		t.Fatal("the streamer's extensions went through")
	}
	// h is dropped now; l goes on
	if sw.push("h", vp8(12, 0, true)) || sw.lastSeq != 11 {
		t.Fatal("h still goes through")
	}
	p = vp8(602, 99000+6000, false)
	sw.push("l", p)
	if p.SequenceNumber != 12 {
		t.Fatalf("l's next: %d", p.SequenceNumber)
	}
	if sw.switches != 2 {
		t.Fatalf("switches: %d", sw.switches)
	}
}

func TestSwitcherWrap(t *testing.T) {
	sw, _ := testSwitcher(t)
	sw.choose("m")
	sw.push("m", vp8(65534, 10, true))
	sw.push("m", vp8(65535, 10, false))
	p := vp8(0, 3010, false)
	sw.push("m", p)
	if sw.lastSeq != 0 || p.SequenceNumber != 0 {
		t.Fatalf("wrap: last %d got %d", sw.lastSeq, p.SequenceNumber)
	}
	sw.choose("h")
	p = vp8(100, 500000, true)
	sw.push("h", p)
	if p.SequenceNumber != 1 {
		t.Fatalf("after the wrap: %d", p.SequenceNumber)
	}
}

func TestKeyFrames(t *testing.T) {
	cases := []struct {
		mime string
		p    []byte
		key  bool
	}{
		{webrtc.MimeTypeH264, []byte{0x65, 0x88}, true},                               // IDR
		{webrtc.MimeTypeH264, []byte{0x67, 0x42}, true},                               // SPS
		{webrtc.MimeTypeH264, []byte{0x41, 0x9a}, false},                              // a P slice
		{webrtc.MimeTypeH264, []byte{0x78, 0x00, 0x02, 0x67, 0x42, 0, 1, 0x68}, true}, // STAP-A with an SPS
		{webrtc.MimeTypeH264, []byte{0x78, 0x00, 0x01, 0x41}, false},
		{webrtc.MimeTypeH264, []byte{0x7c, 0x85, 0xb8}, true},  // FU-A start of an IDR
		{webrtc.MimeTypeH264, []byte{0x7c, 0x05, 0xb8}, false}, // FU-A middle
		{webrtc.MimeTypeH264, []byte{0x7c, 0x81, 0xb8}, false}, // FU-A start of a P slice
		{webrtc.MimeTypeVP8, []byte{0x10, 0x00, 0, 0}, true},
		{webrtc.MimeTypeVP8, []byte{0x10, 0x01, 0, 0}, false},
		{webrtc.MimeTypeVP8, []byte{0x00, 0x00, 0, 0}, false}, // not the start of a partition
		{webrtc.MimeTypeVP9, []byte{0x08, 0xaa}, true},        // B set, P clear
		{webrtc.MimeTypeVP9, []byte{0x48, 0xaa}, false},       // P set
		{webrtc.MimeTypeAV1, []byte{0x18, 0x0a}, true},        // N set
		{webrtc.MimeTypeAV1, []byte{0x10, 0x0a}, false},
		{webrtc.MimeTypeAV1, []byte{0x98, 0x0a}, false}, // a continuation
		{"video/other", []byte{0x18}, false},
		{webrtc.MimeTypeVP8, nil, false},
	}
	for i, c := range cases {
		if got := isKeyFrame(c.mime, c.p); got != c.key {
			t.Errorf("%d %s % x: key %v, want %v", i, c.mime, c.p, got, c.key)
		}
	}
}

func TestPickerRule(t *testing.T) {
	layers := []layerInfo{{"h", 6000}, {"m", 2000}, {"l", 600}}
	var pk picker
	t0 := time.Now()
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	// Off any layer: the best
	if got := pk.step("", layers, linkSample{}, at(0)); got != "h" {
		t.Fatalf("start: %s", got)
	}
	// Clean on h: stays
	for s := 1.0; s < 30; s++ {
		if got := pk.step("h", layers, linkSample{lossPct: 0.5, rembKbps: 9000}, at(s)); got != "h" {
			t.Fatalf("clean at %v: %s", s, got)
		}
	}
	// Loss: down only after 3 s
	for s := 30.0; s < 33; s++ {
		if got := pk.step("h", layers, linkSample{lossPct: 12}, at(s)); got != "h" {
			t.Fatalf("too early at %v: %s", s, got)
		}
	}
	if got := pk.step("h", layers, linkSample{lossPct: 12}, at(33)); got != "m" {
		t.Fatalf("after 3 s of loss: %s", got)
	}
	// Too little bandwidth for m (needs 2400): down after 3 s again
	pk = picker{}
	cur := "m"
	for s := 40.0; s <= 43; s++ {
		cur = pk.step(cur, layers, linkSample{rembKbps: 2100}, at(s))
	}
	if cur != "l" {
		t.Fatalf("bandwidth rule: %s", cur)
	}
	// Short trouble (2 s) then clean: no step
	pk = picker{}
	pk.step("m", layers, linkSample{lossPct: 20}, at(50))
	pk.step("m", layers, linkSample{lossPct: 20}, at(52))
	if got := pk.step("m", layers, linkSample{}, at(53)); got != "m" {
		t.Fatalf("trouble forgotten: %s", got)
	}
	// Clean for 15 s with room for h: up; without room: not
	pk = picker{}
	cur = "m"
	for s := 60.0; s < 75; s++ {
		cur = pk.step(cur, layers, linkSample{rembKbps: 8000}, at(s))
		if cur != "m" {
			t.Fatalf("up too early at %v", s)
		}
	}
	if cur = pk.step(cur, layers, linkSample{rembKbps: 8000}, at(75)); cur != "h" {
		t.Fatalf("after 15 s clean: %s", cur)
	}
	pk = picker{}
	cur = "m"
	for s := 80.0; s <= 100; s++ {
		cur = pk.step(cur, layers, linkSample{rembKbps: 6500}, at(s)) // under 6000×1.2
	}
	if cur != "m" {
		t.Fatalf("went up without bandwidth: %s", cur)
	}
	// NACKs count as loss; a few are fine
	pk = picker{}
	cur = "h"
	for s := 110.0; s <= 113; s++ {
		cur = pk.step(cur, layers, linkSample{nacks: 30}, at(s))
	}
	if cur != "m" {
		t.Fatalf("nack rule: %s", cur)
	}
	// On the lowest with loss: stays
	pk = picker{}
	cur = "l"
	for s := 120.0; s <= 130; s++ {
		cur = pk.step(cur, layers, linkSample{lossPct: 40}, at(s))
	}
	if cur != "l" {
		t.Fatalf("below the lowest: %s", cur)
	}
	// A layer gone (the encoder stopped it) isn't chosen
	if got := pk.step("x", []layerInfo{{"m", 2000}}, linkSample{}, at(140)); got != "m" {
		t.Fatalf("unknown layer: %s", got)
	}
	if got := pk.step("l", nil, linkSample{}, at(141)); got != "l" {
		t.Fatalf("no layers: %s", got)
	}
}

func TestLayerOrder(t *testing.T) {
	sc := &simulcast{layers: map[string]*layer{"l": {rid: "l"}, "h": {rid: "h"}, "m": {rid: "m"}}}
	now := time.Now()
	for _, l := range sc.layers {
		l.last.Store(now.UnixNano())
	}
	if got := strings.Join(sc.rids(), ""); got != "hml" {
		t.Fatalf("order: %s", got)
	}
	sc.layers["l"].last.Store(0) // l stopped
	if sc.nearest("l", now) != "m" || sc.nearest("h", now) != "h" || sc.nearest("auto", now) != "m" {
		t.Fatalf("nearest: %s %s", sc.nearest("l", now), sc.nearest("auto", now))
	}
	for _, l := range sc.layers {
		l.last.Store(0)
	}
	if sc.nearest("h", now) != "" {
		t.Fatal("nearest with nothing alive")
	}
}

// A simulcast streamer (three VP8 layers) through the room: a viewer gets one continuous picture, and its layer
// request is followed and reported.
func TestSimulcastRoom(t *testing.T) {
	s, ts := newTestServer(t, nil)
	ac, _ := dialV(t, ts, newKey(t), "Alice", "")
	vc, _ := dialV(t, ts, newKey(t), "Vio", "")
	room := ac.uid + ":sim"
	base := "ws" + strings.TrimPrefix(ts.URL, "http") + "/streams?token="
	pub := dial(t, base+url.QueryEscape(s.makeToken(ac.uid)))
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	var layers []*webrtc.TrackLocalStaticRTP
	for _, rid := range []string{"h", "m", "l"} {
		tr, err := webrtc.NewTrackLocalStaticRTP(codec, "video", "share", webrtc.WithRTPStreamID(rid))
		if err != nil {
			t.Fatal(err)
		}
		layers = append(layers, tr)
	}
	sender, err := pub.pc.AddTrack(layers[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.AddEncoding(layers[1]); err != nil {
		t.Fatal(err)
	}
	if err := sender.AddEncoding(layers[2]); err != nil {
		t.Fatal(err)
	}
	go pub.read(nil)
	offer, _ := pub.pc.CreateOffer(nil)
	_ = pub.pc.SetLocalDescription(offer)
	if !strings.Contains(offer.SDP, "a=simulcast:send h;m;l") {
		t.Fatalf("no simulcast in the offer:\n%s", offer.SDP)
	}
	pub.send(message{Type: "publish", Room: room, SDP: offer.SDP, Title: "Layers"})
	stop := make(chan bool)
	defer close(stop)
	go func() {
		// Pion's sender leaves the mid and rid extensions (what names a layer) to the writer: their ids come with
		// the answer
		var midID, ridID uint8
		for midID == 0 || ridID == 0 {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			for _, e := range sender.GetParameters().HeaderExtensions {
				switch e.URI {
				case sdp.SDESMidURI:
					midID = uint8(e.ID)
				case sdp.SDESRTPStreamIDURI:
					ridID = uint8(e.ID)
				}
			}
		}
		// Every layer: a key frame each 10 packets, its own numbering, the layer's size in the payload
		seq := [3]uint16{100, 20100, 40100}
		ts := [3]uint32{1000, 900000, 1800000}
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Millisecond):
			}
			for i, tr := range layers {
				p := vp8(seq[i], ts[i], n%10 == 0)
				p.Payload = append(p.Payload, make([]byte, 400/(i+1))...)
				_ = p.Header.SetExtension(midID, []byte("0"))
				_ = p.Header.SetExtension(ridID, []byte(tr.RID()))
				seq[i]++
				ts[i] += 3000
				_ = tr.WriteRTP(p)
			}
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		rm := s.rooms[room]
		s.mu.Unlock()
		n := 0
		if rm != nil {
			rm.mu.Lock()
			if rm.sim != nil {
				n = len(rm.sim.layers)
			}
			rm.mu.Unlock()
		}
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the layers never arrived (%d)", n)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The viewer: one track, continuous numbers
	sub := dial(t, base+url.QueryEscape(s.makeToken(vc.uid)))
	type got struct {
		seq  uint16
		size int
	}
	pkts := make(chan got, 4096)
	sub.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			pkts <- got{p.SequenceNumber, len(p.Payload)}
		}
	})
	status := make(chan message, 64)
	go func() {
		for {
			var m message
			if err := sub.ws.ReadJSON(&m); err != nil {
				return
			}
			switch m.Type {
			case "offer":
				_ = sub.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP})
				ans, _ := sub.pc.CreateAnswer(nil)
				_ = sub.pc.SetLocalDescription(ans)
				sub.send(message{Type: "answer", SDP: ans.SDP})
			case "candidate":
				_ = sub.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: m.Candidate, SDPMid: m.Mid, SDPMLineIndex: m.MLine})
			case "layer":
				status <- m
			}
		}
	}()
	sub.send(message{Type: "subscribe", Room: room})
	waitLayer := func(want string) {
		t.Helper()
		for {
			select {
			case m := <-status:
				if len(m.Layers) != 3 || m.Layers[0] != "h" || m.Layers[2] != "l" {
					t.Fatalf("layers reported: %v", m.Layers)
				}
				if m.Layer == want {
					return
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("the viewer was never told it gets %q", want)
			}
		}
	}
	waitLayer("h")
	var first got
	select {
	case first = <-pkts:
	case <-time.After(10 * time.Second):
		t.Fatal("no packets")
	}
	if first.size < 400 {
		t.Fatalf("auto didn't start on the big layer: %d bytes", first.size)
	}
	// Ask for the small layer: the numbers go on and the packets shrink
	sub.send(message{Type: "layer", Q: "l"})
	waitLayer("l")
	last := first.seq
	small, n := 0, 0
	deadline = time.Now().Add(10 * time.Second)
	for small < 20 && time.Now().Before(deadline) {
		select {
		case g := <-pkts:
			n++
			if d := int16(g.seq - last); d != 1 {
				t.Fatalf("sequence jumped by %d at packet %d", d, n)
			}
			last = g.seq
			if g.size < 200 {
				small++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the picture stopped")
		}
	}
	if small < 20 {
		t.Fatalf("the small layer never came (%d of %d)", small, n)
	}
	// A bad request is ignored; auto brings the best back
	sub.send(message{Type: "layer", Q: "xx"})
	sub.send(message{Type: "layer", Q: "auto"})
	waitLayer("h")
	_ = pub.pc.Close()
	_ = sub.pc.Close()
}
