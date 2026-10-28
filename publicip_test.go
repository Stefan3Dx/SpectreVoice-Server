package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// candidatesOf: the server side's gathered candidates for a fresh audio connection.
func candidatesOf(t *testing.T, s *server) string {
	t.Helper()
	pc, err := s.webrtcAPI().NewPeerConnection(webrtc.Configuration{})
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

// A DDNS name in SV_PUBLIC_IP that moves (a home connection's new address): new connections announce the new one.
func TestPublicIPFollowsDDNS(t *testing.T) {
	addr := "203.0.113.7"
	var mu sync.Mutex
	lookupIP = func(string) ([]net.IP, error) {
		mu.Lock()
		defer mu.Unlock()
		return []net.IP{net.ParseIP(addr)}, nil
	}
	defer func() { lookupIP = net.LookupIP }()
	s, _ := newTestServer(t, func(c *config) { c.udpPort = 41756; c.publicIP = "home.example.net" })
	if sdp := candidatesOf(t, s); !strings.Contains(sdp, "203.0.113.7 41756 typ host") {
		t.Fatalf("first address not offered:\n%s", sdp)
	}
	if s.refreshPublicIP() {
		t.Fatal("rebuilt though the name didn't move")
	}
	mu.Lock()
	addr = "198.51.100.9"
	mu.Unlock()
	if !s.refreshPublicIP() {
		t.Fatal("the move wasn't seen")
	}
	sdp := candidatesOf(t, s)
	if !strings.Contains(sdp, "198.51.100.9 41756 typ host") || strings.Contains(sdp, "203.0.113.7") {
		t.Fatalf("after the move:\n%s", sdp)
	}
	// A failed lookup keeps the address
	lookupIP = func(string) ([]net.IP, error) { return nil, errors.New("dns down") }
	if s.refreshPublicIP() {
		t.Fatal("a failed lookup changed the address")
	}
}

// With SV_TURN_SECRET each person gets TURN credentials of their own that end (coturn's TURN REST scheme).
func TestTurnCredentials(t *testing.T) {
	s, _ := newTestServer(t, func(c *config) { c.turn = "turn:t.example.com:3478"; c.turnSecret = "s3cret" })
	ice := s.publicICE("U1")
	if len(ice) != 1 {
		t.Fatalf("ice %v", ice)
	}
	user, pass := ice[0]["username"].(string), ice[0]["credential"].(string)
	exp, uid, ok := strings.Cut(user, ":")
	n, err := strconv.ParseInt(exp, 10, 64)
	if !ok || uid != "U1" || err != nil || n < time.Now().Unix() || n > time.Now().Add(turnCredLife+time.Minute).Unix() {
		t.Fatalf("username %q", user)
	}
	m := hmac.New(sha1.New, []byte("s3cret"))
	m.Write([]byte(user))
	if pass != base64.StdEncoding.EncodeToString(m.Sum(nil)) {
		t.Fatalf("credential %q isn't the HMAC of %q", pass, user)
	}
	// Without the secret: the fixed user and password, as before
	s.cfg.turnSecret, s.cfg.turnUser, s.cfg.turnPass = "", "spectre", "pw"
	if ice := s.publicICE("U1"); ice[0]["username"] != "spectre" || ice[0]["credential"] != "pw" {
		t.Fatalf("fixed credentials: %v", ice)
	}
}
