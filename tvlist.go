package main

// IPTV playlists (M3U / M3U8) for the server's TV (tv.go): the list the admin gives, by address or as a file, is
// read here. Only the server ever sees a channel's address or the playlist's: IPTV subscriptions put the user name
// and password in them.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type tvChannel struct {
	Name  string
	Group string
	Logo  string
	ID    string // tvg-id
	URL   string
	Agent string // #EXTVLCOPT:http-user-agent, for providers that want a player's name
	Ref   string // #EXTVLCOPT:http-referrer
}

// Vars so tests can make them small.
var (
	tvListMax     int64 = 10 << 20 // a playlist bigger than this isn't read
	tvMaxChannels       = 20000
)

const (
	tvListTimeout = 20 * time.Second
	tvUserAgent   = "VLC/3.0.20 LibVLC/3.0.20" // IPTV providers often turn away ffmpeg's own name
)

var m3uAttr = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"]*)"`)

// parseM3U reads a playlist: #EXTINF lines (with tvg-*, group-title) and the address after each. Only http(s)
// addresses are taken (never a file on the server, nor rtmp/rtsp/udp, which the proxy can't check). Lines it
// doesn't know (#EXTM3U, comments, #KODIPROP…) are skipped; an address with no #EXTINF before it gets its host as
// a name. CRLF and a byte order mark are fine. At most tvMaxChannels are kept (more: cut, said by [cut]).
func parseM3U(r io.Reader) (out []tvChannel, cut bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var cur *tvChannel
	group := "" // #EXTGRP, for lists that put the group on its own line
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXTINF:"):
			c := parseExtinf(line[len("#EXTINF:"):])
			cur = &c
			group = ""
		case strings.HasPrefix(line, "#EXTGRP:"):
			group = clip(strings.TrimSpace(line[len("#EXTGRP:"):]), 80)
		case strings.HasPrefix(line, "#EXTVLCOPT:"):
			if cur == nil {
				cur = &tvChannel{}
			}
			k, v, _ := strings.Cut(line[len("#EXTVLCOPT:"):], "=")
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "http-user-agent":
				cur.Agent = clip(strings.TrimSpace(v), 200)
			case "http-referrer", "http-referer":
				cur.Ref = clip(strings.TrimSpace(v), 500)
			}
		case strings.HasPrefix(line, "#"):
		default:
			u, perr := url.Parse(line)
			if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || len(line) > 4096 {
				cur = nil
				continue
			}
			c := tvChannel{}
			if cur != nil {
				c = *cur
			}
			c.URL = line
			if c.Group == "" {
				c.Group = group
			}
			if c.Name == "" {
				c.Name = u.Hostname()
			}
			if c.Logo != "" {
				if lu, lerr := url.Parse(c.Logo); lerr != nil || (lu.Scheme != "http" && lu.Scheme != "https") || lu.Hostname() == "" {
					c.Logo = ""
				}
			}
			out = append(out, c)
			cur = nil
			if len(out) >= tvMaxChannels {
				return out, true, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return out, false, errors.New("the playlist isn't readable (a line too long?)")
	}
	if len(out) == 0 {
		return nil, false, errors.New("no channels in the playlist (only http and https addresses are taken)")
	}
	return out, false, nil
}

// parseExtinf reads `-1 tvg-id="…" tvg-logo="…" group-title="…",Name`: the name is after the first comma that
// isn't inside quotes (attribute values may hold commas).
func parseExtinf(s string) tvChannel {
	inQuote := false
	cut := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			inQuote = !inQuote
		} else if s[i] == ',' && !inQuote {
			cut = i
			break
		}
	}
	head, name := s, ""
	if cut >= 0 {
		head, name = s[:cut], s[cut+1:]
	}
	c := tvChannel{Name: clip(strings.TrimSpace(name), 120)}
	for _, m := range m3uAttr.FindAllStringSubmatch(head, -1) {
		v := strings.TrimSpace(m[2])
		switch strings.ToLower(m[1]) {
		case "tvg-logo":
			c.Logo = clip(v, 1000)
		case "group-title":
			c.Group = clip(v, 80)
		case "tvg-id":
			c.ID = clip(v, 120)
		case "tvg-name":
			if c.Name == "" {
				c.Name = clip(v, 120)
			}
		}
	}
	return c
}

// fetchM3U downloads and reads the playlist at u (an error never holds the address: it may hold a password).
func fetchM3U(u string, lan bool) ([]tvChannel, bool, error) {
	pu, err := tvCheckURL(u, lan)
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequest(http.MethodGet, pu.String(), nil)
	if err != nil {
		return nil, false, errors.New("bad playlist address")
	}
	req.Header.Set("User-Agent", tvUserAgent)
	res, err := tvClient(lan, tvListTimeout).Do(req)
	if err != nil {
		err = cleanErr(err)
		if errors.Is(err, errTVBlocked) {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("couldn't download the playlist from %s: %v", pu.Hostname(), err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("the playlist's server %s answered %s", pu.Hostname(), res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, tvListMax+1))
	if err != nil {
		return nil, false, fmt.Errorf("couldn't download the playlist from %s", pu.Hostname())
	}
	if int64(len(data)) > tvListMax {
		return nil, false, fmt.Errorf("the playlist is bigger than %d MB", tvListMax>>20)
	}
	return parseM3U(strings.NewReader(string(data)))
}
