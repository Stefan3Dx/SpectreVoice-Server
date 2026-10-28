package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

func TestRTPRewriter(t *testing.T) {
	r := &rtpRewriter{clock: 90000}
	at := time.Unix(1000, 0)
	p := &rtp.Packet{Header: rtp.Header{SSRC: 1, SequenceNumber: 100, Timestamp: 5000}}
	r.rewrite(p, at)
	if p.SequenceNumber != 100 || p.Timestamp != 5000 {
		t.Fatalf("first: %+v", p.Header)
	}
	// A new ffmpeg (new SSRC, random numbers) a second later: one stream going on
	p = &rtp.Packet{Header: rtp.Header{SSRC: 2, SequenceNumber: 40000, Timestamp: 7}}
	r.rewrite(p, at.Add(time.Second))
	if p.SequenceNumber != 101 || p.Timestamp != 5000+90000+1 {
		t.Fatalf("after a restart: %+v", p.Header)
	}
	p = &rtp.Packet{Header: rtp.Header{SSRC: 2, SequenceNumber: 40001, Timestamp: 3007}}
	r.rewrite(p, at.Add(time.Second))
	if p.SequenceNumber != 102 || p.Timestamp != 5000+90000+1+3000 {
		t.Fatalf("next: %+v", p.Header)
	}
}

func TestTVArgs(t *testing.T) {
	a := strings.Join(tvArgs(tvOpts{mode: "x264", url: "http://x/y.m3u8", vport: 1, aport: 2, kbps: 99999, height: 4000}), " ")
	for _, want := range []string{"-re", "libx264", "-bf 0", "veryfast", "zerolatency", "-fpsmax 30", "libopus", "rtp://127.0.0.1:1?pkt_size=1200", "rtp://127.0.0.1:2?pkt_size=1200",
		"-b:v 12000k", "-maxrate 12000k", "min(iw,1920)", "min(ih,1080)", "-user_agent", "-rw_timeout 15000000", "-reconnect 1", "-protocol_whitelist " + tvProtocols, "-format_whitelist " + tvFormats} {
		if !strings.Contains(a, want) {
			t.Errorf("x264 args lack %q: %s", want, a)
		}
	}
	a = strings.Join(tvArgs(tvOpts{mode: "x264", url: "http://x/y", vport: 1, aport: 2}), " ")
	for _, want := range []string{"-b:v 3000k", "min(iw,1280)", "min(ih,720)"} {
		if !strings.Contains(a, want) {
			t.Errorf("default args lack %q: %s", want, a)
		}
	}
	a = strings.Join(tvArgs(tvOpts{mode: "vaapi", url: "https://x/y", vaDev: "/dev/dri/renderD128", vaProf: "main", vport: 1, aport: 2, noAudio: true}), " ")
	for _, want := range []string{"-vaapi_device /dev/dri/renderD128", "hwupload,scale_vaapi", "h264_vaapi", "-bf 0", "-fpsmax 30"} {
		if !strings.Contains(a, want) {
			t.Errorf("vaapi args lack %q: %s", want, a)
		}
	}
	if strings.Contains(a, "libopus") {
		t.Errorf("vaapi args: %s", a)
	}
	for _, line := range []string{"[http @ 0x1] http://u:p@h/x: Server returned 403", "Opening u:secret@host.example/live for reading"} {
		if got := redactURLs(line); strings.Contains(got, "u:p") || strings.Contains(got, "secret") {
			t.Fatal(got)
		}
	}
}

func tvState(m map[string]any) map[string]any { v, _ := m["tv"].(map[string]any); return v }

func isTV(m map[string]any) bool { return m["t"] == "tv" }

func isTVErr(m map[string]any) bool { return m["t"] == "error" && m["op"] == "tv" }

func TestTVProtocol(t *testing.T) {
	const secret = "http://user:hunter2@iptv.example/get.php?type=m3u"
	adminKey := newKey(t)
	s, hs := newTestServer(t, func(c *config) { c.tv = true; c.owners = uidOfKey(adminKey); c.ffmpeg = "ffmpeg-not-here" })
	s.tv.fetch = func(u string) ([]tvChannel, bool, error) {
		if u != secret {
			return nil, false, fmt.Errorf("unknown list")
		}
		return []tvChannel{{Name: "Pro TV", Group: "RO", Logo: "http://l/1.png", URL: "http://user:hunter2@iptv.example/1.ts"}, {Name: "Digi 24", Group: "Știri", URL: "http://user:hunter2@iptv.example/2.ts"}}, false, nil
	}
	admin, _ := dialV(t, hs, adminKey, "Boss", "")
	user, _ := dialV(t, hs, newKey(t), "User", "")
	if st := tvState(user.wel); st["on"] != true || st["why"] != "ffmpeg" || st["set"] != false {
		t.Fatalf("welcome tv: %v", user.wel["tv"])
	}
	// Not the user's to set
	user.send(map[string]any{"t": "tv", "op": "playlist", "url": "http://mine.example/x.m3u"})
	if m := user.wait(isTVErr); m["why"] != "role" {
		t.Fatalf("user playlist: %v", m)
	}
	// A private address is refused before anything is fetched
	admin.send(map[string]any{"t": "tv", "op": "playlist", "url": "http://192.168.1.1/list.m3u"})
	if m := admin.wait(isTVErr); m["why"] != "fetch" || !strings.Contains(m["message"].(string), "SV_TV_LAN") {
		t.Fatalf("private playlist: %v", m)
	}
	admin.send(map[string]any{"t": "tv", "op": "playlist", "url": secret})
	if m := admin.wait(isT("done")); m["op"] != "tv" || m["n"] != 2.0 {
		t.Fatalf("admin's playlist: %v", m)
	}
	m := admin.wait(func(m map[string]any) bool { return isTV(m) && tvState(m)["set"] == true })
	if st := tvState(m); st["kind"] != "url" || st["host"] != "iptv.example" || st["n"] != 2.0 {
		t.Fatalf("admin's state: %v", st)
	}
	m = user.wait(func(m map[string]any) bool { return isTV(m) && tvState(m)["set"] == true })
	raw, _ := json.Marshal(m)
	if bytes.Contains(raw, []byte("hunter2")) || bytes.Contains(raw, []byte("iptv.example")) {
		t.Fatalf("the user got the address: %s", raw)
	}
	// Newcomers: the same
	n, _ := dialV(t, hs, newKey(t), "New", "")
	raw, _ = json.Marshal(n.wel)
	if bytes.Contains(raw, []byte("hunter2")) || tvState(n.wel)["set"] != true {
		t.Fatalf("newcomer's welcome: %s", n.wel["tv"])
	}
	// The channel list, without addresses
	res, err := http.Get(hs.URL + "/tv/channels?token=" + url.QueryEscape(user.tok))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || bytes.Contains(body, []byte("hunter2")) || bytes.Contains(body, []byte("http")) ||
		!bytes.Contains(body, []byte(`{"i":1,"n":"Digi 24","g":"Știri"}`)) || !bytes.Contains(body, []byte(`"l":true`)) {
		t.Fatalf("channels: %d %s", res.StatusCode, body)
	}
	etag := res.Header.Get("ETag")
	req, _ := http.NewRequest("GET", hs.URL+"/tv/channels?token="+url.QueryEscape(user.tok), nil)
	req.Header.Set("If-None-Match", etag)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusNotModified {
		t.Fatalf("etag: %v %v", res, err)
	}
	if res, _ := http.Get(hs.URL + "/tv/channels?token=nope"); res.StatusCode != 401 {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
	// Playing: refused to the user until everyone may; then ffmpeg being missing is said clearly
	user.send(map[string]any{"t": "tv", "op": "play", "i": 0})
	if m := user.wait(isTVErr); m["why"] != "role" {
		t.Fatalf("user play: %v", m)
	}
	user.send(map[string]any{"t": "tv", "op": "open", "v": true})
	if m := user.wait(isTVErr); m["why"] != "role" {
		t.Fatalf("user open: %v", m)
	}
	admin.send(map[string]any{"t": "tv", "op": "open", "v": true})
	user.wait(func(m map[string]any) bool { return isTV(m) && tvState(m)["open"] == true })
	user.send(map[string]any{"t": "tv", "op": "play", "i": 0, "n": "Pro TV"})
	if m := user.wait(isTVErr); m["why"] != "ffmpeg" || !strings.Contains(m["message"].(string), "ffmpeg isn't installed") {
		t.Fatalf("user play when open: %v", m)
	}
	// An uploaded file: only with the TV permission; kept readable by the server alone
	file := "#EXTM3U\n#EXTINF:-1 group-title=\"Up\",Uploaded 1\nhttp://a:b@up.example/1\n#EXTINF:-1,Uploaded 2\nhttps://up.example/2\n"
	post := func(tok string) (int, map[string]any) {
		res, err := http.Post(hs.URL+"/tv/playlist?token="+url.QueryEscape(tok), "audio/x-mpegurl", strings.NewReader(file))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	if code, _ := post(user.tok); code != http.StatusForbidden {
		t.Fatalf("user upload: %d", code)
	}
	if code, out := post(admin.tok); code != 200 || out["n"] != 2.0 {
		t.Fatalf("admin upload: %d %v", code, out)
	}
	if st := tvState(admin.wait(func(m map[string]any) bool { return isTV(m) && tvState(m)["kind"] == "file" })); st["n"] != 2.0 || st["host"] != nil {
		t.Fatalf("after upload: %v", st)
	}
	if fi, err := os.Stat(s.tv.playlistFile()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("playlist file: %v %v", fi, err)
	}
	// The server's TV room is the server's: nobody publishes in its place
	pub, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+"/streams?token="+url.QueryEscape(admin.tok), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	_ = pub.WriteJSON(message{Type: "publish", Room: tvRoom, SDP: "v=0"})
	var em message
	_ = pub.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := pub.ReadJSON(&em); err != nil || em.Type != "error" {
		t.Fatalf("publishing as the TV: %+v %v", em, err)
	}
	// Clear: gone for everyone
	admin.send(map[string]any{"t": "tv", "op": "clear"})
	user.wait(func(m map[string]any) bool { return isTV(m) && tvState(m)["set"] == false })
	if _, err := os.Stat(s.tv.playlistFile()); !os.IsNotExist(err) {
		t.Fatal("the file stayed")
	}
	// Bots never touch the TV
	if why := s.hub.botAllowed(&session{api: true}, "tv"); why == "" {
		t.Fatal("a bot may use the TV")
	}
	// Nothing secret ever went into the log
	logs, _ := json.Marshal(s.logs.last())
	if bytes.Contains(logs, []byte("hunter2")) || bytes.Contains(logs, []byte("a:b@")) {
		t.Fatalf("the log holds a secret: %s", logs)
	}
}

// The whole way: a playlist on an HTTP server, ffmpeg playing its channel (HTTPS, through the proxy's CONNECT)
// into the room, a WebRTC viewer getting H.264 it can decode and Opus; off when nobody watches; the next viewer
// starts it again.
func TestTVEndToEnd(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	// The test's sources are on 127.0.0.1: the SSRF rule lets loopback through for this test only
	tvLoopbackOK = true
	defer func() { tvLoopbackOK = false }()
	oldGap := tvPlayGap
	tvPlayGap = 0
	defer func() { tvPlayGap = oldGap }()
	// A 30 s channel as HLS, like an IPTV provider's
	media := t.TempDir()
	gen := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "30", "-c:v", "libx264", "-preset", "ultrafast", "-g", "30", "-c:a", "aac", "-f", "hls", "-hls_time", "2", "-hls_playlist_type", "vod", filepath.Join(media, "index.m3u8"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("making the test channel: %v %s", err, out)
	}
	var hits atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// The subscription's password in the path, as IPTV providers do (/live/user/pass/…)
		if !strings.HasPrefix(r.URL.Path, "/s3cret/") {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		http.StripPrefix("/s3cret", http.FileServer(http.Dir(media))).ServeHTTP(w, r)
	}))
	defer provider.Close()
	lists := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "#EXTM3U\n#EXTINF:-1 tvg-logo=\"%s/logo.png\" group-title=\"Test\",Test Channel\n%s/s3cret/index.m3u8\n#EXTINF:-1,Broken\nhttp://127.0.0.1:9/none.m3u8\n", "http://"+r.Host, provider.URL)
	}))
	defer lists.Close()

	adminKey := newKey(t)
	s, hs := newTestServer(t, func(c *config) {
		c.tv = true
		c.owners = uidOfKey(adminKey)
		c.ffmpeg = ff
		c.tvVideo = "x264"
		c.tvKbps = 1500
		c.tvHeight = 360
	})
	tm := s.tv
	tm.idle, tm.idleTick = 6*time.Second, 200*time.Millisecond
	t.Cleanup(tm.stopAll)
	admin, _ := dialV(t, hs, adminKey, "Boss", "")
	vc, _ := dialV(t, hs, newKey(t), "Vio", "")

	admin.send(map[string]any{"t": "tv", "op": "playlist", "url": lists.URL + "/list.m3u"})
	admin.wait(isT("done"))
	admin.send(map[string]any{"t": "tv", "op": "play", "i": 0})
	started := time.Now()
	now := tvState(admin.wait(func(m map[string]any) bool {
		if isTVErr(m) {
			t.Fatalf("play: %v", m)
		}
		n, _ := tvState(m)["now"].(map[string]any)
		return isTV(m) && n != nil && n["live"] == true
	}))["now"].(map[string]any)
	t.Logf("live after %v: %v", time.Since(started), now)
	if now["n"] != "Test Channel" || now["room"] != tvRoom || now["byName"] != "Boss" || now["g"] != "Test" {
		t.Fatalf("now: %v", now)
	}
	res, _ := http.Get(hs.URL + "/streams/status?room=" + url.QueryEscape(tvRoom) + "&token=" + url.QueryEscape(vc.tok))
	var st message
	_ = json.NewDecoder(res.Body).Decode(&st)
	res.Body.Close()
	if st.Live == nil || !*st.Live {
		t.Fatalf("status: %+v", st)
	}

	watch := func() (*client, chan []byte, *atomic.Int32) {
		viewer := dial(t, "ws"+strings.TrimPrefix(hs.URL, "http")+"/streams?token="+url.QueryEscape(vc.tok))
		var audioPkts atomic.Int32
		keyframe := make(chan []byte, 1)
		viewer.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			if tr.Kind() == webrtc.RTPCodecTypeAudio {
				for {
					if _, _, err := tr.ReadRTP(); err != nil {
						return
					}
					audioPkts.Add(1)
				}
			}
			if !strings.EqualFold(tr.Codec().MimeType, webrtc.MimeTypeH264) {
				t.Errorf("video codec %s", tr.Codec().MimeType)
				return
			}
			sb := samplebuilder.New(512, &codecs.H264Packet{}, 90000)
			var stream bytes.Buffer
			frames := 0
			for {
				pkt, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				sb.Push(pkt)
				for smp := sb.Pop(); smp != nil; smp = sb.Pop() {
					if stream.Len() == 0 && !hasNAL(smp.Data, 7) {
						continue
					}
					stream.Write(smp.Data)
					frames++
					if frames == 10 {
						select {
						case keyframe <- stream.Bytes():
						default:
						}
					}
				}
			}
		})
		go viewer.readKeepingCandidates()
		viewer.send(message{Type: "subscribe", Room: tvRoom})
		return viewer, keyframe, &audioPkts
	}
	viewer, keyframe, audioPkts := watch()
	var h264 []byte
	select {
	case h264 = <-keyframe:
	case <-time.After(20 * time.Second):
		t.Fatal("no key frame for the viewer")
	}
	f := filepath.Join(t.TempDir(), "got.h264")
	_ = os.WriteFile(f, h264, 0o644)
	out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "h264", "-i", f, "-f", "null", "-").CombinedOutput()
	if err != nil || len(bytes.TrimSpace(out)) > 0 {
		t.Fatalf("the viewer's H.264 doesn't decode: %v %s", err, out)
	}
	probe, _ := exec.Command(ff, "-hide_banner", "-f", "h264", "-i", f).CombinedOutput()
	if !bytes.Contains(probe, []byte("640x360")) {
		t.Errorf("picture size: %s", probe)
	}
	deadline := time.Now().Add(5 * time.Second)
	for audioPkts.Load() < 20 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := audioPkts.Load(); n < 20 {
		t.Fatalf("audio packets: %d", n)
	}
	// The logo, through the server
	res, err = http.Get(hs.URL + "/tv/logo?i=0&token=" + url.QueryEscape(vc.tok))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound { // the list server answers text, not a picture: refused
		t.Fatalf("logo: %d", res.StatusCode)
	}

	// Nobody watching: off by itself, ffmpeg gone, the channel still chosen
	tm.mu.Lock()
	player := tm.player
	tm.mu.Unlock()
	_ = viewer.pc.Close()
	_ = viewer.ws.Close()
	admin.wait(func(m map[string]any) bool {
		n, _ := tvState(m)["now"].(map[string]any)
		return isTV(m) && n != nil && n["playing"] == false
	})
	done := make(chan struct{})
	go func() { player.wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ffmpeg is still running")
	}
	s.mu.Lock()
	left := s.rooms[tvRoom]
	s.mu.Unlock()
	if left != nil {
		t.Fatal("the TV's room stayed")
	}
	// The next viewer starts it again
	viewer, keyframe, _ = watch()
	select {
	case <-keyframe:
	case <-time.After(25 * time.Second):
		t.Fatal("no picture after coming back")
	}
	_ = viewer.pc.Close()
	_ = viewer.ws.Close()

	// A channel that doesn't work says why, without its address
	tvFirstPacket = 3 * time.Second
	defer func() { tvFirstPacket = 40 * time.Second }()
	admin.send(map[string]any{"t": "tv", "op": "play", "i": 1, "n": "Broken"})
	m := admin.wait(func(m map[string]any) bool {
		n, _ := tvState(m)["now"].(map[string]any)
		return isTV(m) && n != nil && n["n"] == "Broken" && n["err"] != ""
	})
	t.Logf("broken channel: %v", tvState(m)["now"])
	// The same channel when the rule doesn't allow loopback (as in production): the proxy refuses, and says why
	tvLoopbackOK = false
	admin.send(map[string]any{"t": "tv", "op": "play", "i": 0})
	m = admin.wait(func(m map[string]any) bool {
		n, _ := tvState(m)["now"].(map[string]any)
		return isTV(m) && n != nil && n["i"] == 0.0 && strings.Contains(fmt.Sprint(n["err"]), "SV_TV_LAN")
	})
	t.Logf("blocked channel: %v", tvState(m)["now"])
	admin.send(map[string]any{"t": "tv", "op": "stop"})
	admin.wait(func(m map[string]any) bool { return isTV(m) && tvState(m)["now"] == nil })
	logs, _ := json.Marshal(s.logs.last())
	if bytes.Contains(logs, []byte("s3cret")) {
		t.Fatalf("the log holds the channel's password: %s", logs)
	}
	if hits.Load() == 0 {
		t.Fatal("the provider was never asked")
	}
}

// hasNAL says whether Annex B data holds a NAL unit of type typ.
func hasNAL(b []byte, typ byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 && b[i+3]&0x1f == typ {
			return true
		}
	}
	return false
}

// readKeepingCandidates is read for a viewer, keeping the server's candidates that come before its offer (as the
// app does).
func (c *client) readKeepingCandidates() {
	var early []webrtc.ICECandidateInit
	for {
		var m message
		if err := c.ws.ReadJSON(&m); err != nil {
			return
		}
		switch m.Type {
		case "offer":
			_ = c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP})
			for _, ci := range early {
				_ = c.pc.AddICECandidate(ci)
			}
			early = nil
			ans, _ := c.pc.CreateAnswer(nil)
			_ = c.pc.SetLocalDescription(ans)
			c.send(message{Type: "answer", SDP: ans.SDP})
		case "candidate":
			ci := webrtc.ICECandidateInit{Candidate: m.Candidate, SDPMid: m.Mid, SDPMLineIndex: m.MLine}
			if c.pc.RemoteDescription() == nil {
				early = append(early, ci)
			} else {
				_ = c.pc.AddICECandidate(ci)
			}
		case "error":
			c.t.Log("server error:", m.Message)
		}
	}
}
