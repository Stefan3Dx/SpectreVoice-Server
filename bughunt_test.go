package main

import (
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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

// The same, with the old connection never closed: the server's peer still looks connected.
func TestVoiceFreshConnectionOldOpen(t *testing.T) {
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
	// The old one isn't closed (its network broke: no DTLS close reaches the server)
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

// One person can't keep thousands of push tokens (each notification would go to every one of them).
func TestPushTokensPerPerson(t *testing.T) {
	s, hs := newTestServer(t, nil)
	if err := s.store.migratePush(); err != nil {
		t.Fatal(err)
	}
	s.push = &pusher{st: s.store, queue: make(chan pushJob, 1)} // kept, never sent
	a, _ := dialV(t, hs, newKey(t), "A", "")
	for i := range 40 {
		a.send(map[string]any{"t": "push-token", "token": fmt.Sprintf("tok-%030d", i), "platform": "android"})
	}
	a.send(map[string]any{"t": "ping", "n": 1})
	a.wait(isT("pong"))
	if n := len(s.store.pushTokens(a.uid)); n > maxPushTokens {
		t.Fatalf("%d push tokens kept for one person", n)
	}
	toks := s.store.pushTokens(a.uid)
	if !slices.Contains(toks, fmt.Sprintf("tok-%030d", 39)) {
		t.Fatalf("the newest token went: %v", toks)
	}
}

// Every message, with fields of every JSON type (and odd values), from an owner (who passes the permission
// checks): no panic takes the server down, and it still answers.
func TestMessagesOfAnyShape(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	_, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	a, _ := dialV(t, hs, ownerKey, "Owner", "")
	b, _ := dialV(t, hs, newKey(t), "B", "")
	ops := []string{"ban", "bans", "bot-add", "bot-del", "bots", "cf-del", "cf-list", "cf-mkdir", "cf-rename", "ch-ban", "ch-del",
		"ch-kick", "ch-save", "ch-unban", "claim", "community-join", "community-save", "dms", "edit", "history", "ice", "invite-create",
		"invite-del", "invite-use", "invites", "join", "kick", "member-roles", "move", "mute", "name", "p2p-answer", "p2p-bye", "p2p-ice",
		"p2p-join", "p2p-live", "p2p-offer", "ping", "poke", "push-off", "push-token", "relay", "role", "role-del", "role-save", "rtc",
		"say", "search", "server-ban", "server-save", "server-unban", "slots", "state", "talker", "unban", "unmute", "unsay",
		"voice-mute", "voice-unmute", "whisper", "music", "admin-link", "tv", "enter", "nothing-like-this"}
	fields := []string{"ch", "to", "text", "id", "uid", "sid", "sids", "chs", "on", "name", "password", "room", "sdp", "type", "c",
		"mid", "mline", "q", "before", "after", "around", "perms", "roles", "minutes", "folder", "code", "pass", "hours", "uses",
		"data", "parent", "pos", "max", "kind", "icon", "pic", "quality", "default", "moderated", "cmd", "candidate", "mic", "snd",
		"away", "awayMsg", "rtt", "loss", "token", "platform", "invite", "about", "color", "why", "title", "url"}
	values := []any{nil, true, false, 0, -1, 1e300, -1e300, 3.5, "", "x", "c:1", "u:" + b.uid, "g:1", "*", b.uid, strings.Repeat("é", 5000),
		[]any{}, []any{1, "a", nil, []any{}}, map[string]any{}, map[string]any{"a": []any{1}}, float64(b.sid)}
	makes := map[string]bool{"invite-create": true, "ch-save": true, "role-save": true, "cf-mkdir": true, "bot-add": true, "say": true, "server-save": true, "community-save": true}
	n := 0
	for _, op := range ops {
		for fi, f := range fields {
			if raceOn && fi%4 != len(op)%4 {
				continue // under the race detector a quarter of the fields, a different quarter for each message
			}
			for _, v := range values {
				if makes[op] && f != "name" && f != "ch" && f != "uid" { // each one adds a row: a few are enough
					continue
				}
				a.send(map[string]any{"t": op, f: v})
				n++
				if n%60 == 0 {
					a.send(map[string]any{"t": "ping", "n": n})
					a.wait(func(m map[string]any) bool { return m["t"] == "pong" && m["n"] == float64(n) })
					a.seen = nil
				}
			}
		}
		// A few with everything at once
		m := map[string]any{"t": op}
		for i, f := range fields {
			if v, ok := values[(i*5+len(op))%len(values)].(string); !ok || len(v) < 1000 { // under the 64 KB a message may be
				m[f] = values[(i*5+len(op))%len(values)]
			}
		}
		a.send(m)
	}
	time.Sleep(500 * time.Millisecond)
	// Both still connected and served (A may have banned or kicked B: then B reconnects)
	a.send(map[string]any{"t": "ping", "n": -7})
	a.wait(func(m map[string]any) bool { return m["t"] == "pong" && m["n"] == float64(-7) })
	a.send(map[string]any{"t": "server-save", "password": ""}) // the run above set one
	a.wait(func(m map[string]any) bool { return m["t"] == "done" && m["op"] == "server-save" })
	c, e := dialV(t, hs, newKey(t), "C", "")
	if c == nil {
		t.Fatal(e)
	}
	c.send(map[string]any{"t": "ping", "n": 1})
	c.wait(isT("pong"))
}

// An owner can't server-ban themselves (they'd be locked out of their own server until the command line).
func TestServerBanNotOneself(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	a, _ := dialV(t, hs, ownerKey, "Owner", "")
	a.send(map[string]any{"t": "server-ban", "uid": a.uid})
	if e := a.wait(isT("error")); e["why"] != "role" {
		t.Fatal(e)
	}
	if s.mods.banned(a.uid, 0) {
		t.Fatal("the owner banned themselves")
	}
}

// A chat file's "space" is the server's own (or a known one): a made-up name isn't a quota of its own.
func TestFileSpaceQuotaNotByName(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.files, c.fileMB, c.fileDays = true, 10, 1 })
	s.files.spaceQuota = 100
	a, _ := dialV(t, hs, newKey(t), "A", "")
	up := func(space string) int {
		req, _ := http.NewRequest("POST", hs.URL+"/files?token="+a.tok+"&name=a.txt&space="+space, strings.NewReader(strings.Repeat("x", 80)))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := up("made-up-1"); c != 200 {
		t.Fatal(c)
	}
	if c := up("made-up-2"); c != http.StatusInsufficientStorage {
		t.Fatalf("a made-up space got its own quota: %d", c)
	}
}
