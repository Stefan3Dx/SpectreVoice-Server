package main

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

// An app that makes a brand-new voice connection (its old one broke on its side first) and offers it on the same
// session must get voice again: the server's old peer can't take another connection's offer.
func TestVoiceFreshConnectionSameSession(t *testing.T) {
	_, hs := newTestServer(t, nil)
	a, _ := dialV(t, hs, newKey(t), "A", "")
	b, _ := dialV(t, hs, newKey(t), "B", "")
	va, vb := newVoiceClient(t, a), newVoiceClient(t, b)
	stopA := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go va.pump(stopA)
	go vb.pump(stop)
	speak := func(vc *voiceClient) {
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
			_ = vc.track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte{0xf8, 0xff, 0xfe}})
		}
	}
	go speak(vb)
	want := "s" + itoa(b.sid)
	deadline := time.Now().Add(15 * time.Second)
	for va.count(want) < 10 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if va.count(want) < 10 {
		t.Fatalf("A didn't hear B at first")
	}
	// A's side broke: a new connection, same session
	close(stopA)
	time.Sleep(200 * time.Millisecond)
	_ = va.pc.Close()
	va2 := newVoiceClient(t, a)
	go va2.pump(stop)
	deadline = time.Now().Add(15 * time.Second)
	for va2.count(want) < 10 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := va2.count(want); n < 10 {
		t.Fatalf("A's new connection got %d packets of B (%s)", n, va2.pc.ConnectionState())
	}
}
