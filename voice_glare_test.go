package main

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// glareRun: A is in voice and connected; B comes and talks, so the server offers B's voice to A, and A offers at
// the same moment (a far app: its new voice level crosses the server's offer). polite: A gives way as apps 0.9.1+
// do (its offer is taken back, the server's answered, its own offered again after); else A keeps its own offer and
// ignores the server's, as older apps do.
func glareRun(t *testing.T, polite bool) (s *server, va *voiceClient, a, b *tclient, before *voicePeer, errs chan map[string]any, answers chan struct{}, stop chan struct{}) {
	s, hs := newTestServer(t, nil)
	a, _ = dialV(t, hs, newKey(t), "A", "")
	va = newVoiceClient(t, a)
	stop = make(chan struct{})
	t.Cleanup(func() { close(stop) })
	errs = make(chan map[string]any, 10)
	answers = make(chan struct{}, 10)
	crossed := make(chan struct{})
	var keep bool
	again := false // polite A: its own offer still to make after answering the server's
	go func() {
		ignoredOne := false
		for {
			select {
			case <-stop:
				return
			case m, ok := <-a.in:
				if !ok {
					return
				}
				switch m["t"] {
				case "error", "rtc-reset":
					errs <- m
				case "ice":
					c := webrtc.ICECandidateInit{Candidate: m["c"].(string)}
					if mid, ok := m["mid"].(string); ok {
						c.SDPMid = &mid
					}
					_ = va.pc.AddICECandidate(c)
				case "rtc":
					desc := webrtc.SessionDescription{SDP: m["sdp"].(string)}
					if m["type"] == "answer" {
						desc.Type = webrtc.SDPTypeAnswer
						if err := va.pc.SetRemoteDescription(desc); err != nil {
							t.Errorf("answer: %v", err)
						}
						answers <- struct{}{}
						continue
					}
					if !ignoredOne && va.pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
						// The server offers B's voice: A offers at the same moment
						ignoredOne = true
						keep, _ = m["keep"].(bool)
						o, err := va.pc.CreateOffer(nil)
						if err != nil {
							t.Errorf("app's offer: %v", err)
							continue
						}
						msg := map[string]any{"t": "rtc", "type": "offer", "sdp": o.SDP}
						if polite {
							// Pion can't roll back: a polite app's state after its rollback is that of an offer never set
							msg["polite"] = true
						} else if err := va.pc.SetLocalDescription(o); err != nil {
							t.Errorf("app's offer: %v", err)
						}
						a.send(msg)
						close(crossed)
						if !polite {
							continue // an older app keeps its own and ignores the server's
						}
						again = true
					}
					desc.Type = webrtc.SDPTypeOffer
					if err := va.pc.SetRemoteDescription(desc); err != nil {
						t.Errorf("server's offer: %v", err)
						continue
					}
					ans, _ := va.pc.CreateAnswer(nil)
					_ = va.pc.SetLocalDescription(ans)
					a.send(map[string]any{"t": "rtc", "type": "answer", "sdp": ans.SDP})
					if again {
						again = false
						o, _ := va.pc.CreateOffer(nil)
						_ = va.pc.SetLocalDescription(o)
						a.send(map[string]any{"t": "rtc", "type": "offer", "sdp": o.SDP, "polite": true})
					}
				}
			}
		}
	}()
	select {
	case <-answers:
	case <-time.After(10 * time.Second):
		t.Fatal("no answer to A's first offer")
	}
	deadline := time.Now().Add(10 * time.Second)
	for va.pc.ConnectionState() != webrtc.PeerConnectionStateConnected && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if before = s.voice.peer(a.sid); before == nil {
		t.Fatal("A has no voice peer")
	}
	b, _ = dialV(t, hs, newKey(t), "B", "")
	vb := newVoiceClient(t, b)
	go vb.pump(stop)
	go func() {
		seq := uint16(0)
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			seq++
			_ = vb.track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte{0xf8, 0xff, 0xfe}})
		}
	}()
	select {
	case <-crossed:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never offered B's voice to A")
	}
	if !keep {
		t.Fatal(`the server's offer doesn't say it keeps it when offers cross ("keep")`)
	}
	return
}

// A polite app (0.9.1+): the crossing costs nothing. The same connection stays up, the server's offer is answered,
// the app's own goes out after and is answered, and A hears B.
func TestVoiceCrossedOffersPoliteApp(t *testing.T) {
	s, va, a, b, before, errs, answers, _ := glareRun(t, true)
	select {
	case e := <-errs:
		t.Fatalf("the crossed offers broke A's voice: %v", e)
	case <-answers:
	case <-time.After(5 * time.Second):
		t.Fatal("no answer to A's offer made again after the crossing")
	}
	if s.voice.peer(a.sid) != before {
		t.Fatal("the server made a new peer for A")
	}
	deadline := time.Now().Add(10 * time.Second)
	for va.count("s"+itoa(b.sid)) < 10 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := va.count("s" + itoa(b.sid)); n < 10 {
		t.Fatalf("A got %d packets of B's voice after the crossing", n)
	}
	select {
	case e := <-errs:
		t.Fatalf("A's voice was reset: %v", e)
	default:
	}
	if st := va.pc.ConnectionState(); st != webrtc.PeerConnectionStateConnected {
		t.Fatalf("A's connection is %s", st)
	}
	if st := va.pc.SignalingState(); st != webrtc.SignalingStateStable {
		t.Fatalf("A's signalling is %s", st)
	}
}

// An app that keeps its own offer (0.9.0 and older): Pion can't roll the server's back, so the app is told to make
// a clean new connection (rtc-reset) instead of an "offer: invalid state" and a half-made peer.
func TestVoiceCrossedOffersOldApp(t *testing.T) {
	s, _, a, _, before, errs, _, _ := glareRun(t, false)
	select {
	case e := <-errs:
		if e["t"] != "rtc-reset" {
			t.Fatalf("expected rtc-reset, got %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no rtc-reset for an app whose offer crossed the server's")
	}
	if p := s.voice.peer(a.sid); p == before {
		t.Fatal("the old peer is still kept")
	}
}
