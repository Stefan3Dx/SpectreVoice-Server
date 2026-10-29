package main

// The server's TV: one IPTV channel for the whole server, played by the server itself (ffmpeg, tvplay.go) into the
// stream room "tv:main", which everyone watches like a screen share (/streams subscribe, with sound). The playlist
// (an M3U address or an uploaded .m3u file) is set by those with the "tv" permission; they also decide whether
// everyone may change the channel ("open"). ffmpeg starts when someone puts a channel on or comes to watch, and
// stops when nobody has watched for SV_TV_IDLE seconds (the channel chosen stays).
//
//	<- {"t":"tv","op":"state"}                    -> {"t":"tv","tv":STATE}
//	<- {"t":"tv","op":"playlist","url":U}         (tv) download and keep the playlist at U
//	<- {"t":"tv","op":"refresh"}                  (tv) read it again
//	<- {"t":"tv","op":"clear"}                    (tv) forget it (the TV goes off)
//	<- {"t":"tv","op":"open","v":bool}            (tv) may everyone change the channel
//	<- {"t":"tv","op":"play","i":N,"n":NAME}      (tv, or anyone when open) channel N (its name, to catch a list
//	                                              that changed meanwhile) on the TV
//	<- {"t":"tv","op":"stop"}                     (the same) the TV off for everyone
//	-> {"t":"tv","tv":STATE}                      to everyone, whenever it changes (and in the welcome as "tv")
//	-> {"t":"done","op":"tv","what":OP,"n":N}     a playlist read (N channels)
//	-> {"t":"error","op":"tv","why":"role"|"off"|"ffmpeg"|"none"|"fetch"|"bad"|"busy"|"rate","message":…}
//
//	STATE = {"on","why"?:"off"|"ffmpeg","set","n","rev","open","now"?:{"i","n","g","l","by","byName","at","room",
//	         "playing","live","err"},"kind"?,"host"?}   (kind and host, where the playlist is from, only to "tv")
//
//	GET  /tv/channels?token=T        -> {"rev":R,"items":[{"i","n","g","l":bool}]}  names, groups, whether a logo
//	GET  /tv/logo?i=N&token=T        -> the channel's logo, fetched by the server (no app asks another host)
//	POST /tv/playlist?token=T        (tv) the body is an .m3u file -> {"n":N,"cut":bool}
//
// The playlist's address and the channels' addresses hold the subscription's credentials: they never leave the
// server and are never logged; the uploaded file is kept readable by the server only. Every address the TV opens is
// checked to be public (tvnet.go; SV_TV_LAN allows the LAN).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const tvRoom = tvRoomPrefix + mainSpace

var (
	errNoPlaylist = errors.New("no playlist has been set for the TV yet")
	errTVBusy     = errors.New("the playlist is being read already; wait a moment")
)

// tvPlayGap: channel changes at most this often (each one restarts ffmpeg).
var tvPlayGap = 1500 * time.Millisecond

// tvSource: where the playlist is from (kept in the settings table as "tv_source").
type tvSource struct {
	Kind string `json:"kind"`          // "url" or "file" (data/tv/playlist.m3u)
	URL  string `json:"url,omitempty"` // never sent to an app, never logged
	N    int    `json:"n"`
	At   int64  `json:"at"` // when it was read (ms): the list's revision for the apps
	By   string `json:"by,omitempty"`
}

// tvChoice: the channel chosen, kept ("tv_now") while the TV is on, even with nobody watching.
type tvChoice struct {
	I      int    `json:"i"`
	Name   string `json:"n"`
	By     string `json:"by"`
	ByName string `json:"byName"`
	At     int64  `json:"at"`
}

type tvManager struct {
	s        *server
	mu       sync.Mutex
	src      *tvSource
	open     bool
	now      *tvChoice
	list     []tvChannel
	listErr  error
	listAt   time.Time
	loading  chan struct{}
	player   *tvPlayer
	proxy    *tvProxy
	busy     bool // a playlist is being read
	lastPlay time.Time
	ffErr    string
	startMu  sync.Mutex // one player at a time
	fetch    func(u string) ([]tvChannel, bool, error)
	idle     time.Duration
	idleTick time.Duration
	kick     chan struct{}
	logos    *tvLogos
	party    *tvParty // reactions and lines over the picture (tvparty.go)

	modeOnce            sync.Once
	mode, vaProf, vaDev string
}

func newTV(s *server) *tvManager {
	tm := &tvManager{s: s, idle: time.Duration(max(s.cfg.tvIdle, 10)) * time.Second, idleTick: 5 * time.Second, kick: make(chan struct{}, 1), party: newTVParty()}
	tm.fetch = func(u string) ([]tvChannel, bool, error) { return fetchM3U(u, s.cfg.tvLAN) }
	tm.logos = newTVLogos(s.cfg.tvLAN)
	st := s.store
	// Roles from before the TV permission: those that could manage the server get it
	if _, ok := st.setting("perm_tv"); !ok {
		_, _ = st.db.Exec(`UPDATE roles SET perms = perms | ? WHERE perms & ? != 0`, int64(pTV), int64(pManageCommunity))
		_ = st.setSetting("perm_tv", "1")
	}
	if v, ok := st.setting("tv_source"); ok {
		var src tvSource
		if json.Unmarshal([]byte(v), &src) == nil && (src.Kind == "url" || src.Kind == "file") {
			tm.src = &src
		}
	}
	if v, ok := st.setting("tv_open"); ok {
		tm.open = v == "true"
	}
	if v, ok := st.setting("tv_now"); ok && tm.src != nil {
		var c tvChoice
		if json.Unmarshal([]byte(v), &c) == nil {
			tm.now = &c
		}
	}
	if s.cfg.tv {
		if _, err := tm.ffmpegPath(); err != nil {
			tm.ffErr = err.Error()
			log.Printf("tv: %v: the TV won't play until it is", err)
		}
	}
	go tm.loop()
	return tm
}

func (tm *tvManager) enabled() bool { return tm != nil && tm.s.cfg.tv }

func (tm *tvManager) playlistFile() string { return filepath.Join(tm.s.cfg.data, "tv", "playlist.m3u") }

func (st *store) delSetting(key string) { _, _ = st.db.Exec(`DELETE FROM settings WHERE key=?`, key) }

// ====== What the apps are told ======

// changed: the state is sent to everyone soon (several changes together, once).
func (tm *tvManager) changed() {
	if tm == nil {
		return
	}
	select {
	case tm.kick <- struct{}{}:
	default:
	}
}

func (tm *tvManager) loop() {
	for range tm.kick {
		time.Sleep(50 * time.Millisecond)
		h := tm.s.hub
		if h == nil {
			continue
		}
		full, _ := json.Marshal(map[string]any{"t": "tv", "tv": tm.state(true)})
		pub, _ := json.Marshal(map[string]any{"t": "tv", "tv": tm.state(false)})
		h.mu.Lock()
		for _, ss := range h.sessions {
			if ss.comm != spaceID || ss.api {
				continue
			}
			if h.may(ss, pTV) {
				ss.queue(full)
			} else {
				ss.queue(pub)
			}
		}
		h.mu.Unlock()
	}
}

// state: the TV as the apps see it ([manage]: also where the playlist is from, never its address).
func (tm *tvManager) state(manage bool) map[string]any {
	if tm == nil {
		return map[string]any{"on": false, "why": "off"}
	}
	// Who watches (the room's lock, before the TV's)
	watching, watchers := tm.watchers()
	tm.mu.Lock()
	defer tm.mu.Unlock()
	st := map[string]any{"on": tm.s.cfg.tv, "open": tm.open, "set": tm.src != nil, "n": 0, "rev": int64(0), "watching": watching, "watchers": watchers}
	switch {
	case !tm.s.cfg.tv:
		st["why"] = "off"
	case tm.ffErr != "":
		st["why"] = "ffmpeg"
	}
	if tm.src != nil {
		st["n"], st["rev"] = tm.src.N, tm.src.At
		if manage {
			st["kind"] = tm.src.Kind
			if tm.src.Kind == "url" {
				if u, err := url.Parse(tm.src.URL); err == nil {
					st["host"] = u.Hostname()
				}
			}
		}
	}
	if c := tm.now; c != nil {
		n := map[string]any{"i": c.I, "n": c.Name, "by": c.By, "byName": c.ByName, "at": c.At, "room": tvRoom, "playing": false, "live": false, "err": ""}
		if c.I >= 0 && c.I < len(tm.list) {
			n["g"], n["l"] = tm.list[c.I].Group, tm.list[c.I].Logo != ""
		}
		if p := tm.player; p != nil {
			idx, live, why := p.status()
			n["playing"] = true
			n["live"] = live && idx == c.I
			n["err"] = why
		}
		st["now"] = n
	}
	return st
}

// ====== The playlist ======

// channels gives the playlist's channels, reading the kept one first if this server hasn't yet (after a restart).
func (tm *tvManager) channels() ([]tvChannel, error) {
	tm.mu.Lock()
	if tm.list != nil {
		l := tm.list
		tm.mu.Unlock()
		return l, nil
	}
	if tm.src == nil {
		tm.mu.Unlock()
		return nil, errNoPlaylist
	}
	// A failed read is tried again after a while (the provider was down), not on every request
	if tm.listErr != nil && time.Since(tm.listAt) < 30*time.Second {
		err := tm.listErr
		tm.mu.Unlock()
		return nil, err
	}
	ch := tm.loading
	if ch == nil {
		ch = make(chan struct{})
		tm.loading = ch
		src := *tm.src
		go func() {
			chs, _, err := tm.read(src)
			tm.mu.Lock()
			tm.loading = nil
			if tm.src != nil && tm.src.At == src.At {
				tm.list, tm.listErr, tm.listAt = chs, err, time.Now()
			}
			tm.mu.Unlock()
			close(ch)
			if err != nil {
				log.Printf("tv: the playlist couldn't be read: %v", err)
			} else {
				log.Printf("tv: playlist read, %d channels", len(chs))
				tm.changed()
			}
		}()
	}
	tm.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(tvListTimeout + 10*time.Second):
		return nil, errors.New("the playlist is taking too long")
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.list != nil {
		return tm.list, nil
	}
	if tm.listErr != nil {
		return nil, tm.listErr
	}
	return nil, errNoPlaylist
}

func (tm *tvManager) read(src tvSource) ([]tvChannel, bool, error) {
	if src.Kind == "url" {
		return tm.fetch(src.URL)
	}
	f, err := os.Open(tm.playlistFile())
	if err != nil {
		return nil, false, errors.New("the uploaded playlist is gone from the server")
	}
	defer f.Close()
	return parseM3U(io.LimitReader(f, tvListMax))
}

// begin: one playlist change at a time.
func (tm *tvManager) begin() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.busy {
		return false
	}
	tm.busy = true
	return true
}

func (tm *tvManager) end() {
	tm.mu.Lock()
	tm.busy = false
	tm.mu.Unlock()
}

// setURL downloads the playlist at u and keeps it (and u).
func (tm *tvManager) setURL(u, by string) (int, bool, error) {
	if _, err := tvCheckURL(u, tm.s.cfg.tvLAN); err != nil {
		return 0, false, err
	}
	if !tm.begin() {
		return 0, false, errTVBusy
	}
	defer tm.end()
	chs, cut, err := tm.fetch(strings.TrimSpace(u))
	if err != nil {
		return 0, false, err
	}
	src := &tvSource{Kind: "url", URL: strings.TrimSpace(u), N: len(chs), At: time.Now().UnixMilli(), By: by}
	if err := tm.install(src, chs); err != nil {
		return 0, false, err
	}
	log.Printf("tv: playlist from a link set by %s: %d channels%s", by, len(chs), map[bool]string{true: " (cut)", false: ""}[cut])
	return len(chs), cut, nil
}

// setFile keeps an uploaded playlist.
func (tm *tvManager) setFile(data []byte, by string) (int, bool, error) {
	if int64(len(data)) > tvListMax {
		return 0, false, fmt.Errorf("the playlist is bigger than %d MB", tvListMax>>20)
	}
	if !tm.begin() {
		return 0, false, errTVBusy
	}
	defer tm.end()
	chs, cut, err := parseM3U(strings.NewReader(string(data)))
	if err != nil {
		return 0, false, err
	}
	dir := filepath.Dir(tm.playlistFile())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, false, err
	}
	_ = os.Chmod(dir, 0o700)
	if err := writeFileAtomic(tm.playlistFile(), data, 0o600); err != nil {
		return 0, false, errors.New("the playlist couldn't be kept on the server")
	}
	src := &tvSource{Kind: "file", N: len(chs), At: time.Now().UnixMilli(), By: by}
	if err := tm.install(src, chs); err != nil {
		return 0, false, err
	}
	log.Printf("tv: playlist file set by %s: %d channels%s", by, len(chs), map[bool]string{true: " (cut)", false: ""}[cut])
	return len(chs), cut, nil
}

// refresh reads the kept playlist again (a link: downloaded again).
func (tm *tvManager) refresh(by string) (int, bool, error) {
	tm.mu.Lock()
	src := tm.src
	tm.mu.Unlock()
	if src == nil {
		return 0, false, errNoPlaylist
	}
	if src.Kind == "url" {
		return tm.setURL(src.URL, by)
	}
	if !tm.begin() {
		return 0, false, errTVBusy
	}
	defer tm.end()
	chs, cut, err := tm.read(*src)
	if err != nil {
		return 0, false, err
	}
	next := *src
	next.N, next.At, next.By = len(chs), time.Now().UnixMilli(), by
	if err := tm.install(&next, chs); err != nil {
		return 0, false, err
	}
	return len(chs), cut, nil
}

// install makes chs the TV's channels: kept, the channel on now found again by its name (or the TV off).
func (tm *tvManager) install(src *tvSource, chs []tvChannel) error {
	raw, _ := json.Marshal(src)
	if err := tm.s.store.setSetting("tv_source", string(raw)); err != nil {
		return errors.New("the playlist couldn't be kept on the server")
	}
	if src.Kind == "url" {
		_ = os.Remove(tm.playlistFile())
	}
	tm.mu.Lock()
	tm.src, tm.list, tm.listErr, tm.listAt = src, chs, nil, time.Now()
	var off, restart *tvPlayer
	var ch tvChannel
	idx := -1
	if c := tm.now; c != nil {
		idx = findChannel(chs, c.Name, c.I)
		if idx < 0 {
			off, tm.player, tm.now = tm.player, nil, nil
			tm.s.store.delSetting("tv_now")
		} else {
			c.I = idx
			tm.saveNow()
			if tm.player != nil {
				restart, ch = tm.player, chs[idx]
			}
		}
	}
	tm.mu.Unlock()
	if off != nil {
		off.close()
	}
	if restart != nil {
		restart.play(ch, idx) // its address may have changed (new credentials)
	}
	tm.logos.reset()
	tm.changed()
	return nil
}

// findChannel: the channel named name, preferring index i; -1 when there's none.
func findChannel(chs []tvChannel, name string, i int) int {
	if i >= 0 && i < len(chs) && chs[i].Name == name {
		return i
	}
	for j, c := range chs {
		if c.Name == name {
			return j
		}
	}
	return -1
}

// clear forgets the playlist: the TV off, the file gone.
func (tm *tvManager) clear(by string) {
	st := tm.s.store
	st.delSetting("tv_source")
	st.delSetting("tv_now")
	_ = os.Remove(tm.playlistFile())
	tm.mu.Lock()
	p := tm.player
	tm.src, tm.list, tm.listErr, tm.now, tm.player = nil, nil, nil, nil, nil
	tm.mu.Unlock()
	if p != nil {
		p.close()
	}
	tm.logos.reset()
	log.Printf("tv: playlist removed by %s", by)
	tm.changed()
}

func (tm *tvManager) setOpen(v bool, by string) {
	_ = tm.s.store.setSetting("tv_open", strconv.FormatBool(v))
	tm.mu.Lock()
	tm.open = v
	tm.mu.Unlock()
	log.Printf("tv: anyone may change the channel: %v (by %s)", v, by)
	tm.changed()
}

// saveNow keeps the channel chosen; tm.mu held.
func (tm *tvManager) saveNow() {
	if tm.now == nil {
		tm.s.store.delSetting("tv_now")
		return
	}
	raw, _ := json.Marshal(tm.now)
	_ = tm.s.store.setSetting("tv_now", string(raw))
}

// ====== Playing ======

// play puts channel i on the TV (starting it if it's off): why and the error when it can't.
func (tm *tvManager) play(i int, name, by, byName string) (string, error) {
	if _, err := tm.ffmpegPath(); err != nil {
		tm.mu.Lock()
		tm.ffErr = err.Error()
		tm.mu.Unlock()
		tm.changed()
		return "ffmpeg", err
	}
	chs, err := tm.channels()
	if err != nil {
		if errors.Is(err, errNoPlaylist) {
			return "none", err
		}
		return "fetch", err
	}
	if i < 0 || i >= len(chs) || (name != "" && chs[i].Name != name) {
		return "bad", errors.New("no such channel (the list changed?)")
	}
	tm.mu.Lock()
	tm.ffErr = ""
	if time.Since(tm.lastPlay) < tvPlayGap {
		tm.mu.Unlock()
		return "rate", errors.New("the channel was changed a moment ago")
	}
	tm.lastPlay = time.Now()
	tm.now = &tvChoice{I: i, Name: chs[i].Name, By: by, ByName: byName, At: time.Now().UnixMilli()}
	tm.saveNow()
	tm.mu.Unlock()
	p, err := tm.startPlayer()
	if err != nil {
		tm.changed()
		return "busy", err
	}
	log.Printf("tv: %q by %s", chs[i].Name, by)
	p.play(chs[i], i)
	return "", nil
}

// startPlayer gives the running player, or starts one (the room opened, no channel yet).
func (tm *tvManager) startPlayer() (*tvPlayer, error) {
	tm.startMu.Lock()
	defer tm.startMu.Unlock()
	tm.mu.Lock()
	if p := tm.player; p != nil {
		tm.mu.Unlock()
		return p, nil
	}
	tm.mu.Unlock()
	p, err := newTVPlayer(tm)
	if err != nil {
		return nil, err
	}
	tm.mu.Lock()
	tm.player = p
	tm.mu.Unlock()
	tm.changed()
	return p, nil
}

// ensure: someone came to watch: the TV's room, its channel started if it was off (nil when there's nothing on).
func (tm *tvManager) ensure() *room {
	if !tm.enabled() {
		return nil
	}
	tm.mu.Lock()
	c, p := tm.now, tm.player
	tm.mu.Unlock()
	if p != nil {
		return p.rm
	}
	if c == nil {
		return nil
	}
	if _, err := tm.ffmpegPath(); err != nil {
		return nil
	}
	chs, err := tm.channels()
	if err != nil {
		return nil
	}
	idx := findChannel(chs, c.Name, c.I)
	if idx < 0 {
		return nil
	}
	p, err = tm.startPlayer()
	if err != nil {
		log.Printf("tv: %v", err)
		return nil
	}
	if cur, _, _ := p.status(); cur != idx {
		p.play(chs[idx], idx)
	}
	return p.rm
}

// stop turns the TV off for everyone.
func (tm *tvManager) stop(by string) {
	tm.mu.Lock()
	p := tm.player
	tm.player, tm.now = nil, nil
	tm.saveNow()
	tm.mu.Unlock()
	if p != nil {
		p.close()
	}
	// Who turned it off goes in the log ("Turn off" taken for "stop watching" ends it for everyone)
	log.Printf("tv: turned off by %s", by)
	tm.changed()
}

// playerIdle: nobody watched p for a while: it goes, the channel chosen stays.
func (tm *tvManager) playerIdle(p *tvPlayer) {
	tm.mu.Lock()
	if tm.player == p {
		tm.player = nil
	}
	tm.mu.Unlock()
	p.close()
	tm.changed()
}

// stopAll ends the player (the server is stopping, or a test ends).
func (tm *tvManager) stopAll() {
	if tm == nil {
		return
	}
	tm.mu.Lock()
	p := tm.player
	tm.player = nil
	px := tm.proxy
	tm.proxy = nil
	tm.mu.Unlock()
	if p != nil {
		p.close()
		p.wait()
	}
	if px != nil {
		px.close()
	}
}

// proxyURL: the TV's proxy for ffmpeg (started the first time).
func (tm *tvManager) proxyURL() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.proxy == nil {
		p, err := startTVProxy(tm.s.cfg.tvLAN)
		if err != nil {
			return "", err
		}
		tm.proxy = p
	}
	return tm.proxy.env, nil
}

// proxyBlocked: when the proxy last refused an address (unix ns; 0: never).
func (tm *tvManager) proxyBlocked() int64 {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.proxy == nil {
		return 0
	}
	return tm.proxy.blocked.Load()
}

// ====== From the apps ======

var tvWhyText = map[string]string{
	"role": "you aren't allowed to change the TV", "off": "this server has no TV (SV_TV=false)", "bad": "unknown TV operation",
}

func (h *hub) tvOp(ss *session, m map[string]any) {
	tm := h.s.tv
	op := str(m, "op")
	h.mu.Lock()
	manage := !ss.api && h.may(ss, pTV)
	name := ss.name
	h.mu.Unlock()
	refuse := func(why, msg string) {
		if msg == "" {
			msg = tvWhyText[why]
		}
		ss.send(map[string]any{"t": "error", "op": "tv", "what": op, "why": why, "message": msg})
	}
	if op == "state" {
		ss.send(map[string]any{"t": "tv", "tv": tm.state(manage)})
		return
	}
	if h.tvPartyOp(ss, op, m) {
		return
	}
	if !tm.enabled() {
		refuse("off", "")
		return
	}
	tm.mu.Lock()
	open := tm.open
	tm.mu.Unlock()
	switch op {
	case "playlist", "refresh", "clear", "open":
		if !manage {
			refuse("role", "")
			return
		}
	case "play", "stop":
		if !manage && !open {
			refuse("role", "")
			return
		}
	default:
		refuse("bad", "")
		return
	}
	done := func(n int, cut bool, err error) {
		if err != nil {
			why := "fetch"
			switch {
			case errors.Is(err, errNoPlaylist):
				why = "none"
			case errors.Is(err, errTVBusy):
				why = "busy"
			}
			refuse(why, err.Error())
			return
		}
		ss.send(map[string]any{"t": "done", "op": "tv", "what": op, "n": n, "cut": cut})
	}
	switch op {
	case "playlist":
		u := strings.TrimSpace(str(m, "url"))
		if u == "" || len(u) > 4096 {
			refuse("bad", "no playlist address")
			return
		}
		go func() { n, cut, err := tm.setURL(u, ss.uid); done(n, cut, err) }()
	case "refresh":
		go func() { n, cut, err := tm.refresh(ss.uid); done(n, cut, err) }()
	case "clear":
		tm.clear(ss.uid)
		ss.send(map[string]any{"t": "done", "op": "tv", "what": op})
	case "open":
		v, _ := boolOf(m, "v")
		tm.setOpen(v, ss.uid)
	case "play":
		i, ok := m["i"].(float64)
		if !ok || i < 0 || i != float64(int(i)) {
			refuse("bad", "no channel")
			return
		}
		go func() {
			if why, err := tm.play(int(i), str(m, "n"), ss.uid, name); err != nil {
				refuse(why, err.Error())
			}
		}()
	case "stop":
		tm.stop(ss.uid)
	}
}

// ====== HTTP ======

// tvViewer: the request's person, if they may see the TV (on the server, not banned).
func (s *server) tvViewer(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, ok := s.tokenUID(r)
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return "", false
	}
	if !s.mayUseSpace(mainSpace, uid) {
		http.Error(w, "not on this server", http.StatusForbidden)
		return "", false
	}
	if !s.tv.enabled() {
		http.Error(w, "this server has no TV", http.StatusNotFound)
		return "", false
	}
	return uid, true
}

func tvJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// GET /tv/channels: the channel list without addresses.
func (s *server) handleTVChannels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.tvViewer(w, r); !ok {
		return
	}
	tm := s.tv
	chs, err := tm.channels()
	if err != nil {
		code := http.StatusBadGateway
		why := "fetch"
		if errors.Is(err, errNoPlaylist) {
			code, why = http.StatusNotFound, "none"
		}
		tvJSON(w, code, map[string]string{"error": err.Error(), "why": why})
		return
	}
	tm.mu.Lock()
	rev := int64(0)
	if tm.src != nil {
		rev = tm.src.At
	}
	tm.mu.Unlock()
	etag := fmt.Sprintf(`"tv-%d-%d"`, rev, len(chs))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	type entry struct {
		I int    `json:"i"`
		N string `json:"n"`
		G string `json:"g,omitempty"`
		L bool   `json:"l,omitempty"`
	}
	out := make([]entry, len(chs))
	for i, c := range chs {
		out[i] = entry{i, c.Name, c.Group, c.Logo != ""}
	}
	w.Header().Set("ETag", etag)
	tvJSON(w, http.StatusOK, map[string]any{"rev": rev, "items": out})
}

// GET /tv/logo?i=N: the channel's logo, through the server (the apps load pictures only from their own server).
func (s *server) handleTVLogo(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.tvViewer(w, r); !ok {
		return
	}
	i, err := strconv.Atoi(r.URL.Query().Get("i"))
	chs, lerr := s.tv.channels()
	if err != nil || lerr != nil || i < 0 || i >= len(chs) || chs[i].Logo == "" {
		http.NotFound(w, r)
		return
	}
	data, ctype, ok := s.tv.logos.get(chs[i].Logo)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// POST /tv/playlist: an uploaded .m3u (those with the TV permission).
func (s *server) handleTVPlaylist(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.tvViewer(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST the playlist", http.StatusMethodNotAllowed)
		return
	}
	s.hub.mu.Lock()
	may := s.hub.permsOf(spaceID, uid)&pTV != 0
	s.hub.mu.Unlock()
	if !may || s.store.botOf(uid) != nil {
		tvJSON(w, http.StatusForbidden, map[string]string{"error": tvWhyText["role"], "why": "role"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, tvListMax+1))
	if err != nil {
		tvJSON(w, http.StatusBadRequest, map[string]string{"error": "the upload broke off", "why": "bad"})
		return
	}
	n, cut, err := s.tv.setFile(data, uid)
	if err != nil {
		code, why := http.StatusBadRequest, "bad"
		if errors.Is(err, errTVBusy) {
			code, why = http.StatusConflict, "busy"
		}
		tvJSON(w, code, map[string]string{"error": err.Error(), "why": why})
		return
	}
	tvJSON(w, http.StatusOK, map[string]any{"n": n, "cut": cut})
}

// ====== Logos ======

// tvLogos: the channels' logos, fetched by the server (small, pictures only) and kept in memory a while.
type tvLogos struct {
	mu     sync.Mutex
	m      map[string]*tvLogo
	order  []string
	bytes  int
	sem    chan struct{}
	client *http.Client
}

type tvLogo struct {
	ready chan struct{}
	data  []byte
	ctype string
	at    time.Time
}

const (
	tvLogoMax     = 512 << 10
	tvLogoEntries = 600
	tvLogoBytes   = 48 << 20
)

func newTVLogos(lan bool) *tvLogos {
	return &tvLogos{m: map[string]*tvLogo{}, sem: make(chan struct{}, 4), client: tvClient(lan, 10*time.Second)}
}

func (l *tvLogos) reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.m, l.order, l.bytes = map[string]*tvLogo{}, nil, 0
	l.mu.Unlock()
}

// get: the logo at u (a failure is remembered for 10 minutes).
func (l *tvLogos) get(u string) ([]byte, string, bool) {
	l.mu.Lock()
	e := l.m[u]
	if e != nil {
		select {
		case <-e.ready:
			if e.data == nil && time.Since(e.at) > 10*time.Minute {
				e = nil
			}
		default:
		}
	}
	if e == nil {
		e = &tvLogo{ready: make(chan struct{})}
		l.m[u] = e
		l.order = append(l.order, u)
		for len(l.order) > 0 && (len(l.order) > tvLogoEntries || l.bytes > tvLogoBytes) {
			old := l.order[0]
			l.order = l.order[1:]
			if o := l.m[old]; o != nil && o != e {
				l.bytes -= len(o.data)
				delete(l.m, old)
			}
		}
		go l.fetch(u, e)
	}
	l.mu.Unlock()
	select {
	case <-e.ready:
	case <-time.After(15 * time.Second):
		return nil, "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return e.data, e.ctype, e.data != nil
}

func (l *tvLogos) fetch(u string, e *tvLogo) {
	defer close(e.ready)
	defer func() { e.at = time.Now() }()
	l.sem <- struct{}{}
	defer func() { <-l.sem }()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", tvUserAgent)
	res, err := l.client.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, tvLogoMax+1))
	if err != nil || len(data) > tvLogoMax || len(data) == 0 {
		return
	}
	// What it is, by its bytes (not by what the other server says): pictures only, no SVG
	ct := http.DetectContentType(data)
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp", "image/x-icon", "image/vnd.microsoft.icon":
	default:
		return
	}
	l.mu.Lock()
	e.data, e.ctype = data, ct
	// Counted only while kept: one pushed out while it came, or from before a reset, is the waiting request's alone
	if l.m[u] == e {
		l.bytes += len(data)
	}
	l.mu.Unlock()
}
