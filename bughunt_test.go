package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
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

// sinkConn: a /streams connection whose messages go nowhere (tests that call the room's functions directly).
func sinkConn(t *testing.T) *conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return &conn{ws: ws}
}

// A streamer's track that arrives while a viewer is being made reaches that viewer too (OnTrack only gave it to
// the viewers already in the room, and the new one had read the tracks before it).
func TestStreamTrackDuringSubscribe(t *testing.T) {
	s, _ := newTestServer(t, nil)
	rm := &room{name: "x", subs: map[*subscriber]bool{}, keyAt: map[webrtc.SSRC]time.Time{}, stop: make(chan struct{})}
	late, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "a", "spectre")
	if err != nil {
		t.Fatal(err)
	}
	subscribeGap = func(rm *room) {
		rm.mu.Lock()
		rm.tracks = append(rm.tracks, late) // what OnTrack does, with nobody in rm.subs to give it to
		rm.mu.Unlock()
	}
	defer func() { subscribeGap = nil }()
	sub, err := s.subscribe(rm, sinkConn(t), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.pc.Close()
	for _, snd := range sub.pc.GetSenders() {
		if snd.Track() == late {
			return
		}
	}
	t.Fatal("the viewer never got the track that came while it joined")
}

// A song asked for right after !stop (or as the idle bot leaves) isn't lost in the bot that's going.
func TestMusicPlayAfterStop(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.music = true; c.ytdlp = "sh"; c.ffmpeg = "sh" })
	if !s.music.enabled() {
		t.Skip("no sh")
	}
	a, _ := dialV(t, hs, newKey(t), "A", "")
	lobby := chanID(t, a.wel, "Lobby")
	b, err := s.music.botOf(spaceID, lobby)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.stopped = true // what !stop does, before the bot's loop takes it away
	b.mu.Unlock()
	nb, err := s.music.botOf(spaceID, lobby)
	if err != nil {
		t.Fatal(err)
	}
	if nb == b {
		t.Fatal("a stopped bot was given the next song (it would drop it)")
	}
}
