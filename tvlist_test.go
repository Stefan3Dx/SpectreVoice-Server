package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseM3U(t *testing.T) {
	list := "\ufeff#EXTM3U x-tvg-url=\"http://epg\"\r\n" +
		"# a comment\r\n" +
		"#EXTINF:-1 tvg-id=\"protv.ro\" tvg-name=\"PRO TV HD\" tvg-logo=\"http://logo/pro.png\" group-title=\"Romania, HD\",Pro TV\r\n" +
		"#EXTVLCOPT:http-user-agent=Mozilla/5.0\r\n" +
		"http://user:pass@iptv.example:8080/live/1.m3u8\r\n" +
		"\r\n" +
		"#EXTINF:0 tvg-name=\"Only tvg name\" tvg-logo=\"file:///etc/x.png\",\r\n" +
		"#EXTGRP:News\r\n" +
		"https://cdn.example/news.m3u8\r\n" +
		"#EXTINF:-1,Local file\r\n" +
		"file:///etc/passwd\r\n" +
		"#EXTINF:-1,Raw UDP\r\n" +
		"udp://239.0.0.1:1234\r\n" +
		"#EXTINF:-1 group-title=\"Sport\",Sport 1, live\n" +
		"rtmp://rtmp.example/app/sport\n" +
		"https://bare.example/x.ts\n"
	chs, cut, err := parseM3U(strings.NewReader(list))
	if err != nil || cut {
		t.Fatal(err, cut)
	}
	want := []tvChannel{
		{Name: "Pro TV", Group: "Romania, HD", Logo: "http://logo/pro.png", ID: "protv.ro", URL: "http://user:pass@iptv.example:8080/live/1.m3u8", Agent: "Mozilla/5.0"},
		{Name: "Only tvg name", Group: "News", URL: "https://cdn.example/news.m3u8"},
		{Name: "bare.example", URL: "https://bare.example/x.ts"},
	}
	if len(chs) != len(want) {
		t.Fatalf("%d channels: %+v", len(chs), chs)
	}
	for i, w := range want {
		if chs[i] != w {
			t.Errorf("channel %d:\n got %+v\nwant %+v", i, chs[i], w)
		}
	}
	if _, _, err := parseM3U(strings.NewReader("#EXTM3U\n# nothing\n")); err == nil {
		t.Fatal("an empty list was taken")
	}
	// Bounded: more channels than allowed are cut
	old := tvMaxChannels
	tvMaxChannels = 5
	defer func() { tvMaxChannels = old }()
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "#EXTINF:-1,C%d\nhttp://h.example/%d\n", i, i)
	}
	chs, cut, err = parseM3U(strings.NewReader(b.String()))
	if err != nil || !cut || len(chs) != 5 {
		t.Fatalf("cut: %d %v %v", len(chs), cut, err)
	}
}

func TestTVIPAllowed(t *testing.T) {
	for _, c := range []struct {
		ip       string
		lan, out bool
	}{
		{"8.8.8.8", false, true}, {"2a00:1450::1", false, true},
		{"127.0.0.1", true, false}, {"::1", true, false}, {"0.0.0.0", true, false},
		{"169.254.169.254", true, false}, {"fe80::1", true, false}, {"224.0.0.1", true, false},
		{"10.1.2.3", false, false}, {"10.1.2.3", true, true}, {"192.168.1.5", false, false}, {"192.168.1.5", true, true},
		{"172.20.0.1", false, false}, {"100.64.1.1", false, false}, {"fd00::1", false, false}, {"fd00::1", true, true},
		{"::ffff:10.0.0.1", false, false}, {"::ffff:127.0.0.1", true, false}, {"64:ff9b::a00:1", true, false},
	} {
		if got := tvIPAllowed(net.ParseIP(c.ip), c.lan); got != c.out {
			t.Errorf("%s lan=%v: %v, want %v", c.ip, c.lan, got, c.out)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "ftp://x/y", "http://", "http://127.0.0.1/list.m3u", "http://[::1]:7880/", "http://10.0.0.1/x", "javascript:x", "http://a b/"} {
		if _, err := tvCheckURL(bad, false); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
	if _, err := tvCheckURL("https://iptv.example:8080/get.php?u=a&p=b", false); err != nil {
		t.Error(err)
	}
	if _, err := tvCheckURL("http://10.0.0.1/x", true); err != nil {
		t.Error("LAN with SV_TV_LAN:", err)
	}
}

// The proxy ffmpeg goes through: password, public addresses only (loopback refused unless tests allow it).
func TestTVProxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "hello") }))
	defer target.Close()
	p, err := startTVProxy(false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	pu, _ := url.Parse(p.env)
	via := func(withAuth bool) *http.Client {
		u := *pu
		if !withAuth {
			u.User = nil
		}
		return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&u)}, Timeout: 5 * time.Second}
	}
	get := func(c *http.Client, u string) (int, string) {
		res, err := c.Get(u)
		if err != nil {
			return 0, err.Error()
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, _ := get(via(false), target.URL); code != http.StatusProxyAuthRequired {
		t.Fatalf("no password: %d", code)
	}
	tvLoopbackOK = false
	if code, _ := get(via(true), target.URL); code != http.StatusForbidden {
		t.Fatalf("loopback through the proxy: %d", code)
	}
	if code, _ := get(via(true), "http://10.255.255.1:9/"); code != http.StatusForbidden {
		t.Fatalf("LAN through the proxy: %d", code)
	}
	// CONNECT (https) into the server's own network: refused too
	c, err := net.Dial("tcp", pu.Host)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", strings.TrimPrefix(target.URL, "http://"), strings.TrimPrefix(target.URL, "http://"), p.auth)
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if !strings.Contains(line, "403") {
		t.Fatalf("CONNECT to loopback: %q", line)
	}
	tvLoopbackOK = true
	defer func() { tvLoopbackOK = false }()
	if code, body := get(via(true), target.URL); code != 200 || body != "hello" {
		t.Fatalf("allowed: %d %q", code, body)
	}
	// HTTPS through CONNECT, the way ffmpeg reaches https sources (the end-to-end test may only get plain HTTP)
	tlsTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "secure") }))
	defer tlsTarget.Close()
	sc := via(true)
	sc.Transport.(*http.Transport).TLSClientConfig = tlsTarget.Client().Transport.(*http.Transport).TLSClientConfig
	if code, body := get(sc, tlsTarget.URL); code != 200 || body != "secure" {
		t.Fatalf("https via CONNECT: %d %q", code, body)
	}
}

// A provider with a self-signed certificate: its playlist is refused by default, with words saying what to do,
// and read when the owner allows an untrusted certificate for it (only its own host's certificate unchecked).
func TestFetchM3UUntrustedCertificate(t *testing.T) {
	tvLoopbackOK = true
	defer func() { tvLoopbackOK = false }()
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:-1,Unu\nhttps://"+r.Host+"/live/u/p/1.m3u8\n")
	}))
	defer provider.Close()
	_, _, err := fetchM3U(provider.URL+"/get.php?username=u&password=p", false, false)
	if !errors.Is(err, errTVUntrusted) || !strings.Contains(err.Error(), "allow untrusted certificate") || strings.Contains(err.Error(), "password=p") {
		t.Fatalf("self-signed, not allowed: %v", err)
	}
	chs, _, err := fetchM3U(provider.URL+"/get.php?username=u&password=p", false, true)
	if err != nil || len(chs) != 1 || chs[0].Name != "Unu" {
		t.Fatalf("self-signed, allowed: %v %+v", err, chs)
	}
	// Allowed, the SSRF rule still holds
	tvLoopbackOK = false
	if _, _, err := fetchM3U(provider.URL+"/x.m3u", false, true); !errors.Is(err, errTVBlocked) {
		t.Fatalf("allowed but on loopback: %v", err)
	}
}

func TestTVMarkPlaylist(t *testing.T) {
	ok := func(h string) bool { return h == "iptv.example" || h == "10.0.0.5" }
	for in, want := range map[string]string{
		"https://iptv.example/live/u/p/1.m3u8":     "http://iptv.example" + tvTLSMark + ":443/live/u/p/1.m3u8",
		"https://IPTV.example:8443/a.ts?t=1&x=%2F": "http://iptv.example" + tvTLSMark + ":8443/a.ts?t=1&x=%2F",
		"https://u:p@iptv.example/x":               "http://u:p@iptv.example" + tvTLSMark + ":443/x",
		"https://10.0.0.5/x":                       "http://10.0.0.5" + tvTLSMark + ":443/x",
		"https://other.example/x":                  "https://other.example/x",
		"http://iptv.example/x":                    "http://iptv.example/x",
		"https://[2001:db8::1]/x":                  "https://[2001:db8::1]/x",
		"/relative/seg.ts":                         "/relative/seg.ts",
	} {
		if got := tvMarkURL(in, ok); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if got := tvMarkURL("https://iptv.example/x", nil); got != "https://iptv.example/x" {
		t.Errorf("nothing allowed: %s", got)
	}
	all := func(string) bool { return true }
	list := "#EXTM3U\r\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://keys.example/k\"\r\n#EXTINF:2,\r\nhttps://cdn.example/s1.ts\r\nseg2.ts\r\n#EXT-X-MAP:URI=\"init.mp4\"\r\n"
	got := string(tvMarkPlaylist([]byte(list), func(u string) string { return tvMarkURL(u, all) }))
	want := "#EXTM3U\r\n#EXT-X-KEY:METHOD=AES-128,URI=\"http://keys.example" + tvTLSMark + ":443/k\"\r\n#EXTINF:2,\r\nhttp://cdn.example" + tvTLSMark + ":443/s1.ts\r\nseg2.ts\r\n#EXT-X-MAP:URI=\"init.mp4\"\r\n"
	if got != want {
		t.Fatalf("playlist:\n%q\nwant\n%q", got, want)
	}
}

// The proxy's marked way: plain http from ffmpeg, https upstream, the certificate unchecked only for a host the
// owner allowed; the HLS playlists it passes on have their https addresses marked; the SSRF rule as everywhere.
func TestTVProxyMarked(t *testing.T) {
	tvLoopbackOK = true
	defer func() { tvLoopbackOK = false }()
	var target *httptest.Server
	target = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:2,\n"+target.URL+"/seg.ts\nseg2.ts\n")
		case "/moved":
			http.Redirect(w, r, target.URL+"/seg.ts", http.StatusFound)
		default:
			_, _ = io.WriteString(w, "segment")
		}
	}))
	defer target.Close()
	var allow atomic.Bool
	p, err := startTVProxyTrusting(false, func(h string) bool { return allow.Load() && h == "127.0.0.1" })
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	pu, _ := url.Parse(p.env)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	marked := tvMarkURL(target.URL, func(string) bool { return true })
	get := func(path string) (int, string, http.Header) {
		res, err := c.Get(marked + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b), res.Header
	}
	// Not allowed: the certificate is checked, and doesn't pass
	if code, _, _ := get("/seg.ts"); code != http.StatusBadGateway || p.untrustedAt.Load() == 0 {
		t.Fatalf("not allowed: %d %d", code, p.untrustedAt.Load())
	}
	allow.Store(true)
	if code, body, _ := get("/seg.ts"); code != 200 || body != "segment" {
		t.Fatalf("allowed: %d %q", code, body)
	}
	code, body, h := get("/index.m3u8")
	if code != 200 || body != "#EXTM3U\n#EXTINF:2,\n"+marked+"/seg.ts\nseg2.ts\n" || h.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("playlist: %d %q %v", code, body, h)
	}
	if code, _, h := get("/moved"); code != http.StatusFound || h.Get("Location") != marked+"/seg.ts" {
		t.Fatalf("redirect: %d %v", code, h.Get("Location"))
	}
	// Allowed, but loopback isn't (as in production): refused before any TLS
	tvLoopbackOK = false
	p.tr.CloseIdleConnections() // (a kept connection was made while loopback was allowed)
	if code, _, _ := get("/seg.ts"); code != http.StatusForbidden {
		t.Fatalf("loopback on the marked way: %d", code)
	}
}
