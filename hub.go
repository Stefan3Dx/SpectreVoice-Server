package main

// The voice server's own connection: one WebSocket per app (GET /v1/ws), JSON messages with a "t" field.
//
//	-> {"t":"challenge","n":…}
//	<- {"t":"hello","pub":…,"sig":…,"name":…,"client":"android/1.2.0","password":…}
//	-> {"t":"welcome","sid":…,"uid":…,"token":…,"role":…,"server":{…},"channels":[…],"members":[…]}
//
// After that, both ways (see the handlers below): join, say/history/dms/edit/unsay, relay, poke, state, name,
// ch-save/ch-del/move (admins), rtc/ice (voice, voice.go). The server sends channel/channel-del,
// member/member-left, talk, msg, history, dms, poke, rtc/ice and error.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	maxText     = 4000
	maxName     = 32
	historyPage = 50
	sessQueue   = 512
)

// Vars so tests can change them.
var (
	sessRatePerSec float64 = 40
	sessRateBurst  float64 = 200
	helloTimeout           = 15 * time.Second
	// One person's connections at once (a PC, a phone, a tablet); admins aren't counted
	maxSessionsPerUID = 3
)

type session struct {
	// relay budget (bytes it may still pass on to others; only its own read loop touches these)
	relayLeft float64
	relayAt   time.Time
	sid       int
	uid       string
	name      string
	role      role    // the simple role in its community (for the apps that show only that)
	roles     []int64 // its roles in its community
	client    string
	comm      int64  // the community it's in now (0: none yet)
	base      string // how the app reached the server (https://sfu.example.com), for links
	ch        int64
	mic       bool // muted microphone
	snd       bool // muted sound (deafened)
	away      bool
	talk      bool
	ws        *websocket.Conn
	out       chan []byte
	done      chan struct{}
	once      sync.Once
	voice     *voicePeer
	// Whisper: while on, the voice goes only to these sessions and channels (not to one's own channel)
	whisperTo  []int
	whisperCh  []int64
	whispering bool
	nonce      string // the app's own (the server signs it: license.go)
	awayMsg    string
	rtt, loss  int   // the app's own measure of its connection (ms, % lost)
	since      int64 // connected at (ms)
	vmuted     bool  // a moderator silenced the voice
	talker     bool  // may talk in a moderated channel
	bot        bool  // the music bot
	tokens     float64
	refill     time.Time
	passTries  []time.Time // channel password attempts (its own read loop only)
	dev        string      // the device it came from (optional; the app's own random id)
	drops      int
}

type member struct {
	SID    int     `json:"sid"`
	UID    string  `json:"uid"`
	Name   string  `json:"name"`
	Ch     int64   `json:"ch"`
	Role   role    `json:"role,omitempty"`
	Roles  []int64 `json:"roles,omitempty"`
	Client string  `json:"client,omitempty"`
	Mic    bool    `json:"mic,omitempty"`
	Snd    bool    `json:"snd,omitempty"`
	Away   bool    `json:"away,omitempty"`
	Talk   bool    `json:"talk,omitempty"`
	Whisp  bool    `json:"whisper,omitempty"`
	AwayM  string  `json:"awayMsg,omitempty"`
	RTT    int     `json:"rtt,omitempty"`
	Loss   int     `json:"loss,omitempty"`
	Since  int64   `json:"since,omitempty"`
	VMuted bool    `json:"vmuted,omitempty"`
	Talker bool    `json:"talker,omitempty"`
	Bot    bool    `json:"bot,omitempty"`
}

// A stream on the server now (streams.go), told to everyone so the apps show who's live.
type liveStream struct {
	Room  string `json:"room"`
	UID   string `json:"uid"`
	Title string `json:"title,omitempty"`
	Since int64  `json:"since"`
	Comm  int64  `json:"comm,omitempty"` // only that community is told (and may watch)
}

func (ss *session) member() member {
	return member{SID: ss.sid, UID: ss.uid, Name: ss.name, Ch: ss.ch, Role: ss.role, Roles: ss.roles, Client: ss.client, Mic: ss.mic, Snd: ss.snd, Away: ss.away, Talk: ss.talk, Whisp: ss.whispering,
		AwayM: ss.awayMsg, RTT: ss.rtt, Loss: ss.loss, Since: ss.since, VMuted: ss.vmuted, Talker: ss.talker, Bot: ss.bot}
}

func (ss *session) close() {
	ss.once.Do(func() {
		close(ss.done)
		if ss.ws != nil {
			_ = ss.ws.Close()
		}
	})
}

// send queues v for the session's writer; one too far behind is let go.
func (ss *session) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	ss.queue(data)
}

func (ss *session) queue(data []byte) {
	select {
	case <-ss.done:
		return
	default:
	}
	select {
	case ss.out <- data:
	default:
		log.Printf("session %d (%s) falls behind, let go", ss.sid, ss.uid)
		ss.close()
	}
}

func (ss *session) writer() {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ss.done:
			return
		case data := <-ss.out:
			_ = ss.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ss.ws.WriteMessage(websocket.TextMessage, data); err != nil {
				ss.close()
				return
			}
		case <-t.C:
			if err := ss.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				ss.close()
				return
			}
		}
	}
}

func (ss *session) allow() bool {
	now := time.Now()
	if ss.refill.IsZero() {
		ss.tokens, ss.refill = sessRateBurst, now
	}
	ss.tokens = min(sessRateBurst, ss.tokens+now.Sub(ss.refill).Seconds()*sessRatePerSec)
	ss.refill = now
	if ss.tokens < 1 {
		ss.drops++
		return false
	}
	ss.tokens--
	return true
}

func (ss *session) refuse(op, why, msg string) {
	ss.send(map[string]any{"t": "error", "op": op, "why": why, "message": msg})
}

type hub struct {
	s        *server
	mu       sync.Mutex
	sessions map[int]*session
	chans    map[int64]*channel
	next     int
	live     map[string]liveStream
	acc      map[int64]*commAccess
	gate     ipGate
}

func newHub(s *server) (*hub, error) {
	h := &hub{s: s, sessions: map[int]*session{}, chans: map[int64]*channel{}, live: map[string]liveStream{}, acc: map[int64]*commAccess{}}
	cs, err := s.store.channels()
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		h.chans[c.ID] = c
	}
	return h, nil
}

// broadcast sends v to every session that passes keep (nil: all); h.mu held.
func (h *hub) broadcast(v any, keep func(*session) bool) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	for _, ss := range h.sessions {
		if keep == nil || keep(ss) {
			ss.queue(data)
		}
	}
}

// toComm sends v to everyone in community comm; h.mu held.
func (h *hub) toComm(comm int64, v any) {
	h.broadcast(v, func(o *session) bool { return o.comm == comm })
}

// memberChanged tells ss's community about it; h.mu held.
func (h *hub) memberChanged(ss *session) {
	h.toComm(ss.comm, map[string]any{"t": "member", "m": ss.member()})
}

// defaultChannel is where a newcomer to comm lands; h.mu held.
func (h *hub) defaultChannel(comm int64) int64 {
	var first int64
	for _, c := range h.sortedChannels(comm) {
		if c.Kind != "" { // text and spacers: nobody lands in them
			continue
		}
		if c.Default {
			return c.ID
		}
		if first == 0 && c.pass == "" {
			first = c.ID
		}
	}
	return first
}

func (h *hub) sortedChannels(comm int64) []*channel {
	out := make([]*channel, 0, len(h.chans))
	for _, c := range h.chans {
		if c.Comm == comm {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Parent != out[j].Parent {
			return out[i].Parent < out[j].Parent
		}
		if out[i].Pos != out[j].Pos {
			return out[i].Pos < out[j].Pos
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (h *hub) countIn(ch int64) int {
	n := 0
	for _, ss := range h.sessions {
		if ss.ch == ch {
			n++
		}
	}
	return n
}

// mayRead: a channel's chat is for its community; a locked one's only for who is in it (and who may pass locks).
func (h *hub) mayRead(ss *session, ch int64) bool {
	c := h.chans[ch]
	return c != nil && c.Comm == ss.comm && (c.pass == "" || ss.ch == ch || h.may(ss, pBypassLocks))
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, ReadBufferSize: 4096, WriteBufferSize: 4096}

func cleanName(n string) string {
	n = strings.Join(strings.Fields(n), " ")
	if utf8.RuneCountInString(n) > maxName {
		n = string([]rune(n)[:maxName])
	}
	return n
}

func (h *hub) handle(w http.ResponseWriter, r *http.Request) {
	// Unfinished handshakes and failed sign-ins per address: many idle sockets, or guessing the server's password,
	// get turned away before any work
	ip := requestIP(r)
	if !h.gate.begin(ip) {
		http.Error(w, "too many connections from your address; wait a little", http.StatusTooManyRequests)
		return
	}
	handshaking := true
	defer func() {
		if handshaking {
			h.gate.end(ip)
		}
	}()
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 10)
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if ws.WriteJSON(map[string]any{"t": "challenge", "n": base64.StdEncoding.EncodeToString(nonce), "version": version}) != nil {
		_ = ws.Close()
		return
	}
	fail := func(why, msg string) {
		if why == "auth" || why == "password" || why == "bad" {
			h.gate.failed(ip)
		}
		_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = ws.WriteJSON(map[string]any{"t": "error", "op": "hello", "why": why, "message": msg})
		_ = ws.Close()
	}
	_ = ws.SetReadDeadline(time.Now().Add(helloTimeout))
	var hello struct {
		T        string `json:"t"`
		Pub      string `json:"pub"`
		Sig      string `json:"sig"`
		Name     string `json:"name"`
		Client   string `json:"client"`
		Password string `json:"password"`
		Comm     int64  `json:"comm"`
		Nonce    string `json:"nonce"`
		V        int    `json:"v"`
		Dev      string `json:"dev"` // this device (optional): a new connection from it replaces its old one
	}
	if ws.ReadJSON(&hello) != nil || hello.T != "hello" {
		fail("bad", "say hello first")
		return
	}
	sig, _ := base64.StdEncoding.DecodeString(hello.Sig)
	// v2: signed for the host this request came to (Caddy keeps the Host header), so a signature made for another
	// server is refused here
	var uid string
	if hello.V >= 2 {
		uid, err = verifyIdentityV2(hello.Pub, r.Host, nonce, sig)
		if err != nil {
			err = errors.New("signature not for this server (" + hostOnly(r.Host) + ")")
		}
	} else {
		uid, err = verifyIdentity(hello.Pub, nonce, sig)
	}
	if err != nil {
		fail("auth", err.Error())
		return
	}
	name := cleanName(hello.Name)
	if name == "" {
		fail("name", "choose a name")
		return
	}
	// Nothing is written for a visitor until the password and bans let them in
	rl := h.s.store.role(uid)
	if h.s.cfg.joinPass != "" && !rl.atLeast(roleAdmin) && !constEq(hello.Password, h.s.cfg.joinPass) {
		fail("password", "wrong server password")
		return
	}
	if h.s.mods.banned(uid, 0) {
		fail("banned", "you are banned from this server")
		return
	}
	rl = h.s.store.seen(uid, hello.Pub, name)
	ss := &session{uid: uid, name: name, client: clip(hello.Client, 40), base: requestBase(r), ws: ws, out: make(chan []byte, sessQueue), done: make(chan struct{}),
		nonce: clip(hello.Nonce, 100), since: time.Now().UnixMilli(), dev: clip(hello.Dev, 64)}
	comm := h.pickCommunity(uid, hello.Comm)
	h.mu.Lock()
	// The same device again (a dropped Wi-Fi, before the old socket timed out): the old one goes, and doesn't count
	mine, gone := 0, 0
	for _, o := range h.sessions {
		if o.uid != uid || o.bot {
			continue
		}
		if ss.dev != "" && o.dev == ss.dev {
			gone++
			go o.close()
			continue
		}
		mine++
	}
	if mine >= maxSessionsPerUID && !rl.atLeast(roleAdmin) {
		h.mu.Unlock()
		fail("many", "you're connected from too many places")
		return
	}
	if mu := h.s.maxUsers(); mu > 0 && h.people()-gone >= mu && !rl.atLeast(roleAdmin) {
		h.mu.Unlock()
		fail("full", "the server is full")
		return
	}
	h.next++
	ss.sid = h.next
	h.sessions[ss.sid] = ss
	welcome := map[string]any{
		"t": "welcome", "sid": ss.sid, "uid": uid, "token": h.s.makeToken(uid),
		"server": map[string]any{"name": h.s.cfg.name, "motd": h.s.cfg.motd, "version": version, "ice": h.s.publicICE(), "create": h.s.cfg.createCommunities,
			"audio": audioLevels, "video": videoLevels, "audioDefault": h.s.cfg.audioLevel, "music": h.s.music.enabled(), "files": h.s.chfiles != nil},
		"tier": h.s.lic.welcome(),
	}
	for k, v := range h.enterLocked(ss, comm) {
		welcome[k] = v
	}
	ss.send(welcome)
	h.mu.Unlock()
	statJoins.Add(1)
	log.Printf("in: %s %q (%s) sid %d", uid, name, ss.client, ss.sid)
	go ss.writer()
	defer h.leave(ss)
	handshaking = false
	h.gate.end(ip)

	_ = ws.SetReadDeadline(time.Now().Add(busReadTimeout))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(busReadTimeout)) })
	for {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(busReadTimeout))
		t, _ := m["t"].(string)
		if !h.staff(ss.uid) && !ss.allow() {
			if ss.drops >= busRateDrops {
				return
			}
			if ss.drops%50 == 1 {
				ss.refuse(t, "rate", "too many messages, slow down")
			}
			continue
		}
		h.dispatch(ss, t, m)
	}
}

func (h *hub) leave(ss *session) {
	ss.close()
	h.mu.Lock()
	delete(h.sessions, ss.sid)
	ch := ss.ch
	h.toComm(ss.comm, map[string]any{"t": "member-left", "sid": ss.sid, "uid": ss.uid})
	h.mu.Unlock()
	h.s.voice.drop(ss)
	if ss.whispering {
		h.s.voice.syncAll()
	} else {
		h.s.voice.sync(ch)
	}
	log.Printf("out: %s sid %d", ss.uid, ss.sid)
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func i64(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func boolOf(m map[string]any, k string) (bool, bool) { v, ok := m[k].(bool); return v, ok }

func (h *hub) dispatch(ss *session, t string, m map[string]any) {
	switch t {
	case "ping":
		ss.send(map[string]any{"t": "pong", "n": m["n"]})
	case "join":
		h.join(ss, i64(m, "ch"), str(m, "password"))
	case "say", "relay":
		h.say(ss, t, str(m, "to"), m)
	case "history":
		h.history(ss, str(m, "to"), i64(m, "before"))
	case "dms":
		list, err := h.s.store.dms(ss.uid, 100)
		if err != nil {
			ss.refuse(t, "server", err.Error())
			return
		}
		ss.send(map[string]any{"t": "dms", "items": list})
	case "edit", "unsay":
		h.edit(ss, t, i64(m, "id"), str(m, "text"))
	case "poke":
		h.poke(ss, str(m, "uid"), str(m, "text"))
	case "state":
		h.mu.Lock()
		if v, ok := boolOf(m, "mic"); ok {
			ss.mic = v
		}
		if v, ok := boolOf(m, "snd"); ok {
			ss.snd = v
		}
		if v, ok := boolOf(m, "away"); ok {
			ss.away = v
			if !v {
				ss.awayMsg = ""
			}
		}
		if v, ok := m["awayMsg"].(string); ok {
			ss.awayMsg = clip(strings.TrimSpace(v), 120)
		}
		if _, ok := m["rtt"]; ok {
			ss.rtt = int(min(max(i64(m, "rtt"), 0), 60000))
			ss.loss = int(min(max(i64(m, "loss"), 0), 100))
		}
		if ss.mic && ss.talk {
			ss.talk = false
		}
		h.memberChanged(ss)
		h.mu.Unlock()
	case "name":
		n := cleanName(str(m, "name"))
		if n == "" {
			ss.refuse(t, "name", "choose a name")
			return
		}
		h.mu.Lock()
		ss.name = n
		h.memberChanged(ss)
		h.mu.Unlock()
		h.s.store.seen(ss.uid, "", n)
	case "ch-save":
		h.saveChannel(ss, m)
	case "ch-del":
		h.deleteChannel(ss, i64(m, "id"))
	case "move":
		h.move(ss, int(i64(m, "sid")), i64(m, "ch"))
	case "whisper":
		h.whisper(ss, m)
	case "rtc", "ice":
		h.s.voice.signal(ss, t, m)
	default:
		if !h.communityOp(ss, t, m) && !h.codeOp(ss, t, m) && !h.s.mods.dispatch(ss, t, m) && !h.s.inviteOp(ss, t, m) && !h.s.pushOp(ss, t, m) &&
			!h.s.licenseOp(ss, t, m) && !h.s.adminOp(ss, t, m) && !h.featureOp(ss, t, m) && !h.s.music.op(ss, t, m) && !h.s.chfiles.op(ss, t, m) {
			ss.refuse(t, "bad", "unknown message")
		}
	}
}

// join moves ss into channel ch (a password, room for one more), and its voice with it.
func (h *hub) join(ss *session, ch int64, pass string) {
	// The password first, without the hub's lock: bcrypt takes ~80 ms, and wrong passwords on a loop would hold
	// everyone up. A few tries a minute per session.
	h.mu.Lock()
	var hash string
	if c := h.chans[ch]; c != nil && c.pass != "" && !h.may(ss, pBypassLocks) {
		hash = c.pass
	}
	h.mu.Unlock()
	if hash != "" {
		if !ss.passTry() {
			ss.refuse("join", "rate", "too many password tries; wait a minute")
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil {
			ss.refuse("join", "password", "wrong channel password")
			return
		}
	}
	h.mu.Lock()
	c := h.chans[ch]
	switch {
	case c == nil || c.Comm != ss.comm:
		h.mu.Unlock()
		ss.refuse("join", "gone", "no such channel")
		return
	case ss.ch == ch:
		h.mu.Unlock()
		return
	case c.Kind == "spacer" || c.Kind == "text":
		h.mu.Unlock()
		ss.refuse("join", "bad", "not a voice channel")
		return
	case !h.may(ss, pBypassLocks) && c.pass != "" && c.pass != hash:
		// The password changed meanwhile (or was just set): ask again
		h.mu.Unlock()
		ss.refuse("join", "password", "wrong channel password")
		return
	case !h.may(ss, pBypassLocks) && c.MaxUsers > 0 && h.countIn(ch) >= c.MaxUsers:
		h.mu.Unlock()
		ss.refuse("join", "full", "the channel is full")
		return
	case h.s.mods.channelBanned(ss.uid, ch) && !h.may(ss, pChannelBan):
		h.mu.Unlock()
		ss.refuse("join", "banned", "you are banned from this channel")
		return
	}
	old := ss.ch
	ss.ch = ch
	ss.talk = false
	ss.talker = false
	h.memberChanged(ss)
	h.mu.Unlock()
	h.s.voice.sync(old, ch)
	h.s.music.joined(ss, ch)
}

func (h *hub) move(ss *session, sid int, ch int64) {
	h.mu.Lock()
	if !h.may(ss, pMove) {
		h.mu.Unlock()
		ss.refuse("move", "role", "not allowed")
		return
	}
	o, c := h.sessions[sid], h.chans[ch]
	if o == nil || c == nil || o.ch == ch || o.comm != ss.comm || c.Comm != ss.comm || c.Kind != "" {
		h.mu.Unlock()
		return
	}
	if o != ss && !h.outranks(ss, o.uid) {
		h.mu.Unlock()
		ss.refuse("move", "role", "they rank as high as you")
		return
	}
	old := o.ch
	o.ch = ch
	o.talk, o.talker = false, false
	h.memberChanged(o)
	h.mu.Unlock()
	o.send(map[string]any{"t": "moved", "ch": ch, "by": ss.name})
	h.s.voice.sync(old, ch)
}

// target works out a "to": "c:<channel>" or "u:<uid>"; the conversation and who gets it.
func (h *hub) target(ss *session, to string) (conv string, ch int64, other string, err error) {
	switch {
	case strings.HasPrefix(to, "c:"):
		ch, _ = strconv.ParseInt(to[2:], 10, 64)
		h.mu.Lock()
		ok := h.mayRead(ss, ch)
		h.mu.Unlock()
		if !ok {
			return "", 0, "", fmt.Errorf("no such channel, or it's locked")
		}
		return chanConv(ch), ch, "", nil
	case strings.HasPrefix(to, "g:"):
		// The community's own chat, everyone in it
		comm, _ := strconv.ParseInt(to[2:], 10, 64)
		if comm == 0 || comm != h.commOf(ss) {
			return "", 0, "", fmt.Errorf("not in that community")
		}
		return to, -comm, "", nil
	case strings.HasPrefix(to, "u:"):
		other = to[2:]
		if !uidRe.MatchString(other) || other == ss.uid {
			return "", 0, "", fmt.Errorf("bad recipient")
		}
		return dmConv(ss.uid, other), 0, other, nil
	}
	return "", 0, "", fmt.Errorf("bad recipient")
}

// deliver sends v to who should get a message in conv; h.mu held.
func (h *hub) deliver(v any, ch int64, a, b string) {
	if ch < 0 {
		h.broadcast(v, func(o *session) bool { return o.comm == -ch })
		return
	}
	if ch != 0 {
		h.broadcast(v, func(o *session) bool { return h.mayRead(o, ch) })
		return
	}
	h.broadcast(v, func(o *session) bool { return o.uid == a || o.uid == b })
}

func (h *hub) say(ss *session, t, to string, m map[string]any) {
	conv, ch, other, err := h.target(ss, to)
	if err != nil {
		ss.refuse(t, "bad", err.Error())
		return
	}
	if comm := h.commOf(ss); comm != 0 && h.s.mods.muted(ss.uid, comm) {
		ss.refuse(t, "muted", "you can't write here for now")
		return
	}
	// Private messages only to people one shares a community with (not to any uid on the server)
	if other != "" && !h.s.store.sharesCommunity(ss.uid, other) {
		ss.refuse(t, "bad", "you don't share a community with them")
		return
	}
	if t == "relay" {
		// Games and other app-to-app messages: passed on, never kept
		data, ok := m["data"]
		if !ok {
			return
		}
		raw, _ := json.Marshal(data)
		if len(raw) > 32<<10 {
			ss.refuse(t, "full", "too long")
			return
		}
		// A byte budget (64 KB/s, bursts of 256 KB): one app can't flood a whole community off the server
		now := time.Now()
		ss.relayLeft = min(256<<10, ss.relayLeft+now.Sub(ss.relayAt).Seconds()*(64<<10))
		ss.relayAt = now
		if ss.relayLeft < float64(len(raw)) {
			ss.refuse(t, "slow", "too much at once")
			return
		}
		ss.relayLeft -= float64(len(raw))
		h.mu.Lock()
		h.deliver(map[string]any{"t": "relay", "to": conv, "from": ss.uid, "sid": ss.sid, "name": ss.name, "data": json.RawMessage(raw)}, ch, ss.uid, other)
		h.mu.Unlock()
		return
	}
	text := strings.TrimSpace(str(m, "text"))
	if text == "" || utf8.RuneCountInString(text) > maxText {
		ss.refuse(t, "bad", "empty or too long")
		return
	}
	msg := &chatMsg{To: conv, From: ss.uid, Name: ss.name, Text: text, At: time.Now().UnixMilli()}
	if err := h.s.store.addMessage(msg); err != nil {
		log.Printf("chat: %v", err)
		ss.refuse(t, "server", "not saved")
		return
	}
	statMessages.Add(1)
	out := map[string]any{"t": "msg", "m": msg}
	if n := str(m, "n"); n != "" {
		out["n"] = clip(n, 64) // the app's own id, so it can match its pending message
	}
	h.mu.Lock()
	h.deliver(out, ch, ss.uid, other)
	h.mu.Unlock()
	go h.pushFor(ss, msg, ch, other)
	if ch > 0 && strings.HasPrefix(text, "!") {
		go h.s.music.command(ss, ch, text)
	}
}

func (h *hub) history(ss *session, to string, before int64) {
	conv, _, _, err := h.target(ss, to)
	if err != nil {
		ss.refuse("history", "bad", err.Error())
		return
	}
	items, err := h.s.store.history(conv, before, historyPage+1)
	if err != nil {
		ss.refuse("history", "server", err.Error())
		return
	}
	more := len(items) > historyPage
	if more {
		items = items[1:]
	}
	if items == nil {
		items = []chatMsg{}
	}
	ss.send(map[string]any{"t": "history", "to": conv, "items": items, "more": more, "before": before})
}

// edit changes or takes back a message: its writer, or a moderator for a channel's.
func (h *hub) edit(ss *session, t string, id int64, text string) {
	msg, err := h.s.store.message(id)
	if err != nil || msg.Del {
		ss.refuse(t, "gone", "no such message")
		return
	}
	mine := msg.From == ss.uid
	isChan := strings.HasPrefix(msg.To, "c:")
	isComm := strings.HasPrefix(msg.To, "g:")
	if t == "edit" && !mine {
		ss.refuse(t, "owner", "not yours")
		return
	}
	// One's own message, but only where one may still write: in the community and the channel now, not banned
	// or muted there (a banned person can't rewrite what they left behind)
	if mine && (isChan || isComm) {
		cid, _ := strconv.ParseInt(msg.To[2:], 10, 64)
		h.mu.Lock()
		comm := ss.comm
		ok := false
		if isChan {
			ok = h.mayRead(ss, cid)
		} else {
			ok = cid == comm
		}
		h.mu.Unlock()
		if !ok || h.s.mods.banned(ss.uid, comm) || (t == "edit" && h.s.mods.muted(ss.uid, comm)) {
			ss.refuse(t, "owner", "you can't change messages there now")
			return
		}
	}
	if t == "unsay" && !mine {
		ok := false
		if isChan {
			cid, _ := strconv.ParseInt(msg.To[2:], 10, 64)
			h.mu.Lock()
			c := h.chans[cid]
			ok = c != nil && c.Comm == ss.comm && h.may(ss, pDeleteMessages)
			h.mu.Unlock()
		}
		if isComm {
			cid, _ := strconv.ParseInt(msg.To[2:], 10, 64)
			h.mu.Lock()
			ok = cid == ss.comm && h.may(ss, pDeleteMessages)
			h.mu.Unlock()
		}
		if !ok {
			ss.refuse(t, "owner", "not yours")
			return
		}
	}
	text = strings.TrimSpace(text)
	if t == "edit" {
		if text == "" || utf8.RuneCountInString(text) > maxText {
			ss.refuse(t, "bad", "empty or too long")
			return
		}
		msg.Text, msg.Edit = text, time.Now().UnixMilli()
		err = h.s.store.editMessage(id, text, msg.Edit)
	} else {
		msg.Text, msg.Del = "", true
		err = h.s.store.deleteMessage(id)
	}
	if err != nil {
		ss.refuse(t, "server", err.Error())
		return
	}
	var ch int64
	a, b := "", ""
	if isChan {
		ch, _ = strconv.ParseInt(msg.To[2:], 10, 64)
	} else if isComm {
		ch, _ = strconv.ParseInt(msg.To[2:], 10, 64)
		ch = -ch
	} else {
		rest := strings.TrimPrefix(msg.To, "d:")
		a, b, _ = strings.Cut(rest, ":")
	}
	h.mu.Lock()
	h.deliver(map[string]any{"t": "msg", "m": msg}, ch, a, b)
	h.mu.Unlock()
}

func (h *hub) poke(ss *session, uid, text string) {
	text = clip(strings.TrimSpace(text), 200)
	if !uidRe.MatchString(uid) || !h.s.store.sharesCommunity(ss.uid, uid) {
		ss.refuse("poke", "gone", "they aren't here")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, o := range h.sessions {
		if o.uid == uid {
			o.send(map[string]any{"t": "poke", "from": ss.uid, "name": ss.name, "text": text})
			n++
		}
	}
	if n == 0 {
		ss.refuse("poke", "gone", "they aren't here")
	}
}

func (h *hub) saveChannel(ss *session, m map[string]any) {
	h.mu.Lock()
	allowed := ss.comm != 0 && h.may(ss, pManageChannels)
	h.mu.Unlock()
	if !allowed {
		ss.refuse("ch-save", "role", "not allowed")
		return
	}
	name := cleanName(str(m, "name"))
	if name == "" {
		ss.refuse("ch-save", "bad", "a channel needs a name")
		return
	}
	h.mu.Lock()
	id := i64(m, "id")
	var c channel
	if id != 0 {
		old := h.chans[id]
		if old == nil || old.Comm != ss.comm {
			h.mu.Unlock()
			ss.refuse("ch-save", "gone", "no such channel")
			return
		}
		c = *old
	} else {
		c.Comm = ss.comm
	}
	c.Name = name
	c.Topic = clip(str(m, "topic"), 500)
	c.Parent = i64(m, "parent")
	c.Pos = int(i64(m, "pos"))
	c.MaxUsers = max(0, int(i64(m, "max")))
	if v, ok := boolOf(m, "default"); ok {
		c.Default = v
	}
	if _, ok := m["quality"]; ok {
		c.Quality = clampAudio(int(i64(m, "quality")), h.s.limits().AudioMax)
	}
	if v, ok := m["icon"].(string); ok {
		c.Icon = clip(strings.TrimSpace(v), 16)
	}
	if v, ok := m["pic"].(string); ok {
		v = strings.TrimSpace(v)
		if v != "" && !strings.HasPrefix(v, "https://") {
			h.mu.Unlock()
			ss.refuse("ch-save", "bad", "a channel picture is an https link")
			return
		}
		c.Pic = clip(v, 400)
	}
	if v, ok := m["kind"].(string); ok && (v == "" || v == "spacer" || v == "text") {
		c.Kind = v
	}
	if v, ok := boolOf(m, "moderated"); ok {
		c.Moderated = v
	}
	if c.Parent != 0 && (h.chans[c.Parent] == nil || h.chans[c.Parent].Comm != c.Comm || c.Parent == c.ID || (c.ID != 0 && h.isUnder(c.Parent, c.ID))) {
		h.mu.Unlock()
		ss.refuse("ch-save", "bad", "bad parent")
		return
	}
	if id == 0 && len(h.sortedChannels(c.Comm)) >= 500 {
		h.mu.Unlock()
		ss.refuse("ch-save", "full", "too many channels")
		return
	}
	h.mu.Unlock()
	if p, ok := m["password"].(string); ok {
		hp, err := hashPass(p)
		if err != nil {
			ss.refuse("ch-save", "bad", err.Error())
			return
		}
		c.pass = hp
	}
	if c.Kind == "text" || c.Kind == "spacer" {
		// Text channels are read by the whole community (their history stays on the server); nobody lands in them
		c.pass, c.Default, c.MaxUsers, c.Moderated = "", false, 0, false
	}
	c.HasPass = c.pass != ""
	if err := h.s.store.saveChannel(&c); err != nil {
		ss.refuse("ch-save", "server", err.Error())
		return
	}
	h.mu.Lock()
	if c.Default {
		for _, o := range h.chans {
			if o.Default && o.ID != c.ID && o.Comm == c.Comm {
				o.Default = false
			}
		}
	}
	cp := c
	h.chans[c.ID] = &cp
	h.toComm(c.Comm, map[string]any{"t": "channel", "c": cp})
	// Turned into a text channel or a spacer: who was talking in it goes to the default channel
	var def int64
	if c.Kind != "" {
		def = h.defaultChannel(c.Comm)
		for _, o := range h.sessions {
			if o.ch == c.ID && o.comm == c.Comm {
				o.ch, o.talk, o.talker = def, false, false
				h.memberChanged(o)
			}
		}
	}
	h.mu.Unlock()
	log.Printf("channel %d %q saved by %s", c.ID, c.Name, ss.uid)
	if id != 0 {
		h.s.voice.sync(c.ID, def) // moderated or not changes who's heard
	}
}

// isUnder: is ch somewhere below anc; h.mu held.
func (h *hub) isUnder(ch, anc int64) bool {
	for i := 0; i < 64 && ch != 0; i++ {
		c := h.chans[ch]
		if c == nil {
			return false
		}
		if c.Parent == anc {
			return true
		}
		ch = c.Parent
	}
	return false
}

func (h *hub) deleteChannel(ss *session, id int64) {
	h.mu.Lock()
	c := h.chans[id]
	if c == nil || c.Comm != ss.comm {
		h.mu.Unlock()
		return
	}
	if !h.may(ss, pManageChannels) {
		h.mu.Unlock()
		ss.refuse("ch-del", "role", "not allowed")
		return
	}
	// The channel and everything under it; whoever is in them goes to the default channel
	gone := []int64{id}
	for cid := range h.chans {
		if h.isUnder(cid, id) {
			gone = append(gone, cid)
		}
	}
	isGone := map[int64]bool{}
	for _, g := range gone {
		isGone[g] = true
	}
	def := h.defaultChannel(c.Comm)
	if isGone[def] {
		h.mu.Unlock()
		ss.refuse("ch-del", "bad", "make another channel the default first")
		return
	}
	h.mu.Unlock()
	for _, g := range gone {
		if err := h.s.store.deleteChannel(g); err != nil {
			ss.refuse("ch-del", "server", err.Error())
			return
		}
	}
	h.s.chfiles.dropChannels(gone)
	if h.s.mods != nil {
		h.s.mods.dropScope([]string{"ch-ban"}, gone...)
	}
	h.mu.Lock()
	var moved []*session
	for _, o := range h.sessions {
		if isGone[o.ch] {
			o.ch = def
			o.talk, o.talker = false, false
			moved = append(moved, o)
		}
	}
	for _, g := range gone {
		delete(h.chans, g)
		h.toComm(c.Comm, map[string]any{"t": "channel-del", "id": g})
	}
	for _, o := range moved {
		h.memberChanged(o)
	}
	h.mu.Unlock()
	h.s.voice.sync(append(gone, def)...)
}

// people: how many are connected (bots don't take a place); h.mu held.
func (h *hub) people() int {
	n := 0
	for _, o := range h.sessions {
		if !o.bot {
			n++
		}
	}
	return n
}

// sessionsOf gives a uid's sessions (a person may be on a phone and a PC).
func (h *hub) sessionsOf(uid string) []*session {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*session
	for _, o := range h.sessions {
		if o.uid == uid {
			out = append(out, o)
		}
	}
	return out
}

// refreshRoles works out again everyone's roles in comm (roles or memberships changed) and tells them; h.mu held.
func (h *hub) refreshRoles(comm int64) {
	h.forget(comm)
	a := h.access(comm)
	for _, o := range h.sessions {
		if o.comm != comm {
			continue
		}
		nr, ids := h.legacyRole(comm, o.uid), a.member[o.uid]
		o.role, o.roles = nr, ids
		o.send(map[string]any{"t": "role", "role": nr, "roles": ids, "perms": permList(h.permsOf(comm, o.uid)), "staff": h.staff(o.uid)})
		h.memberChanged(o)
	}
	if h.s.voice != nil {
		go h.s.voice.syncAll() // the speak permission may have changed (h.mu is held here)
	}
}

// setLive records a stream starting or ending and tells its community.
func (h *hub) setLive(room, uid, title string, comm int64, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if on {
		ls := liveStream{Room: room, UID: uid, Title: title, Since: time.Now().UnixMilli(), Comm: comm}
		h.live[room] = ls
		h.toComm(comm, map[string]any{"t": "live", "on": true, "s": ls})
		return
	}
	ls, ok := h.live[room]
	if !ok {
		return
	}
	delete(h.live, room)
	h.toComm(ls.Comm, map[string]any{"t": "live", "on": false, "s": liveStream{Room: room}})
}

// streamComm: the community uid streams in (where its app is now), if it may stream there.
func (h *hub) streamComm(uid string) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, o := range h.sessions {
		if o.uid == uid && !o.bot && o.comm != 0 {
			return o.comm, h.may(o, pStream)
		}
	}
	return 0, false
}

// mayWatch: uid may watch room (a member of the stream's community).
func (h *hub) mayWatch(uid, room string) bool {
	h.mu.Lock()
	ls, ok := h.live[room]
	h.mu.Unlock()
	return ok && (ls.Comm == 0 || h.s.store.isMember(ls.Comm, uid)) && !h.s.mods.banned(uid, ls.Comm)
}

// liveList: comm's streams now, oldest first; h.mu held.
func (h *hub) liveList(comm int64) []liveStream {
	out := make([]liveStream, 0, len(h.live))
	for _, l := range h.live {
		if l.Comm == comm {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since < out[j].Since })
	return out
}

// whisper: {"t":"whisper","on":true,"sids":[…],"chs":[…]} talks only to those people and channels while on;
// {"t":"whisper","on":false} back to one's channel. The voice is moved at once (a new offer to whoever changes).
func (h *hub) whisper(ss *session, m map[string]any) {
	on, _ := boolOf(m, "on")
	var sids []int
	var chs []int64
	if on {
		if l, ok := m["sids"].([]any); ok {
			for _, v := range l {
				if f, ok := v.(float64); ok && len(sids) < 50 {
					sids = append(sids, int(f))
				}
			}
		}
		if l, ok := m["chs"].([]any); ok {
			for _, v := range l {
				if f, ok := v.(float64); ok && len(chs) < 20 {
					chs = append(chs, int64(f))
				}
			}
		}
		if len(sids) == 0 && len(chs) == 0 {
			on = false
		}
	}
	h.mu.Lock()
	if on {
		// A channel one may not hear (locked, not in it) isn't whispered to, unless an admin
		kept := chs[:0]
		for _, c := range chs {
			if h.chans[c] != nil && h.chans[c].Comm == ss.comm && (h.chans[c].pass == "" || ss.ch == c || h.may(ss, pBypassLocks)) {
				kept = append(kept, c)
			}
		}
		chs = kept
	}
	if on {
		// Only people in the same community
		kept := sids[:0]
		for _, sid := range sids {
			if o := h.sessions[sid]; o != nil && o.comm == ss.comm {
				kept = append(kept, sid)
			}
		}
		sids = kept
	}
	changed := ss.whispering != on || !equalInts(ss.whisperTo, sids) || !equalInt64s(ss.whisperCh, chs)
	ss.whispering, ss.whisperTo, ss.whisperCh = on, sids, chs
	if changed {
		h.memberChanged(ss)
	}
	h.mu.Unlock()
	if changed {
		h.s.voice.syncAll()
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// passTry: may this session try a channel password now? Five a minute.
func (ss *session) passTry() bool {
	now := time.Now()
	kept := ss.passTries[:0]
	for _, t := range ss.passTries {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	ss.passTries = kept
	if len(kept) >= 5 {
		return false
	}
	ss.passTries = append(ss.passTries, now)
	return true
}

// ipGate: per address, the handshakes still going and the sign-ins that failed lately.
type ipGate struct {
	mu      sync.Mutex
	pending map[string]int
	fails   map[string][]time.Time
}

const (
	gatePending = 20 // unfinished handshakes from one address
	gateFails   = 20 // failed sign-ins from one address in 10 minutes
)

// gateFree: this machine itself (tests, local tools; behind the proxy the real address comes in its header)
func gateFree(ip string) bool { return ip == "127.0.0.1" || ip == "::1" }

func (g *ipGate) begin(ip string) bool {
	if gateFree(ip) {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending == nil {
		g.pending, g.fails = map[string]int{}, map[string][]time.Time{}
	}
	if g.pending[ip] >= gatePending || len(g.recent(ip)) >= gateFails {
		return false
	}
	g.pending[ip]++
	return true
}

func (g *ipGate) end(ip string) {
	if gateFree(ip) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending[ip] <= 1 {
		delete(g.pending, ip)
	} else {
		g.pending[ip]--
	}
}

func (g *ipGate) failed(ip string) {
	if gateFree(ip) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fails == nil {
		g.pending, g.fails = map[string]int{}, map[string][]time.Time{}
	}
	g.fails[ip] = append(g.recent(ip), time.Now())
	// Old addresses go (the map can't grow for ever)
	if len(g.fails) > 10000 {
		for k := range g.fails {
			if len(g.recent(k)) == 0 {
				delete(g.fails, k)
			}
		}
	}
}

// recent: ip's failures in the last 10 minutes; g.mu held.
func (g *ipGate) recent(ip string) []time.Time {
	kept := g.fails[ip][:0]
	for _, t := range g.fails[ip] {
		if time.Since(t) < 10*time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(g.fails, ip)
		return nil
	}
	g.fails[ip] = kept
	return kept
}

// uidRe: a person's uid (base64url of 15 bytes).
var uidRe = regexp.MustCompile(`^[A-Za-z0-9_-]{20}$`)

// commOf: the session's community now (another goroutine may move it: a ban, a deleted community).
func (h *hub) commOf(ss *session) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return ss.comm
}
