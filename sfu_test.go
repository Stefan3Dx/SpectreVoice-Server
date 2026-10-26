package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// A client end: a PeerConnection driven over the WebSocket signalling.
type client struct {
	t  *testing.T
	ws *websocket.Conn
	pc *webrtc.PeerConnection
	mu sync.Mutex
}

func (c *client) send(m message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ws.WriteJSON(m); err != nil {
		c.t.Log("send:", err)
	}
}

func dial(t *testing.T, url string) *client {
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, ws: ws, pc: pc}
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		ci := cand.ToJSON()
		c.send(message{Type: "candidate", Candidate: ci.Candidate, Mid: ci.SDPMid, MLine: ci.SDPMLineIndex})
	})
	return c
}

// read handles the server's messages until the connection ends.
func (c *client) read(ended chan<- bool) {
	for {
		var m message
		if err := c.ws.ReadJSON(&m); err != nil {
			return
		}
		switch m.Type {
		case "answer":
			_ = c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP})
		case "offer":
			_ = c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP})
			ans, _ := c.pc.CreateAnswer(nil)
			_ = c.pc.SetLocalDescription(ans)
			c.send(message{Type: "answer", SDP: ans.SDP})
		case "candidate":
			_ = c.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: m.Candidate, SDPMid: m.Mid, SDPMLineIndex: m.MLine})
		case "ended":
			if ended != nil {
				ended <- true
			}
		case "error":
			c.t.Log("server error:", m.Message)
		}
	}
}

func TestPublishAndWatch(t *testing.T) {
	s, ts := newTestServer(t, nil)
	alice := newUID(t)
	room := alice + ":screen"
	base := "ws" + strings.TrimPrefix(ts.URL, "http") + "/streams?token="
	wsURL := base + url.QueryEscape(s.makeToken(alice))
	viewerURL := base + url.QueryEscape(s.makeToken(newUID(t)))
	status := func(q string) (int, message) {
		res, err := http.Get(ts.URL + "/streams/status?room=" + url.QueryEscape(room) + q)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m message
		_ = json.NewDecoder(res.Body).Decode(&m)
		return res.StatusCode, m
	}

	// Without a token, or a wrong one: refused
	for _, bad := range []string{"", "nope"} {
		if _, res, err := websocket.DefaultDialer.Dial(base+bad, nil); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q got in: %v", bad, err)
		}
	}
	if code, _ := status(""); code != http.StatusUnauthorized {
		t.Fatalf("status without a token: %d", code)
	}
	if code, m := status("&token=" + url.QueryEscape(s.makeToken(alice))); code != 200 || m.Live == nil || *m.Live || *m.Viewers != 0 {
		t.Fatalf("status before: %d %+v", code, m)
	}

	// Someone on the server, told when the stream goes live
	watcher, _ := dialV(t, ts, newKey(t), "W", "")

	// The streamer: a VP8 track of made-up frames
	pub := dial(t, wsURL)
	track, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "share")
	if _, err := pub.pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	go pub.read(nil)
	offer, _ := pub.pc.CreateOffer(nil)
	_ = pub.pc.SetLocalDescription(offer)
	pub.send(message{Type: "publish", Room: room, SDP: offer.SDP, Title: "Joc"})
	lv := watcher.wait(func(m map[string]any) bool { return m["t"] == "live" })
	if ls, _ := lv["s"].(map[string]any); lv["on"] != true || ls["room"] != room || ls["uid"] != alice || ls["title"] != "Joc" {
		t.Fatalf("live: %v", lv)
	}
	stop := make(chan bool)
	go func() {
		frame := make([]byte, 800)
		frame[0] = 0x10 // a VP8 key frame's first byte
		for {
			select {
			case <-stop:
				return
			case <-time.After(33 * time.Millisecond):
				_ = track.WriteSample(media.Sample{Data: frame, Duration: 33 * time.Millisecond})
			}
		}
	}()
	defer close(stop)

	// Live?
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		rm := s.rooms[room]
		s.mu.Unlock()
		if rm != nil {
			rm.mu.Lock()
			n := len(rm.tracks)
			rm.mu.Unlock()
			if n > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream never went live")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A viewer gets the picture
	sub := dial(t, viewerURL)
	got := make(chan int, 1)
	sub.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		n := 0
		buf := make([]byte, 1500)
		for n < 20 {
			if _, _, err := tr.Read(buf); err != nil {
				return
			}
			n++
		}
		got <- n
	})
	ended := make(chan bool, 1)
	go sub.read(ended)
	sub.send(message{Type: "subscribe", Room: room})
	select {
	case n := <-got:
		t.Logf("viewer got %d packets", n)
	case <-time.After(15 * time.Second):
		t.Fatal("the viewer got no picture")
	}
	if code, m := status("&token=" + url.QueryEscape(s.makeToken(alice))); code != 200 || !*m.Live || *m.Viewers != 1 {
		t.Fatalf("status live: %d %+v", code, m)
	}

	// The streamer leaves: the viewer is told
	_ = pub.pc.Close()
	_ = pub.ws.Close()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the viewer wasn't told the stream ended")
	}
}

func TestUpdates(t *testing.T) {
	apk := []byte("fake package bytes")
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r/releases":
			// Newest first: a Windows release (not ours), a draft, then the Android ones
			fmt.Fprintf(w, `[{"tag_name":"windows-v0.2.0","body":"pc","assets":[{"name":"SpectreVoice-0.2.0.msi","url":"%[1]s/a/9"}]},
				{"tag_name":"android-v0.2.0","draft":true,"body":"draft","assets":[{"name":"x.apk","url":"%[1]s/a/9"}]},
				{"tag_name":"android-v0.1.9","body":"- Changelog: notes","assets":[{"name":"SpectreVoice-0.1.9.aab","url":"%[1]s/a/1"},{"name":"SpectreVoice-0.1.9.apk","url":"%[1]s/a/2"}]},
				{"tag_name":"android-v0.1.8","body":"old"}]`, gh.URL)
		case "/a/2":
			w.Write(apk)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	s, srv := newTestServer(t, nil)
	// No updates set up: none served
	tok := url.QueryEscape(s.makeToken(newUID(t)))
	if res, _ := http.Get(srv.URL + "/update/android.json?token=" + tok); res.StatusCode != 404 {
		t.Fatalf("updates off: %d", res.StatusCode)
	}
	dir := t.TempDir()
	s.updates = []*updater{{dir: dir, index: "android.json", asset: ".apk", prefix: "android-v", repo: "o/r", token: "tok", api: gh.URL, every: time.Hour, http: gh.Client()}}
	if err := s.updates[0].sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.updates[0].sync(); err != nil { // nothing new
		t.Fatal(err)
	}
	res, err := http.Get(srv.URL + "/update/android.json?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	var info updateInfo
	json.NewDecoder(res.Body).Decode(&info)
	res.Body.Close()
	if info.Version != "0.1.9" || info.File != "SpectreVoice-0.1.9.apk" || info.Size != int64(len(apk)) || len(info.SHA256) != 64 || info.Notes != "- notes" || len(info.History) != 2 {
		t.Fatalf("android.json: %+v", info)
	}
	res, _ = http.Get(srv.URL + "/update/" + info.File + "?token=" + tok)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != string(apk) || res.Header.Get("Content-Type") != "application/vnd.android.package-archive" {
		t.Fatalf("package: %q %s", b, res.Header.Get("Content-Type"))
	}
	for _, bad := range []string{"/update/android.json", "/update/android.json?token=nope"} {
		res, _ = http.Get(srv.URL + bad)
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("%s: %d", bad, res.StatusCode)
		}
	}
	// The Bearer header works too
	req, _ := http.NewRequest("GET", srv.URL+"/update/android.json", nil)
	req.Header.Set("Authorization", "Bearer "+s.makeToken("U"))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Fatalf("Bearer: %d", res.StatusCode)
	}
	res, _ = http.Get(srv.URL + "/update/..%2Fsfu_test.go?token=" + tok)
	res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatal("escaped the folder")
	}
}

func TestFiles(t *testing.T) {
	s, srv := newTestServer(t, func(c *config) { c.files = false })
	tok := url.QueryEscape(s.makeToken(newUID(t)))
	if res, _ := http.Post(srv.URL+"/files?token="+tok+"&name=a.txt", "", strings.NewReader("x")); res.StatusCode != 404 {
		t.Fatalf("files off: %d", res.StatusCode)
	}
	s.files = &fileStore{dir: t.TempDir(), maxSize: 1 << 20, keep: time.Hour}
	res, err := http.Post(srv.URL+"/files?token="+tok+"&name=../My%20cat.png", "application/octet-stream", strings.NewReader("png bytes"))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("upload: %v %v", err, res.Status)
	}
	var up struct {
		URL  string `json:"url"`
		Size int64  `json:"size"`
	}
	json.NewDecoder(res.Body).Decode(&up)
	res.Body.Close()
	if !strings.HasSuffix(up.URL, "/My cat.png") || up.Size != 9 {
		t.Fatalf("upload answer: %+v", up)
	}
	// Anyone with the link downloads, without a token
	res, _ = http.Get(srv.URL + strings.ReplaceAll(up.URL, " ", "%20"))
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "png bytes" || res.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("download: %q %s", b, res.Header.Get("Content-Type"))
	}
	for _, bad := range []string{"", "&token=nope"} {
		res, _ = http.Post(srv.URL+"/files?name=a.txt"+bad, "", strings.NewReader("x"))
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("upload without a token (%q): %d", bad, res.StatusCode)
		}
	}
	res, _ = http.Post(srv.URL+"/files?token="+tok+"&name=big.bin", "", strings.NewReader(strings.Repeat("x", 2<<20)))
	res.Body.Close()
	if res.StatusCode == 200 {
		t.Fatal("too big went through")
	}
	res, _ = http.Post(srv.URL+"/files?token="+tok+"&name=page.html", "", strings.NewReader("<script>"))
	json.NewDecoder(res.Body).Decode(&up)
	res.Body.Close()
	res, _ = http.Get(srv.URL + up.URL)
	res.Body.Close()
	if res.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("html served inline")
	}
}

func TestBus(t *testing.T) {
	s, srv := newTestServer(t, nil)
	t.Cleanup(s.bus.conns.Wait)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/bus?space=" + mainSpace + "&token="
	dial := func(uid string) *websocket.Conn {
		_ = s.store.join(1, uid)
		c, _, err := websocket.DefaultDialer.Dial(wsURL+url.QueryEscape(s.makeToken(uid)), nil)
		if err != nil {
			t.Fatal(err)
		}
		var hello map[string]any
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if err := c.ReadJSON(&hello); err != nil || hello["t"] != "state" || hello["version"] != version {
			t.Fatalf("hello: %v %v", hello, err)
		}
		return c
	}
	a := dial("A")
	b := dial("B")
	defer a.Close()
	defer b.Close()
	read := func(c *websocket.Conn) map[string]any {
		var m map[string]any
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if err := c.ReadJSON(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Passed on to the others, from whom
	a.WriteJSON(map[string]any{"t": "react", "emoji": "🔥", "from": "someone else"})
	if m := read(b); m["t"] != "react" || m["from"] != "A" || m["emoji"] != "🔥" || m["relay"] != true {
		t.Fatalf("relay: %v", m)
	}
	// Kept and sent to everyone
	a.WriteJSON(map[string]any{"t": "put", "kind": "events", "id": "e1", "v": map[string]any{"title": "CS2", "at": 123}})
	if m := read(a); m["t"] != "put" || m["id"] != "e1" {
		t.Fatalf("put echo: %v", m)
	}
	if m := read(b); m["t"] != "put" {
		t.Fatalf("put: %v", m)
	}
	// Unknown kind: refused, nothing kept
	a.WriteJSON(map[string]any{"t": "put", "kind": "nope", "id": "x", "v": 1})
	if m := read(a); m["t"] != "error" || m["why"] != "bad" {
		t.Fatalf("unknown kind: %v", m)
	}
	// A newcomer gets what's kept
	_ = s.store.join(1, "D")
	c2, _, _ := websocket.DefaultDialer.Dial(wsURL+url.QueryEscape(s.makeToken("D")), nil)
	defer c2.Close()
	var st struct {
		T     string                                `json:"t"`
		State map[string]map[string]json.RawMessage `json:"state"`
	}
	c2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := c2.ReadJSON(&st); err != nil || st.State["events"]["e1"] == nil || st.State["nope"] != nil {
		t.Fatalf("state: %+v %v", st, err)
	}
	// Backups
	tok := url.QueryEscape(s.makeToken("A"))
	res, err := http.Post(srv.URL+"/backup/0123456789abcdef0123456789abcdef?token="+tok, "", strings.NewReader("sealed"))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("backup put: %v", err)
	}
	res.Body.Close()
	res, _ = http.Get(srv.URL + "/backup/0123456789abcdef0123456789abcdef?token=" + tok)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "sealed" {
		t.Fatalf("backup get: %q", body)
	}
	res, _ = http.Get(srv.URL + "/backup/0123456789abcdef0123456789abcdef?token=wrong")
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("backup without a token")
	}
	res, _ = http.Get(srv.URL + "/backup/not-an-id?token=" + tok)
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("bad backup id: %d", res.StatusCode)
	}
}

func TestCrashReports(t *testing.T) {
	s, srv := newTestServer(t, nil)
	user := url.QueryEscape(s.makeToken(newUID(t)))
	boss := newUID(t)
	if err := s.store.setRole(boss, roleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := url.QueryEscape(s.makeToken(boss))
	// Two in the same second: both kept
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/crash?token="+user+"&name=Ana%20B&file=freeze-1.txt&v=0.1.50", strings.NewReader("stack"))
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("put: %v %v", err, res)
		}
		res.Body.Close()
	}
	// Reading them takes an admin's token
	res, _ := http.Get(srv.URL + "/crash?token=" + user)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("list by a member: %d", res.StatusCode)
	}
	res, _ = http.Get(srv.URL + "/crash?token=" + admin)
	list, _ := io.ReadAll(res.Body)
	res.Body.Close()
	names := strings.Fields(string(list))
	if len(names) != 2 || !strings.Contains(names[0], "Ana_B-freeze-1.txt") {
		t.Fatalf("list: %q", list)
	}
	name := names[0]
	res, _ = http.Get(srv.URL + "/crash/" + name + "?token=" + user)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("report by a member: %d", res.StatusCode)
	}
	res, _ = http.Get(srv.URL + "/crash/" + name + "?token=" + admin)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), "stack") || !strings.Contains(string(body), "Ana B") {
		t.Fatalf("body: %q", body)
	}
	// The owner named in the config reads them too
	owner := newUID(t)
	s2, srv2 := newTestServer(t, func(c *config) { c.owners = owner })
	res, _ = http.Get(srv2.URL + "/crash?token=" + url.QueryEscape(s2.makeToken(owner)))
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("owner: %d", res.StatusCode)
	}
	res, _ = http.Get(srv.URL + "/crash?token=wrong")
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("no token: %d", res.StatusCode)
	}
}

func TestBusAddIncr(t *testing.T) {
	cur := applyAdd(map[string]any{}, map[string]any{"d": -1500.0, "start": 1000.0, "min": 100.0, "set": map[string]any{"n": "Ana"}})
	if cur["v"] != 100.0 || cur["n"] != "Ana" {
		t.Fatalf("refill: %v", cur)
	}
	cur = applyAdd(cur, map[string]any{"d": 100.0, "once": "2026-10-25"})
	if cur["v"] != 200.0 {
		t.Fatalf("daily: %v", cur)
	}
	if applyAdd(cur, map[string]any{"d": 100.0, "once": "2026-10-25"}) != nil {
		t.Fatal("daily twice")
	}
	sc := applyIncr(map[string]any{}, map[string]any{"week": "W1", "weekly": []any{"w7"}, "inc": map[string]any{"w.CHESS": 1.0, "w7.CHESS": 1.0}, "max": map[string]any{"bs.CHESS": 2.0}, "set": map[string]any{"n": "Ana", "s.CHESS": 2.0}})
	sc = applyIncr(sc, map[string]any{"week": "W2", "weekly": []any{"w7"}, "inc": map[string]any{"w.CHESS": 1.0, "w7.CHESS": 1.0}})
	if sc["w"].(map[string]any)["CHESS"] != 2.0 || sc["w7"].(map[string]any)["CHESS"] != 1.0 || sc["wk"] != "W2" || sc["bs"].(map[string]any)["CHESS"] != 2.0 {
		t.Fatalf("incr: %v", sc)
	}
}

func TestBusAdminKinds(t *testing.T) {
	bt := newBusTest(t, nil, nil)
	u := bt.dial("x", "U")
	a := bt.dialAdmin("x", "A")
	u.write(map[string]any{"t": "put", "kind": "donors", "id": "U", "v": map[string]any{"n": "me", "d": []any{}}})
	if m := u.expect("error"); m["why"] != "admin" || m["kind"] != "donors" {
		t.Fatalf("refusal: %v", m)
	}
	a.write(map[string]any{"t": "put", "kind": "donors", "id": "X", "v": map[string]any{"n": "Ana"}})
	// u then sees only the admin's
	if m := u.expect("put"); m["id"] != "X" || m["kind"] != "donors" {
		t.Fatalf("only the admin's put: %v", m)
	}
	a.expect("put")
	// Admins aren't held to the rate limit, nor to others' records
	a.write(map[string]any{"t": "put", "kind": "profiles", "id": "U", "v": map[string]any{"bio": "cleaned"}})
	if m := a.expect("put"); m["id"] != "U" {
		t.Fatalf("admin put on another's profile: %v", m)
	}
}
