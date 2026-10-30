package main

// Private calls between two people of this server (docs/CALLS.md in the apps' repository). The server rings,
// checks who may call whom, passes the signalling on by call id and writes the call into the private chat. The
// sound goes peer to peer between the two apps (DTLS-SRTP end to end, through the server's TURN when the networks
// need it), or, on a server without TURN, through a private SFU room each ("<uid>:call-<id>" on /streams) that only
// the other person of the call may take (not end to end; the apps say so).
//
//	<- {"t":"call-invite","to":U,"n":ref}         -> {"t":"call-ringing","id","to","name","route","n"} or call-end
//	-> {"t":"call-ring","id","from","name","route"} to U's sessions (and a sealed push to U's phones)
//	<- {"t":"call-accept","id"}                   -> both: {"t":"call-accepted","id","route","ice","peer","name"}
//	<- {"t":"call-decline","id","why"?} · {"t":"call-cancel","id"} · {"t":"call-end","id"}
//	-> {"t":"call-end","id","why","secs"?}
//	<- {"t":"call-sdp","id","type","sdp","ik","fsig"} · {"t":"call-ice","id","candidate","mid","mline"}  passed on
//	<- {"t":"call-rejoin","id"}                   a reconnected session carries the call again -> call-rejoined
//
// The welcome's server "calls" says the route ("p2p", "sfu"); without it (off, or an older server) the apps hide calls.
//
// HTTP: POST /v1/call/decline {"call","t"} declines from a phone's notification without signing in (t from the
// sealed push: an HMAC of the call and the callee); 10 tries an address, then one every 6 s.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	callRingDefault = 45 * time.Second
	callGrace       = 20 * time.Second // a side whose last session left has this long to come back (call-rejoin)
	callInviteBurst = 4.0
	callInviteRate  = 0.1 // invites a second a person (one every 10 s), after the burst
	callPairHour    = 20  // invites from one person to another in an hour
	callSigRate     = 30.0
	callSigBurst    = 120.0
	callMaxSDP      = 32 << 10
	callMaxSDPs     = 40 // offers and answers in one call (ICE restarts after network changes)
	callRoomPart    = ":call-"
	// /v1/call/decline (no sign-in): per address, a burst of 10 and one more every 6 s
	callDeclineBurst = 10.0
	callDeclineRate  = 1.0 / 6
)

// callBlockedHook: whether a has blocked b (the private messages' blocking sets it; nil: nobody blocks). Either
// way stops a call: the caller sees it ring and go unanswered, the other is never rung.
var callBlockedHook func(s *server, a, b string) bool

func (s *server) callBlocked(a, b string) bool {
	if callBlockedHook == nil {
		return false
	}
	return callBlockedHook(s, a, b) || callBlockedHook(s, b, a)
}

type callSide struct {
	uid, name string
	sid       int // the session carrying the call (the callee's: once it answered); 0: gone, waiting for a rejoin
	cands     int
	lost      *time.Timer
}

type call struct {
	id       string
	from, to *callSide
	route    string // "p2p" or "sfu"
	active   bool   // answered
	ghost    bool   // blocked: rings for the caller only, ends unanswered, leaves no line
	created  time.Time
	answered time.Time
	ring     *time.Timer
	sdps     int
	ended    bool
}

func (c *call) side(uid string) (me, other *callSide) {
	switch uid {
	case c.from.uid:
		return c.from, c.to
	case c.to.uid:
		return c.to, c.from
	}
	return nil, nil
}

type callBucket struct {
	tokens float64
	at     time.Time
}

type callHub struct {
	mu      sync.Mutex
	calls   map[string]*call
	byUID   map[string]*call // one call a person (ringing or active)
	invites map[string]*callBucket
	pairs   map[string][]time.Time
	sig     map[string]*callBucket // by session id
	decl    map[string]*callBucket // /v1/call/decline, by address
	ring    time.Duration
}

func newCallHub(ring time.Duration) *callHub {
	if ring <= 0 {
		ring = callRingDefault
	}
	return &callHub{calls: map[string]*call{}, byUID: map[string]*call{}, invites: map[string]*callBucket{}, pairs: map[string][]time.Time{}, sig: map[string]*callBucket{}, decl: map[string]*callBucket{}, ring: ring}
}

func takeBucket(m map[string]*callBucket, key string, burst, rate float64) bool {
	now := time.Now()
	b := m[key]
	if b == nil {
		b = &callBucket{tokens: burst, at: now}
		m[key] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// callRoute: how this server's calls carry their sound (SV_CALLS_ROUTE; auto: P2P with a TURN relay, else the SFU).
func (s *server) callRoute() string {
	switch s.cfg.callsRoute {
	case "p2p", "sfu", "off":
		return s.cfg.callsRoute
	}
	if s.cfg.turn != "" {
		return "p2p"
	}
	return "sfu"
}

func newCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

var callIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16}$`)

// appSessions: uid's sessions that can take a call (not bots, not the web client, not apps older than calls); h.mu
// held.
func (h *hub) appSessions(uid string) []*session {
	var out []*session
	for _, o := range h.sessions {
		if o.uid == uid && !o.bot && !o.api && clientCanCall(o.client) {
			out = append(out, o)
		}
	}
	return out
}

// clientCanCall: whether a client (its hello's "client", "android/0.9.0", "windows/0.9.0", "web/…") rings for a
// call. The web client has no calls, and the apps before 0.9.0 don't know call-ring: rung, they'd stay silent and
// the caller would hear it ring for the whole ring time; without them the caller is told "offline" at once.
// Anything else (tests, a dev or local build, a client unknown here) rings.
func clientCanCall(client string) bool {
	kind, ver, _ := strings.Cut(strings.ToLower(client), "/")
	switch kind {
	case "web":
		return false
	case "android", "windows", "linux", "macos":
	default:
		return true
	}
	var v [3]int
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 2 {
		return true
	}
	for i, p := range parts {
		n, digits := 0, 0
		for _, r := range p {
			if r < '0' || r > '9' || digits > 6 {
				break
			}
			n, digits = n*10+int(r-'0'), digits+1
		}
		if digits == 0 {
			return true
		}
		v[i] = n
	}
	// 0.2 … 0.8: the releases before calls. 0.1.0 is what a local build without -PappVersion calls itself (and the
	// 0.1 releases are long gone): it rings
	return !(v[0] == 0 && v[1] >= 2 && v[1] < 9)
}

func (h *hub) sendTo(sid int, m map[string]any) {
	if sid == 0 {
		return
	}
	h.mu.Lock()
	o := h.sessions[sid]
	h.mu.Unlock()
	if o != nil {
		o.send(m)
	}
}

func (h *hub) sendUID(uid string, m map[string]any, except int) {
	h.mu.Lock()
	ss := h.appSessions(uid)
	h.mu.Unlock()
	for _, o := range ss {
		if o.sid != except {
			o.send(m)
		}
	}
}

func (h *hub) callOp(ss *session, t string, m map[string]any) {
	ch := h.s.calls
	if ss.bot || ss.api {
		ss.refuse(t, "bot", "not for bots")
		return
	}
	if h.s.callRoute() == "off" {
		ss.refuse(t, "off", "calls are off on this server")
		return
	}
	if t == "call-invite" {
		h.callInvite(ss, str(m, "to"), clip(str(m, "n"), 64))
		return
	}
	id := str(m, "id")
	ch.mu.Lock()
	ok := takeBucket(ch.sig, fmt.Sprint(ss.sid), callSigBurst, callSigRate)
	c := ch.calls[id]
	ch.mu.Unlock()
	if !ok {
		ss.refuse(t, "rate", "slow down")
		return
	}
	if c == nil {
		if t != "call-ice" { // a candidate after the end is nothing to say
			ss.send(map[string]any{"t": "call-end", "id": id, "why": "gone"})
		}
		return
	}
	switch t {
	case "call-accept":
		h.callAccept(ss, c)
	case "call-decline":
		why := "declined"
		if str(m, "why") == "busy" {
			why = "busy"
		}
		h.callDecline(ss.uid, c, why)
	case "call-cancel":
		if ss.uid == c.from.uid && !c.active {
			h.callFinish(c, "cancelled")
		}
	case "call-end":
		switch {
		case c.active && (ss.uid == c.from.uid || ss.uid == c.to.uid):
			h.callFinish(c, "hangup")
		case ss.uid == c.from.uid:
			h.callFinish(c, "cancelled")
		case ss.uid == c.to.uid:
			h.callDecline(ss.uid, c, "declined")
		}
	case "call-rejoin":
		h.callRejoin(ss, c)
	case "call-sdp", "call-ice":
		h.callPass(ss, t, c, m)
	}
}

func (h *hub) callInvite(ss *session, to, n string) {
	ch := h.s.calls
	reply := func(why string) {
		out := map[string]any{"t": "call-end", "id": "", "why": why, "to": to}
		if n != "" {
			out["n"] = n
		}
		ss.send(out)
	}
	if !uidRe.MatchString(to) || to == ss.uid {
		ss.refuse("call-invite", "bad", "bad person")
		return
	}
	if !h.s.store.isMember(spaceID, to) {
		ss.refuse("call-invite", "bad", "they aren't on this server")
		return
	}
	if h.s.mods.banned(ss.uid, spaceID) {
		ss.refuse("call-invite", "role", "you can't call from here")
		return
	}
	now := time.Now()
	ch.mu.Lock()
	if ch.byUID[ss.uid] != nil {
		ch.mu.Unlock()
		ss.refuse("call-invite", "busy", "you're already in a call")
		return
	}
	pair := ss.uid + ">" + to
	recent := ch.pairs[pair][:0]
	for _, at := range ch.pairs[pair] {
		if now.Sub(at) < time.Hour {
			recent = append(recent, at)
		}
	}
	ch.pairs[pair] = recent
	if len(recent) >= callPairHour || !takeBucket(ch.invites, ss.uid, callInviteBurst, callInviteRate) {
		ch.mu.Unlock()
		ss.refuse("call-invite", "rate", "too many calls, wait a little")
		return
	}
	ch.pairs[pair] = append(recent, now)
	ch.mu.Unlock()

	name := h.s.store.userName(to)
	route := h.s.callRoute()
	c := &call{id: newCallID(), from: &callSide{uid: ss.uid, name: ss.name, sid: ss.sid}, to: &callSide{uid: to, name: name},
		route: route, created: now}
	ringing := map[string]any{"t": "call-ringing", "id": c.id, "to": to, "name": name, "route": route}
	if n != "" {
		ringing["n"] = n
	}
	if h.s.callBlocked(ss.uid, to) {
		// Rings for the caller, unanswered; the other is never told and no line is written
		c.ghost = true
		ch.mu.Lock()
		ch.calls[c.id], ch.byUID[ss.uid] = c, c
		c.ring = time.AfterFunc(ch.ring, func() { h.callFinish(c, "timeout") })
		ch.mu.Unlock()
		ss.send(ringing)
		return
	}
	h.mu.Lock()
	sessions := h.appSessions(to)
	h.mu.Unlock()
	devices := h.s.callDevices(to)
	ch.mu.Lock()
	busy := ch.byUID[to] != nil
	ch.mu.Unlock()
	switch {
	case busy:
		reply("busy")
		h.callLine(c, "missed", 0)
		return
	case len(sessions) == 0 && len(devices) == 0:
		reply("offline")
		h.callLine(c, "missed", 0)
		return
	}
	ch.mu.Lock()
	if ch.byUID[to] != nil || ch.byUID[ss.uid] != nil { // someone else was quicker
		ch.mu.Unlock()
		reply("busy")
		return
	}
	ch.calls[c.id], ch.byUID[ss.uid], ch.byUID[to] = c, c, c
	c.ring = time.AfterFunc(ch.ring, func() { h.callFinish(c, "timeout") })
	ch.mu.Unlock()
	ss.send(ringing)
	ring := map[string]any{"t": "call-ring", "id": c.id, "from": ss.uid, "name": ss.name, "route": route}
	for _, o := range sessions {
		o.send(ring)
	}
	h.s.callPush(c, devices, "ring", "")
	log.Printf("call %s: %s → %s (%s)", c.id, ss.uid, to, route)
}

// callsWelcome rings a session that just came in for a call that's ringing for it.
func (h *hub) callsWelcome(ss *session) {
	if h.s.calls == nil || ss.bot || ss.api {
		return
	}
	ch := h.s.calls
	ch.mu.Lock()
	c := ch.byUID[ss.uid]
	var ring map[string]any
	if c != nil && !c.active && !c.ghost && c.to.uid == ss.uid {
		ring = map[string]any{"t": "call-ring", "id": c.id, "from": c.from.uid, "name": c.from.name, "route": c.route}
	}
	ch.mu.Unlock()
	if ring != nil {
		ss.send(ring)
	}
}

func (h *hub) callAccept(ss *session, c *call) {
	ch := h.s.calls
	ch.mu.Lock()
	if c.ended || c.active || c.ghost || ss.uid != c.to.uid {
		ch.mu.Unlock()
		if ss.uid == c.to.uid {
			ss.send(map[string]any{"t": "call-end", "id": c.id, "why": "gone"})
		}
		return
	}
	c.active, c.answered = true, time.Now()
	c.to.sid = ss.sid
	if c.ring != nil {
		c.ring.Stop()
	}
	from := c.from.sid
	ch.mu.Unlock()
	h.sendUID(c.to.uid, map[string]any{"t": "call-end", "id": c.id, "why": "elsewhere"}, ss.sid)
	h.s.callPush(c, h.s.callDevices(c.to.uid), "end", "elsewhere")
	ss.send(map[string]any{"t": "call-accepted", "id": c.id, "route": c.route, "ice": h.s.publicICE(c.to.uid), "peer": c.from.uid, "name": c.from.name})
	h.sendTo(from, map[string]any{"t": "call-accepted", "id": c.id, "route": c.route, "ice": h.s.publicICE(c.from.uid), "peer": c.to.uid, "name": c.to.name})
	log.Printf("call %s: answered", c.id)
}

func (h *hub) callDecline(uid string, c *call, why string) {
	h.s.calls.mu.Lock()
	ok := !c.ended && !c.active && !c.ghost && uid == c.to.uid
	h.s.calls.mu.Unlock()
	if ok {
		h.callFinish(c, why)
	}
}

func (h *hub) callRejoin(ss *session, c *call) {
	ch := h.s.calls
	ch.mu.Lock()
	me, _ := c.side(ss.uid)
	if me == nil || c.ended || (!c.active && me == c.to) {
		ch.mu.Unlock()
		ss.send(map[string]any{"t": "call-end", "id": c.id, "why": "gone"})
		return
	}
	me.sid = ss.sid
	if me.lost != nil {
		me.lost.Stop()
		me.lost = nil
	}
	ch.mu.Unlock()
	ss.send(map[string]any{"t": "call-rejoined", "id": c.id, "active": c.active})
}

func (h *hub) callPass(ss *session, t string, c *call, m map[string]any) {
	ch := h.s.calls
	out := map[string]any{"t": t, "id": c.id}
	switch t {
	case "call-sdp":
		sdp, typ := str(m, "sdp"), str(m, "type")
		if sdp == "" || len(sdp) > callMaxSDP || (typ != "offer" && typ != "answer") {
			ss.refuse(t, "bad", "bad description")
			return
		}
		out["sdp"], out["type"] = sdp, typ
		// The identity key and its signature over the DTLS fingerprint: checked by the other app, not here
		if ik := str(m, "ik"); ik != "" && len(ik) <= 400 {
			out["ik"] = ik
		}
		if fs := str(m, "fsig"); fs != "" && len(fs) <= 400 {
			out["fsig"] = fs
		}
	case "call-ice":
		cand := str(m, "candidate")
		if cand == "" || len(cand) > maxCandLen {
			return
		}
		out["candidate"] = cand
		if mid := str(m, "mid"); mid != "" && len(mid) <= 32 {
			out["mid"] = mid
		}
		if _, ok := m["mline"]; ok {
			out["mline"] = min(max(i64(m, "mline"), 0), 64)
		}
	}
	ch.mu.Lock()
	me, other := c.side(ss.uid)
	if me == nil || !c.active || c.ended || me.sid != ss.sid {
		ch.mu.Unlock()
		return
	}
	if t == "call-sdp" {
		if c.sdps++; c.sdps > callMaxSDPs {
			ch.mu.Unlock()
			return
		}
	} else if me.cands++; me.cands > 2*maxCands {
		ch.mu.Unlock()
		return
	}
	to := other.sid
	ch.mu.Unlock()
	h.sendTo(to, out)
}

// callsGone: ss left; a call it carried waits for a rejoin a while, then ends.
func (h *hub) callsGone(ss *session) {
	ch := h.s.calls
	if ch == nil {
		return
	}
	ch.mu.Lock()
	delete(ch.sig, fmt.Sprint(ss.sid))
	for _, c := range ch.calls {
		for _, sd := range []*callSide{c.from, c.to} {
			if sd.sid == ss.sid {
				sd.sid = 0
				cc, side := c, sd
				side.lost = time.AfterFunc(callGrace, func() {
					ch.mu.Lock()
					still := side.sid == 0 && !cc.ended
					ch.mu.Unlock()
					if still {
						why := "lost"
						if !cc.active {
							why = "cancelled"
						}
						h.callFinish(cc, why)
					}
				})
			}
		}
	}
	ch.mu.Unlock()
}

// callFinish ends c, tells both sides (and the callee's phones while it rang) and writes the call's line.
func (h *hub) callFinish(c *call, why string) {
	ch := h.s.calls
	ch.mu.Lock()
	if c.ended {
		ch.mu.Unlock()
		return
	}
	c.ended = true
	delete(ch.calls, c.id)
	for _, u := range []string{c.from.uid, c.to.uid} {
		if ch.byUID[u] == c {
			delete(ch.byUID, u)
		}
	}
	for _, sd := range []*callSide{c.from, c.to} {
		if sd.lost != nil {
			sd.lost.Stop()
		}
	}
	if c.ring != nil {
		c.ring.Stop()
	}
	active, ghost := c.active, c.ghost
	fromSid, toSid := c.from.sid, c.to.sid
	ch.mu.Unlock()
	secs := 0
	if active {
		secs = int(time.Since(c.answered).Seconds())
	}
	end := map[string]any{"t": "call-end", "id": c.id, "why": why}
	if active {
		end["secs"] = secs
	}
	h.sendTo(fromSid, end)
	switch {
	case ghost:
	case active:
		h.sendTo(toSid, end)
	default:
		h.sendUID(c.to.uid, end, 0)
		missedWhy := why
		if why == "cancelled" || why == "timeout" {
			missedWhy = "missed"
		}
		h.s.callPush(c, h.s.callDevices(c.to.uid), "end", missedWhy)
	}
	if c.route == "sfu" {
		h.s.endCallRooms(c)
	}
	if !ghost {
		switch {
		case active:
			h.callLine(c, "done", secs)
		case why == "declined":
			h.callLine(c, "declined", 0)
		default:
			h.callLine(c, "missed", 0)
		}
	}
	log.Printf("call %s: ended (%s, %d s)", c.id, why, secs)
}

// callLine writes the call into the two people's private chat: a line of the server's own (sys), with a short
// English text for the apps that don't know it.
func (h *hub) callLine(c *call, kind string, secs int) {
	text := map[string]string{"missed": "📞 Missed call", "declined": "📞 Call declined"}[kind]
	if kind == "done" {
		text = "📞 Call · " + callLength(secs)
	}
	sys, _ := json.Marshal(map[string]any{"call": c.id, "kind": kind, "secs": secs, "from": c.from.uid})
	msg := &chatMsg{To: dmConv(c.from.uid, c.to.uid), From: c.from.uid, Name: c.from.name, Text: text, At: time.Now().UnixMilli(), Sys: string(sys)}
	if err := h.s.store.addMessage(msg); err != nil {
		log.Printf("call line: %v", err)
		return
	}
	h.mu.Lock()
	h.deliver(map[string]any{"t": "msg", "m": msg}, 0, c.from.uid, c.to.uid)
	h.mu.Unlock()
}

// callLength: "45 s", "12 min", "1 h 5 min".
func callLength(secs int) string {
	switch {
	case secs < 60:
		return fmt.Sprintf("%d s", secs)
	case secs < 3600:
		return fmt.Sprintf("%d min", secs/60)
	}
	return fmt.Sprintf("%d h %d min", secs/3600, secs%3600/60)
}

// ---- Call rooms (the SFU route) ----

// callRoomOf: the room uid publishes its microphone to in call id.
func callRoomOf(uid, id string) string { return uid + callRoomPart + id }

func isCallRoom(room string) bool { return strings.Contains(room, callRoomPart) }

// callRoomAllowed: uid may publish (its own) or take (the other's) room, in an answered call through the SFU.
func (s *server) callRoomAllowed(uid, room string, publish bool) bool {
	if s.calls == nil {
		return false
	}
	owner, id, ok := strings.Cut(room, callRoomPart)
	if !ok {
		return false
	}
	s.calls.mu.Lock()
	defer s.calls.mu.Unlock()
	c := s.calls.calls[id]
	if c == nil || !c.active || c.ended || c.route != "sfu" {
		return false
	}
	me, other := c.side(uid)
	if me == nil {
		return false
	}
	if publish {
		return owner == uid
	}
	return owner == other.uid
}

func (s *server) endCallRooms(c *call) {
	for _, name := range []string{callRoomOf(c.from.uid, c.id), callRoomOf(c.to.uid, c.id)} {
		s.mu.Lock()
		rm := s.rooms[name]
		s.mu.Unlock()
		if rm != nil {
			go s.endRoom(rm, nil)
		}
	}
}

// ---- Pushes (sealed for the phone) ----

type callDevice struct{ token, key, addr string }

// callDevices: uid's phones that can take a call push (they gave a key: an app that knows calls).
func (s *server) callDevices(uid string) []callDevice {
	if s.push == nil {
		return nil
	}
	rows, err := s.store.db.Query(`SELECT token, pkey, addr FROM push_tokens WHERE uid=? AND pkey != ''`, uid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []callDevice
	for rows.Next() {
		var d callDevice
		if rows.Scan(&d.token, &d.key, &d.addr) == nil {
			out = append(out, d)
		}
	}
	return out
}

var pushAddrRe = regexp.MustCompile(`^[A-Za-z0-9.:\[\]/_-]{1,200}$`)

// pushKeyOf keeps a push token's own key and address (push-token "key", "addr"), when the app gives them.
func (s *server) pushKeyOf(tok, uid string, m map[string]any) {
	k, err := base64.StdEncoding.DecodeString(str(m, "key"))
	if err != nil || len(k) != 32 {
		return
	}
	addr := str(m, "addr")
	if !pushAddrRe.MatchString(addr) {
		addr = ""
	}
	_, _ = s.store.db.Exec(`UPDATE push_tokens SET pkey=?, addr=? WHERE token=? AND uid=?`, base64.StdEncoding.EncodeToString(k), addr, tok, uid)
}

// declineToken: what lets a phone decline call id for uid from its notification.
func (s *server) declineToken(id, uid string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte("sv-call-decline\n" + id + "\n" + uid))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// sealPush encrypts v for one phone: AES-256-GCM under its key, AAD "sv-push-v1", base64(iv ‖ ciphertext).
func sealPush(keyB64 string, v any) (string, error) {
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		return "", fmt.Errorf("bad key")
	}
	plain, _ := json.Marshal(v)
	blk, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(blk)
	iv := make([]byte, 12)
	_, _ = rand.Read(iv)
	return base64.StdEncoding.EncodeToString(append(iv, g.Seal(nil, iv, plain, []byte("sv-push-v1"))...)), nil
}

// callPush tells uid's phones: "ring" (who calls, where, how to decline) or "end" (stop ringing; why "missed"
// shows a missed call). Only for a short while: a ring delivered late mustn't ring.
func (s *server) callPush(c *call, devs []callDevice, kind, why string) {
	if s.push == nil || len(devs) == 0 {
		return
	}
	host := ""
	if u, err := url.Parse(s.cfg.inviteBase); err == nil {
		host = u.Host
	}
	ttl := fmt.Sprintf("%ds", int(s.calls.ring.Seconds()))
	pk := "call"
	if kind == "end" {
		ttl, pk = "60s", "call-end"
	}
	for _, d := range devs {
		body := map[string]any{"call": c.id, "from": c.from.uid, "name": c.from.name, "ts": time.Now().UnixMilli(), "kind": kind}
		if kind == "ring" {
			body["addr"], body["srv"], body["dt"], body["route"] = d.addr, host, s.declineToken(c.id, c.to.uid), c.route
		} else {
			body["why"] = why
		}
		e, err := sealPush(d.key, body)
		if err != nil {
			continue
		}
		s.push.notifyOne(c.to.uid, d.token, map[string]string{"kind": pk, "e": e}, ttl)
	}
}

// handleCallDecline: POST /v1/call/decline {"call","t"}: a phone says no from its notification.
func (s *server) handleCallDecline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	// No sign-in here: a budget per address, so call ids and tokens can't be tried by the thousand
	s.calls.mu.Lock()
	ok := takeBucket(s.calls.decl, requestIP(r), callDeclineBurst, callDeclineRate)
	if len(s.calls.decl) > 10000 {
		for k, b := range s.calls.decl {
			if time.Since(b.at) > 10*time.Minute {
				delete(s.calls.decl, k)
			}
		}
	}
	s.calls.mu.Unlock()
	if !ok {
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	var q struct {
		Call string `json:"call"`
		T    string `json:"t"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil || !callIDRe.MatchString(q.Call) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.calls.mu.Lock()
	c := s.calls.calls[q.Call]
	s.calls.mu.Unlock()
	if c == nil || subtle.ConstantTimeCompare([]byte(q.T), []byte(s.declineToken(c.id, c.to.uid))) != 1 {
		http.Error(w, "no such call", http.StatusNotFound)
		return
	}
	s.hub.callDecline(c.to.uid, c, "declined")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}
