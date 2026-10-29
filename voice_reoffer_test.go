package main

import (
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/pion/webrtc/v4"
)

// After many voices came and went, the app's own re-offer (ICE restart, quality change) is still taken: the limit
// on its media sections counts the connection's transceivers (they stay), not the voices sent now.
func TestVoiceReofferAfterChurn(t *testing.T) {
	_, hs := newTestServer(t, nil)
	a, _ := dialV(t, hs, newKey(t), "A", "")
	va := newVoiceClient(t, a)
	stop := make(chan struct{})
	defer close(stop)
	errs := make(chan map[string]any, 10)
	answers := make(chan struct{}, 10)
	go func() {
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
						_ = va.pc.SetRemoteDescription(desc)
						select {
						case answers <- struct{}{}:
						default:
						}
						continue
					}
					desc.Type = webrtc.SDPTypeOffer
					_ = va.pc.SetRemoteDescription(desc)
					ans, _ := va.pc.CreateAnswer(nil)
					_ = va.pc.SetLocalDescription(ans)
					a.send(map[string]any{"t": "rtc", "type": "answer", "sdp": ans.SDP})
				}
			}
		}
	}()
	var others []*tclient
	for i := 0; i < 9; i++ {
		c, _ := dialV(t, hs, newKey(t), "P"+itoa(i), "")
		v := newVoiceClient(t, c)
		go v.pump(stop)
		go func() {
			seq := uint16(0)
			for {
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
				seq++
				_ = v.track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte{0xf8, 0xff, 0xfe}})
			}
		}()
		others = append(others, c)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(va.pc.GetTransceivers()) < 10 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("A's transceivers with 9 others: %d", len(va.pc.GetTransceivers()))
	for _, c := range others {
		_ = c.ws.Close()
	}
	time.Sleep(3 * time.Second)
	// A's app re-offers (ICE restart after a network change)
	for va.pc.SignalingState() != webrtc.SignalingStateStable {
		time.Sleep(50 * time.Millisecond)
	}
	o, err := va.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = va.pc.SetLocalDescription(o)
	if n := mlines(o.SDP); n <= 8 {
		t.Fatalf("the re-offer has only %d media sections: the churn didn't happen", n)
	}
	for len(answers) > 0 {
		<-answers
	}
	a.send(map[string]any{"t": "rtc", "type": "offer", "sdp": o.SDP})
	select {
	case e := <-errs:
		t.Fatalf("re-offer refused: %v", e)
	case <-answers:
	case <-time.After(5 * time.Second):
		t.Fatal("no answer to the re-offer")
	}
}
