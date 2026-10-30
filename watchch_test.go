package main

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// chWatcher: a viewer of st's room that remembers why the server ended it.
type chWatcher struct {
	*client
	got   chan struct{}
	ended chan string // the "ended" message's why ("" none)
	gone  chan struct{}
}

func (st *testStream) watchCh(t *testing.T) *chWatcher {
	t.Helper()
	w := &chWatcher{client: dial(t, st.viewers), got: make(chan struct{}), ended: make(chan string, 4), gone: make(chan struct{})}
	w.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buf := make([]byte, 1500)
		for range 5 {
			if _, _, err := tr.Read(buf); err != nil {
				return
			}
		}
		close(w.got)
	})
	go func() {
		defer close(w.gone)
		for {
			var m message
			if err := w.ws.ReadJSON(&m); err != nil {
				return
			}
			switch m.Type {
			case "offer":
				_ = w.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP})
				ans, _ := w.pc.CreateAnswer(nil)
				_ = w.pc.SetLocalDescription(ans)
				w.send(message{Type: "answer", SDP: ans.SDP})
			case "candidate":
				_ = w.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: m.Candidate, SDPMid: m.Mid, SDPMLineIndex: m.MLine})
			case "ended":
				w.ended <- m.Why
			}
		}
	}()
	t.Cleanup(func() { _ = w.pc.Close(); _ = w.ws.Close() })
	w.send(message{Type: "subscribe", Room: st.room})
	return w
}

func (w *chWatcher) wantPicture(t *testing.T, what string) {
	t.Helper()
	select {
	case <-w.got:
	case why := <-w.ended:
		t.Fatalf("%s: ended (%q)", what, why)
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: no picture", what)
	}
}

func (w *chWatcher) wantEnded(t *testing.T, what string, within time.Duration) {
	t.Helper()
	select {
	case why := <-w.ended:
		if why != "channel" {
			t.Fatalf("%s: ended with why %q, not channel", what, why)
		}
	case <-w.gone:
		// The server says why and then closes: both may be ready by now, and select picks either
		select {
		case why := <-w.ended:
			if why != "channel" {
				t.Fatalf("%s: ended with why %q, not channel", what, why)
			}
		default:
			t.Fatalf("%s: closed without saying why", what)
		}
	case <-time.After(within):
		t.Fatalf("%s: still watching", what)
	}
}

func liveCh(room string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		s, _ := m["s"].(map[string]any)
		return m["t"] == "live" && m["on"] == true && s["room"] == room
	}
}

// A stream is watched from its streamer's channel: a viewer who leaves it loses the watch (told why), can't watch
// from another channel, and can again once back; the live entry names the channel, told again when the streamer
// moves, which drops the viewers left behind.
func TestWatchFromStreamersChannel(t *testing.T) {
	frames := make(chan struct{})
	close(frames)
	st := startTestStream(t, frames)
	st.waitTracks(t)
	home := chanID(t, st.vc.wel, "Lobby")
	afk := chanID(t, st.vc.wel, "AFK")
	st.s.hub.mu.Lock()
	ls := st.s.hub.live[st.room]
	st.s.hub.mu.Unlock()
	if ls.Ch != home {
		t.Fatalf("the live entry's channel: %d, want %d", ls.Ch, home)
	}

	w := st.watchCh(t)
	w.wantPicture(t, "same channel")
	// The viewer goes to another channel: its watch ends
	st.vc.send(map[string]any{"t": "join", "ch": afk})
	w.wantEnded(t, "viewer moved", 3*time.Second)
	// From there it can't watch
	w2 := st.watchCh(t)
	w2.wantEnded(t, "watch from another channel", 3*time.Second)
	// Back in the streamer's channel: it can
	st.vc.send(map[string]any{"t": "join", "ch": home})
	time.Sleep(300 * time.Millisecond)
	w3 := st.watchCh(t)
	w3.wantPicture(t, "back in the channel")
	// The streamer moves away: the live entry says where, the viewer left behind is dropped
	st.ac.send(map[string]any{"t": "join", "ch": afk})
	m := st.vc.wait(func(m map[string]any) bool {
		s, _ := m["s"].(map[string]any)
		return liveCh(st.room)(m) && s["ch"] == float64(afk)
	})
	if m == nil {
		t.Fatal("not told the stream's new channel")
	}
	w3.wantEnded(t, "streamer moved", 3*time.Second)
}

// A viewer whose session goes keeps its watch a while (a reconnect), then loses it.
func TestWatchEndsAfterLeaving(t *testing.T) {
	frames := make(chan struct{})
	close(frames)
	st := startTestStream(t, frames)
	st.s.hub.mu.Lock()
	st.s.hub.watchGrace = 1500 * time.Millisecond
	st.s.hub.mu.Unlock()
	st.waitTracks(t)
	w := st.watchCh(t)
	w.wantPicture(t, "watching")
	_ = st.vc.ws.Close() // the viewer's app leaves the server (its stream connection stays open)
	select {
	case why := <-w.ended:
		t.Fatalf("ended at once (%q): a reconnect would lose it", why)
	case <-time.After(700 * time.Millisecond):
	}
	w.wantEnded(t, "viewer left the server", 5*time.Second)
}

// P2P: a join from another channel is refused (why "channel"); a viewer that moves away, or a streamer that does,
// ends the pair: both sides are told.
func TestP2PFromStreamersChannel(t *testing.T) {
	_, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	cara, _ := dialV(t, hs, newKey(t), "Cara", "")
	home := chanID(t, ana.wel, "Lobby")
	afk := chanID(t, ana.wel, "AFK")
	room := ana.uid
	ana.send(map[string]any{"t": "p2p-live", "room": room, "on": true, "title": "Game"})
	lv := bob.wait(liveOn(room))
	if s := lv["s"].(map[string]any); s["ch"] != float64(home) {
		t.Fatal("the P2P stream's channel:", lv)
	}
	// From another channel: refused, saying why
	bob.send(map[string]any{"t": "join", "ch": afk})
	time.Sleep(200 * time.Millisecond)
	bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	if m := bob.wait(isT("p2p-bye")); m["why"] != "channel" {
		t.Fatal(m)
	}
	if !ana.quiet(300*time.Millisecond, isP2P("p2p-join", bob.sid)) {
		t.Fatal("a join from another channel reached the streamer")
	}
	// In the channel: joins; then moves away: both told
	bob.send(map[string]any{"t": "join", "ch": home})
	time.Sleep(200 * time.Millisecond)
	bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	ana.wait(isP2P("p2p-join", bob.sid))
	bob.send(map[string]any{"t": "join", "ch": afk})
	if m := bob.wait(isP2P("p2p-bye", ana.sid)); m["why"] != "channel" {
		t.Fatal(m)
	}
	if m := ana.wait(isP2P("p2p-bye", bob.sid)); m["why"] != "channel" {
		t.Fatal(m)
	}
	// The streamer moves: a viewer left in the old channel is dropped
	cara.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	ana.wait(isP2P("p2p-join", cara.sid))
	ana.send(map[string]any{"t": "join", "ch": afk})
	if m := cara.wait(isP2P("p2p-bye", ana.sid)); m["why"] != "channel" {
		t.Fatal(m)
	}
	// And Bob, in that channel now, may join
	bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	ana.wait(isP2P("p2p-join", bob.sid))
}

// The server's TV isn't a person's stream: watched from any channel.
func TestTVFromAnyChannel(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	ana.send(map[string]any{"t": "join", "ch": chanID(t, ana.wel, "AFK")})
	time.Sleep(200 * time.Millisecond)
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if !s.hub.inStreamChannel(ana.uid, liveStream{Room: tvRoom, UID: "someone"}, false) {
		t.Fatal("the TV needs a channel")
	}
}
