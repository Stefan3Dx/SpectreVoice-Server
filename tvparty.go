package main

// The TV's watch party: those watching the server's TV (viewers of the room tv:main) send reactions (emoji that fly
// over the picture) and short lines shown over it, to everyone watching. Who watches is in the TV's state
// ("watching": their uids, the first 50, and "watchers": how many), so everyone sees the row of faces.
//
//	<- {"t":"tv","op":"react","e":EMOJI}       (watching) -> {"t":"tv-party","k":"react","e","uid","name","at"} to the viewers
//	<- {"t":"tv","op":"say","text":T}          (watching, not muted in chat) -> {"t":"tv-party","k":"say","text","uid","name","at"}
//	-> {"t":"error","op":"tv","what":"react"|"say","why":"watch"|"rate"|"mute"|"bad"}
//
// Limits: a person 5 reactions a second (bursts of 10) and a line every 1.5 s (bursts of 3); the room passes on at
// most 30 reactions a second in all (more are dropped quietly); lines ≤ 200 characters.

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	tvSayMax      = 200
	tvWatchersMax = 50
)

// Vars so tests can change them.
var (
	tvReactRate, tvReactBurst = 5.0, 10.0
	tvSayRate, tvSayBurst     = 1 / 1.5, 3.0
	tvRoomReactsPerSec        = 30
)

type tvBucket struct {
	tokens float64
	at     time.Time
}

func (b *tvBucket) take(rate, burst float64, now time.Time) bool {
	if b.at.IsZero() {
		b.tokens = burst
	} else {
		b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type tvParty struct {
	mu     sync.Mutex
	react  map[int]*tvBucket // per session
	say    map[int]*tvBucket
	second time.Time // the room's reactions this second
	count  int
}

func newTVParty() *tvParty { return &tvParty{react: map[int]*tvBucket{}, say: map[int]*tvBucket{}} }

func (p *tvParty) allow(sid int, say bool, now time.Time) (ok, room bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, rate, burst := p.react, tvReactRate, tvReactBurst
	if say {
		m, rate, burst = p.say, tvSayRate, tvSayBurst
	}
	b := m[sid]
	if b == nil {
		if len(m) > 4096 {
			for k, v := range m {
				if now.Sub(v.at) > time.Minute {
					delete(m, k)
				}
			}
		}
		b = &tvBucket{}
		m[sid] = b
	}
	if !b.take(rate, burst, now) {
		return false, true
	}
	if say {
		return true, true
	}
	if now.Sub(p.second) >= time.Second {
		p.second, p.count = now, 0
	}
	p.count++
	return true, p.count <= tvRoomReactsPerSec
}

// watchers: who watches the TV now (distinct uids, sorted), the first [tvWatchersMax], and how many in all.
func (tm *tvManager) watchers() ([]string, int) {
	s := tm.s
	s.mu.Lock()
	rm := s.rooms[tvRoom]
	s.mu.Unlock()
	if rm == nil {
		return []string{}, 0
	}
	seen := map[string]bool{}
	rm.mu.Lock()
	for sub := range rm.subs {
		if sub.uid != "" {
			seen[sub.uid] = true
		}
	}
	rm.mu.Unlock()
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	n := len(out)
	if n > tvWatchersMax {
		out = out[:tvWatchersMax]
	}
	return out, n
}

func (tm *tvManager) isWatching(uid string) bool {
	s := tm.s
	s.mu.Lock()
	rm := s.rooms[tvRoom]
	s.mu.Unlock()
	if rm == nil {
		return false
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	for sub := range rm.subs {
		if sub.uid == uid {
			return true
		}
	}
	return false
}

// tvEmoji: a reaction is one emoji (or "GG"): no other letters, digits or spaces, at most 8 runes and 32 bytes.
func tvEmoji(e string) bool {
	if e == "GG" {
		return true
	}
	if e == "" || len(e) > 32 || utf8.RuneCountInString(e) > 8 || !utf8.ValidString(e) {
		return false
	}
	for _, r := range e {
		if r < 0x80 || unicode.IsControl(r) || unicode.IsSpace(r) || unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// tvPartyOp: a reaction or a line from a viewer; false when op isn't one of them.
func (h *hub) tvPartyOp(ss *session, op string, m map[string]any) bool {
	if op != "react" && op != "say" {
		return false
	}
	tm := h.s.tv
	refuse := func(why, msg string) {
		ss.send(map[string]any{"t": "error", "op": "tv", "what": op, "why": why, "message": msg})
	}
	if !tm.enabled() {
		refuse("off", tvWhyText["off"])
		return true
	}
	out := map[string]any{"t": "tv-party", "k": op, "uid": ss.uid, "name": ss.name, "at": time.Now().UnixMilli()}
	if op == "react" {
		e := str(m, "e")
		if !tvEmoji(e) {
			refuse("bad", "not a reaction")
			return true
		}
		out["e"] = e
	} else {
		text := strings.Join(strings.FieldsFunc(str(m, "text"), func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
		if text == "" {
			refuse("bad", "say something")
			return true
		}
		if r := []rune(text); len(r) > tvSayMax {
			text = string(r[:tvSayMax])
		}
		if h.s.mods.muted(ss.uid, ss.comm) || h.s.mods.muted(ss.uid, 0) {
			refuse("mute", "you are muted in chat")
			return true
		}
		out["text"] = text
	}
	if !tm.isWatching(ss.uid) {
		refuse("watch", "watch the TV first")
		return true
	}
	ok, room := tm.party.allow(ss.sid, op == "say", time.Now())
	if !ok {
		refuse("rate", "slow down")
		return true
	}
	if !room {
		return true // the room is busy: dropped quietly
	}
	watching := map[string]bool{}
	for _, u := range tm.allWatchers() {
		watching[u] = true
	}
	h.mu.Lock()
	h.broadcast(out, func(o *session) bool { return o.comm == spaceID && !o.api && watching[o.uid] })
	h.mu.Unlock()
	return true
}

// allWatchers: every uid watching (the party's lines go to all of them, past the 50 shown).
func (tm *tvManager) allWatchers() []string {
	s := tm.s
	s.mu.Lock()
	rm := s.rooms[tvRoom]
	s.mu.Unlock()
	if rm == nil {
		return nil
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	out := make([]string, 0, len(rm.subs))
	for sub := range rm.subs {
		out = append(out, sub.uid)
	}
	return out
}
