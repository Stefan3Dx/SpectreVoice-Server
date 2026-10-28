package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
}
