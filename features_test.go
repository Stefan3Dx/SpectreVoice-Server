package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A licence service for tests: its own vendor key; it activates "SV-GOOD-KEY" for the server that asks.
func fakeLicenseService(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	vk := newKey(t)
	pub, _ := x509.MarshalPKIXPublicKey(&vk.PublicKey)
	issue := func(srv string) string {
		c, _ := json.Marshal(licenseClaims{V: 1, Lic: "L1", Tier: "pro", Srv: srv, Iat: time.Now().UnixMilli()})
		body := base64.RawURLEncoding.EncodeToString(c)
		return body + "." + base64.RawURLEncoding.EncodeToString(signRaw(vk, []byte(body)))
	}
	ls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/v1/activate" || req["key"] != "SV-GOOD-KEY" {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"error":"unknown key"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": issue(req["server"])})
	}))
	t.Cleanup(ls.Close)
	return ls, base64.StdEncoding.EncodeToString(pub)
}

func TestLicenseActivation(t *testing.T) {
	ls, vendor := fakeLicenseService(t)
	s, hs := newTestServer(t, func(c *config) { c.licenseURL = ls.URL; c.vendorKey = vendor; c.owners = "" })
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	if tier := ana.wel["tier"].(map[string]any); tier["limits"].(map[string]any)["tier"] != "free" || tier["token"] != nil {
		t.Fatal(tier)
	}
	// Only the server's owners
	ana.send(map[string]any{"t": "license-activate", "key": "SV-GOOD-KEY"})
	if e := ana.wait(isT("error")); e["why"] != "role" {
		t.Fatal(e)
	}
	_ = s.store.setRole(ana.uid, roleOwner)
	ana.send(map[string]any{"t": "license-activate", "key": "SV-BAD-KEY"})
	if e := ana.wait(isT("error")); e["why"] != "license" {
		t.Fatal(e)
	}
	ana.send(map[string]any{"t": "license-activate", "key": "SV-GOOD-KEY"})
	tier := ana.wait(isT("tier"))
	if tier["limits"].(map[string]any)["tier"] != "pro1" || tier["limits"].(map[string]any)["maxUsers"].(float64) != 100 || tier["token"] == nil {
		t.Fatal(tier)
	}
	// The app's own check: the server signs its nonce with the key the token names
	nonce := make([]byte, 32)
	nonce[0] = 7
	bob := dialNonce(t, hs, base64.StdEncoding.EncodeToString(nonce))
	bt := bob["tier"].(map[string]any)
	der, _ := base64.StdEncoding.DecodeString(bt["key"].(string))
	k, _ := x509.ParsePKIXPublicKey(der)
	sig, _ := base64.StdEncoding.DecodeString(bt["sig"].(string))
	if !verifyRaw(k.(*ecdsa.PublicKey), append([]byte(serverSigPrefix), nonce...), sig) {
		t.Fatal("server signature")
	}
	c, err := s.lic.parse(bt["token"].(string))
	if err != nil || c.Srv != fingerprint(der) {
		t.Fatal(c, err)
	}
	// Pro limits apply: more communities per person
	if s.limits().OwnCommunities != proLimits.OwnCommunities {
		t.Fatal(s.limits())
	}
	// A new server key (another server): the token is for someone else, back to Free until activated again
	s2, _ := newTestServer(t, func(c *config) { c.licenseURL = ls.URL; c.vendorKey = vendor })
	if err := s2.lic.apply(bt["token"].(string), false); err != nil || s2.lic.status != "rebind" || s2.limits().Tier != "free" {
		t.Fatal(s2.lic.status, err)
	}
	// Released: Free again
	ana.send(map[string]any{"t": "license-release"})
	ana.wait(func(m map[string]any) bool { return m["t"] == "license" && m["status"] == "free" })
}

func dialNonce(t *testing.T, hs *httptest.Server, nonce string) map[string]any {
	t.Helper()
	key := newKey(t)
	c, _ := dialVWith(t, hs, key, "Bob", map[string]any{"nonce": nonce})
	return c.wel
}

func TestChannelFeatures(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	_ = s.store.setRole(ana.uid, roleOwner)
	s.hub.mu.Lock()
	s.hub.forget(1)
	s.hub.mu.Unlock()
	afk := chanID(t, ana.wel, "AFK")
	// A channel with an icon, a quality level above Free's top (clamped), moderated
	ana.send(map[string]any{"t": "ch-save", "name": "Muzică", "icon": "🎸", "quality": 6, "moderated": true})
	c := ana.wait(func(m map[string]any) bool {
		return m["t"] == "channel" && m["c"].(map[string]any)["name"] == "Muzică"
	})["c"].(map[string]any)
	if c["icon"] != "🎸" || c["quality"].(float64) != 4 || c["moderated"] != true {
		t.Fatal(c)
	}
	// A spacer can't be joined
	ana.send(map[string]any{"t": "ch-save", "name": "───", "kind": "spacer"})
	sp := ana.wait(func(m map[string]any) bool { return m["t"] == "channel" && m["c"].(map[string]any)["kind"] == "spacer" })["c"].(map[string]any)
	bob.send(map[string]any{"t": "join", "ch": sp["id"]})
	if e := bob.wait(isT("error")); e["op"] != "join" {
		t.Fatal(e)
	}
	// A text channel: everyone reads and writes it, nobody joins it, its history stays
	ana.send(map[string]any{"t": "ch-save", "name": "anunturi", "kind": "text", "password": "x"})
	tc := ana.wait(func(m map[string]any) bool { return m["t"] == "channel" && m["c"].(map[string]any)["kind"] == "text" })["c"].(map[string]any)
	if tc["locked"] == true {
		t.Fatal(tc)
	}
	bob.send(map[string]any{"t": "join", "ch": tc["id"]})
	if e := bob.wait(isT("error")); e["op"] != "join" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "say", "to": "c:" + itoa(int(tc["id"].(float64))), "text": "în text"})
	if msg := ana.wait(isT("msg"))["m"].(map[string]any); msg["text"] != "în text" {
		t.Fatal(msg)
	}
	// Kicked out of a channel: back to the default one
	bob.send(map[string]any{"t": "join", "ch": afk})
	bob.wait(func(m map[string]any) bool {
		return m["t"] == "member" && m["m"].(map[string]any)["ch"].(float64) == float64(afk)
	})
	ana.send(map[string]any{"t": "ch-kick", "sid": bob.sid})
	if mv := bob.wait(isT("moved")); mv["kick"] != true {
		t.Fatal(mv)
	}
	// Bob can't kick Ana
	bob.send(map[string]any{"t": "ch-kick", "sid": ana.sid})
	if e := bob.wait(isT("error")); e["op"] != "ch-kick" {
		t.Fatal(e)
	}
	// Voice mute: shown to everyone, kept
	ana.send(map[string]any{"t": "voice-mute", "uid": bob.uid})
	ana.wait(func(m map[string]any) bool { return m["t"] == "member" && m["m"].(map[string]any)["vmuted"] == true })
	if !s.mods.voiceMuted(bob.uid, 1) {
		t.Fatal("not kept")
	}
	s.hub.mu.Lock()
	heard := s.hub.maySpeak(s.hub.sessions[bob.sid])
	s.hub.mu.Unlock()
	if heard {
		t.Fatal("muted voice heard")
	}
	ana.send(map[string]any{"t": "voice-unmute", "uid": bob.uid})
	ana.wait(func(m map[string]any) bool {
		return m["t"] == "member" && m["m"].(map[string]any)["uid"] == bob.uid && m["m"].(map[string]any)["vmuted"] == nil
	})
	// Away message and connection quality, for everyone to see
	bob.send(map[string]any{"t": "state", "away": true, "awayMsg": "la masă", "rtt": 42, "loss": 1})
	m := ana.wait(func(m map[string]any) bool {
		return m["t"] == "member" && m["m"].(map[string]any)["awayMsg"] == "la masă"
	})["m"].(map[string]any)
	if m["rtt"].(float64) != 42 || m["since"] == nil {
		t.Fatal(m)
	}
	// The community's own chat
	bob.send(map[string]any{"t": "say", "to": "g:1", "text": "salut tuturor"})
	if msg := ana.wait(isT("msg"))["m"].(map[string]any); msg["to"] != "g:1" {
		t.Fatal(msg)
	}
	bob.send(map[string]any{"t": "say", "to": "g:2", "text": "nu"})
	if e := bob.wait(isT("error")); e["op"] != "say" {
		t.Fatal(e)
	}
}

func TestChannelFiles(t *testing.T) {
	_, hs := newTestServer(t, func(c *config) { c.fileMB = 10 })
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	lobby := chanID(t, ana.wel, "Lobby")
	ana.send(map[string]any{"t": "cf-mkdir", "ch": lobby, "folder": 0, "name": "Poze"})
	l := ana.wait(isT("cf"))
	folder := l["items"].([]any)[0].(map[string]any)
	if folder["dir"] != true || folder["name"] != "Poze" {
		t.Fatal(l)
	}
	fid := int64(folder["id"].(float64))
	up := func(tok, name string, folder int64) *http.Response {
		req, _ := http.NewRequest("POST", hs.URL+"/cf/upload?ch="+itoa(int(lobby))+"&folder="+itoa(int(folder))+"&name="+name, strings.NewReader("hello world"))
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := up(bob.tok, "a.txt", fid)
	var got struct{ ID int64 }
	_ = json.NewDecoder(res.Body).Decode(&got)
	res.Body.Close()
	if res.StatusCode != 200 || got.ID == 0 {
		t.Fatal(res.Status)
	}
	if res := up("bad", "b.txt", fid); res.StatusCode != 401 {
		t.Fatal(res.Status)
	}
	if res := up(bob.tok, "../../etc/x", 9999); res.StatusCode != 404 {
		t.Fatal(res.Status)
	}
	res, _ = http.Get(hs.URL + "/cf/" + itoa(int(got.ID)) + "?token=" + ana.tok)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "hello world" || !strings.Contains(res.Header.Get("Content-Disposition"), "a.txt") {
		t.Fatal(string(b), res.Header)
	}
	ana.send(map[string]any{"t": "cf-list", "ch": lobby, "folder": fid})
	l = ana.wait(isT("cf"))
	if len(l["items"].([]any)) != 1 || len(l["path"].([]any)) != 1 {
		t.Fatal(l)
	}
	// Ana (not the owner, no "files") can't delete Bob's file; Bob can
	ana.send(map[string]any{"t": "cf-del", "id": got.ID})
	if e := ana.wait(isT("error")); e["why"] != "role" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "cf-del", "id": got.ID})
	if l := bob.wait(isT("cf")); len(l["items"].([]any)) != 0 {
		t.Fatal(l)
	}
	ana.send(map[string]any{"t": "cf-del", "id": fid})
	if l := ana.wait(isT("cf")); len(l["items"].([]any)) != 0 {
		t.Fatal(l)
	}
	res, _ = http.Get(hs.URL + "/cf/" + itoa(int(got.ID)) + "?token=" + ana.tok)
	if res.StatusCode != 404 {
		t.Fatal("deleted file still there")
	}
}

func TestOggPackets(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "libopus", "-b:a", "96k", "-ac", "2", "-frame_duration", "20", "-f", "ogg", "-page_duration", "20000", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	r := newOggPackets(bufio.NewReader(bytes.NewReader(out)))
	n := 0
	for {
		p, err := r.next()
		if err != nil {
			break
		}
		if len(p) == 0 || p[0]&0xf8 == 0 && len(p) < 2 {
			t.Fatal("bad packet")
		}
		n++
	}
	if n < 48 || n > 53 {
		t.Fatalf("%d packets for one second", n)
	}
}

func TestMusicBot(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	s, hs := newTestServer(t, func(c *config) { c.music = true; c.ytdlp = "ffmpeg"; c.ffmpeg = "ffmpeg" })
	if !s.music.enabled() {
		t.Fatal("music off")
	}
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	va := newVoiceClient(t, ana)
	stop := make(chan struct{})
	defer close(stop)
	go va.pump(stop)
	time.Sleep(300 * time.Millisecond)
	// The "song": a generated tone
	s.music.resolveFn = func(q string) (song, error) { return song{Title: "Ton", URL: "lavfi"}, nil }
	ffmpegInput = func(u string) []string { return []string{"-f", "lavfi", "-i", "sine=frequency=440:duration=3"} }
	defer func() { ffmpegInput = defaultFFmpegInput }()
	ana.send(map[string]any{"t": "say", "to": "c:" + itoa(int(chanID(t, ana.wel, "Lobby"))), "text": "!play ceva"})
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		va.mu.Lock()
		n := 0
		for k, v := range va.got {
			if k != "s"+itoa(ana.sid) {
				n += v
			}
		}
		va.mu.Unlock()
		if n > 40 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	va.mu.Lock()
	defer va.mu.Unlock()
	total := 0
	for k, v := range va.got {
		if k != "s"+itoa(ana.sid) {
			total += v
		}
	}
	if total <= 40 {
		t.Fatalf("bot's voice: %v", va.got)
	}
}
