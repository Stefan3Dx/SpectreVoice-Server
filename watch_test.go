package main

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// testStream: a streamer publishing a VP8 picture into its room, whose frames start when [frames] is closed.
type testStream struct {
	s       *server
	room    string
	viewers string // the /streams URL of the viewer's person
	stop    chan struct{}
}

func startTestStream(t *testing.T, frames <-chan struct{}) *testStream {
	t.Helper()
	s, ts := newTestServer(t, nil)
	ac, _ := dialV(t, ts, newKey(t), "Alice", "")
	vc, _ := dialV(t, ts, newKey(t), "Vio", "")
	base := "ws" + strings.TrimPrefix(ts.URL, "http") + "/streams?token="
	st := &testStream{s: s, room: ac.uid + ":screen", viewers: base + url.QueryEscape(s.makeToken(vc.uid)), stop: make(chan struct{})}
	pub := dial(t, base+url.QueryEscape(s.makeToken(ac.uid)))
	track, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "share")
	if _, err := pub.pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	go pub.read(nil)
	offer, _ := pub.pc.CreateOffer(nil)
	_ = pub.pc.SetLocalDescription(offer)
	pub.send(message{Type: "publish", Room: st.room, SDP: offer.SDP})
	vc.wait(func(m map[string]any) bool { return m["t"] == "live" && m["on"] == true })
	go func() {
		select {
		case <-frames:
		case <-st.stop:
			return
		}
		frame := make([]byte, 800)
		frame[0] = 0x10 // a VP8 key frame's first byte
		for {
			select {
			case <-st.stop:
				return
			case <-time.After(33 * time.Millisecond):
				_ = track.WriteSample(media.Sample{Data: frame, Duration: 33 * time.Millisecond})
			}
		}
	}()
	t.Cleanup(func() {
		close(st.stop)
		_ = pub.pc.Close()
		_ = pub.ws.Close()
	})
	return st
}

// watcher: a viewer of the room that reports its first packets on got, and gone when its connection ends.
type watcher struct {
	*client
	got  chan struct{}
	gone chan struct{}
}

func (st *testStream) watch(t *testing.T, dev string) *watcher {
	t.Helper()
	w := &watcher{client: dial(t, st.viewers), got: make(chan struct{}), gone: make(chan struct{})}
	w.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buf := make([]byte, 1500)
		for range 10 {
			if _, _, err := tr.Read(buf); err != nil {
				return
			}
		}
		close(w.got)
	})
	go func() {
		w.read(nil)
		close(w.gone)
	}()
	t.Cleanup(func() {
		_ = w.pc.Close()
		_ = w.ws.Close()
	})
	w.send(message{Type: "subscribe", Room: st.room, Dev: dev})
	return w
}

func (w *watcher) wantPicture(t *testing.T, what string) {
	t.Helper()
	select {
	case <-w.got:
	case <-w.gone:
		t.Fatalf("%s: dropped by the server", what)
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: no picture (%s)", what, w.pc.ConnectionState())
	}
}

// waitTracks: until the streamer's picture reached the server.
func (st *testStream) waitTracks(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st.s.mu.Lock()
		rm := st.s.rooms[st.room]
		st.s.mu.Unlock()
		if rm != nil {
			rm.mu.Lock()
			n := len(rm.tracks)
			rm.mu.Unlock()
			if n > 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream's picture never reached the server")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (st *testStream) viewerCount() int {
	st.s.mu.Lock()
	rm := st.s.rooms[st.room]
	st.s.mu.Unlock()
	if rm == nil {
		return -1
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return len(rm.subs)
}

// A viewer that comes between "X is live" (announced at the publish) and the streamer's first frame is kept
// and gets the picture once it comes: it was offered a connection with nothing in it and dropped ("no ice-ufrag").
func TestStreamWatchBeforeFirstFrame(t *testing.T) {
	frames := make(chan struct{})
	st := startTestStream(t, frames)
	w := st.watch(t, "")
	deadline := time.Now().Add(5 * time.Second)
	for st.viewerCount() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the early viewer never joined the room")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // the viewer waits in the room, nothing sent yet
	close(frames)
	w.wantPicture(t, "the viewer that came before the first frame")
}

// One watch per person per device: a new subscribe from the same person (its old connection dropped and still
// open on the server, for up to 75 s) replaces the old watch instead of being refused; another device of the same
// person watches next to it.
func TestStreamWatchReplacesOld(t *testing.T) {
	frames := make(chan struct{})
	close(frames)
	st := startTestStream(t, frames)
	st.waitTracks(t)
	old := st.watch(t, "phone")
	old.wantPicture(t, "the first watch")

	again := st.watch(t, "phone")
	again.wantPicture(t, "the same device watching again")
	select {
	case <-old.gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the old watch of the same device was kept")
	}
	if n := st.viewerCount(); n != 1 {
		t.Fatalf("%d viewers after the same device watched again, want 1", n)
	}

	pc := st.watch(t, "pc")
	pc.wantPicture(t, "the person's other device")
	if n := st.viewerCount(); n != 2 {
		t.Fatalf("%d viewers with two devices, want 2", n)
	}
	select {
	case <-again.gone:
		t.Fatal("another device's watch closed the phone's")
	case <-time.After(300 * time.Millisecond):
	}

	// An app that doesn't name its device (older ones): the person's watch is replaced, as before it was refused
	plain := st.watch(t, "")
	plain.wantPicture(t, "a watch without a device")
	if n := st.viewerCount(); n != 1 {
		t.Fatalf("%d viewers after a watch without a device, want 1", n)
	}
}

// The apps' voice offers in their real shapes: libwebrtc (the PC's webrtc-java, Android, Chrome and Safari in the
// web client) puts a=fingerprint in each media section with CRLF line ends; Firefox once at the session level. A
// new connection of the app's has another certificate (none of them pins one), an ICE restart or renegotiation of
// the same connection keeps it: only the first must count as another connection (baee460).
func TestVoiceOfferFingerprintShapes(t *testing.T) {
	const fpA = "sha-256 4F:1C:9A:22:7B:E0:31:5D:88:AC:10:6E:F2:93:47:B5:0C:D9:61:2A:7E:13:C8:55:9F:04:BB:E6:3A:70:D1:28"
	const fpB = "sha-256 A0:57:3E:C1:09:9D:72:4B:E8:16:F3:2C:85:6A:D0:47:1B:BE:93:58:0F:C4:6D:21:7A:E5:38:9C:02:F1:4D:B6"
	libwebrtc := func(fp, ufrag string) string {
		return strings.ReplaceAll("v=0\no=- 4611731400430051336 2 IN IP4 127.0.0.1\ns=-\nt=0 0\na=group:BUNDLE 0\na=extmap-allow-mixed\n"+
			"a=msid-semantic: WMS\nm=audio 9 UDP/TLS/RTP/SAVPF 111\nc=IN IP4 0.0.0.0\na=rtcp:9 IN IP4 0.0.0.0\n"+
			"a=ice-ufrag:"+ufrag+"\na=ice-pwd:asd88fgpdd777uzjYhagZg\na=ice-options:trickle\na=fingerprint:"+fp+"\n"+
			"a=setup:actpass\na=mid:0\na=sendrecv\na=rtcp-mux\na=rtpmap:111 opus/48000/2\n", "\n", "\r\n")
	}
	firefox := func(fp, ufrag string) string {
		return "v=0\r\no=mozilla...THIS_IS_SDPARTA-99.0 1 0 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\na=fingerprint:" + strings.ToUpper(fp[:8]) + fp[8:] + "\r\n" +
			"a=group:BUNDLE 0\r\na=ice-options:trickle\r\na=msid-semantic:WMS *\r\nm=audio 9 UDP/TLS/RTP/SAVPF 109\r\nc=IN IP4 0.0.0.0\r\n" +
			"a=sendrecv\r\na=ice-pwd:9a1b2c3d4e5f60718293a4b5c6d7e8f9\r\na=ice-ufrag:" + ufrag + "\r\na=mid:0\r\na=rtcp-mux\r\na=rtpmap:109 opus/48000/2\r\na=setup:actpass\r\n"
	}
	for name, mk := range map[string]func(string, string) string{"libwebrtc": libwebrtc, "firefox": firefox} {
		first, restart, fresh := mk(fpA, "Ab12"), mk(fpA, "Zx98"), mk(fpB, "Qw34")
		if got := sdpFingerprint(first); got != strings.ToLower(fpA) {
			t.Fatalf("%s: fingerprint %q", name, got)
		}
		if sdpFingerprint(first) != sdpFingerprint(restart) {
			t.Fatalf("%s: an ICE restart of the same connection looks like another connection", name)
		}
		if sdpFingerprint(first) == sdpFingerprint(fresh) {
			t.Fatalf("%s: a new connection looks like the old one", name)
		}
	}
	// The same connection's offer in another casing of the hex (Firefox upper, a lower-case echo) is the same
	if sdpFingerprint(libwebrtc(fpA, "x")) != sdpFingerprint(firefox(fpA, "y")) {
		t.Fatal("the fingerprint's case counts")
	}
	if sdpFingerprint("v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n") != "" {
		t.Fatal("an offer without a fingerprint")
	}
}
