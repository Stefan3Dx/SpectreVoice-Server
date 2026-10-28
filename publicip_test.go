package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// candidatesOf: the server side's gathered candidates for a fresh audio connection.
func candidatesOf(t *testing.T, s *server) string {
	t.Helper()
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gathering timed out")
	}
	return pc.LocalDescription().SDP
}

// Behind a router with one forwarded UDP port: the public address must be offered on that very port
// (a mapped srflx candidate used to land on a random port no router forwards: the outside never connected).
func TestPublicIPOnSingleUDPPort(t *testing.T) {
	s, _ := newTestServer(t, func(c *config) { c.udpPort = 41755; c.publicIP = "203.0.113.7" })
	sdp := candidatesOf(t, s)
	if !strings.Contains(sdp, "203.0.113.7 41755 typ host") {
		t.Fatalf("public address not offered on the voice port:\n%s", sdp)
	}
	for _, l := range strings.Split(sdp, "\n") {
		if strings.Contains(l, "a=candidate") && strings.Contains(l, "udp") && !strings.Contains(l, " 41755 ") {
			t.Fatalf("UDP candidate off the voice port: %s", l)
		}
	}
}

// With a port range the public address stays an srflx candidate inside the range.
func TestPublicIPOnPortRange(t *testing.T) {
	s, _ := newTestServer(t, func(c *config) { c.publicIP = "203.0.113.7" })
	sdp := candidatesOf(t, s)
	if !strings.Contains(sdp, "203.0.113.7") {
		t.Fatalf("public address not offered:\n%s", sdp)
	}
}
