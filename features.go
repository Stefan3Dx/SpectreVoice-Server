package main

// Smaller things of a voice server:
//
//	<- {"t":"ch-kick","sid":…,"why":…}          (kick) out of their channel, to the community's default one
//	<- {"t":"talker","sid":…,"on":bool}         (mute_voice) may talk in a moderated channel (until they leave it)
//	<- {"t":"voice-mute","uid":…,"minutes":…}   (mute_voice) roles.go: nobody hears them in the community
//	<- {"t":"state","awayMsg":…,"rtt":…,"loss":…}  hub.go: an away message, the app's connection quality
//	"g:<community>" as a chat's "to": the community's own chat (everyone in it), hub.go

import (
	"crypto/rand"
	"encoding/base32"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// claimCode: on a server nobody owns yet, a code (in <data>/claim.code and the log) that makes whoever types it in
// the app the owner, once. install.sh prints it.
var claimMu sync.Mutex

func (s *server) claimCode() string {
	var n int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='owner'`).Scan(&n)
	f := filepath.Join(s.cfg.data, "claim.code")
	if n > 0 {
		_ = os.Remove(f)
		return ""
	}
	if b, err := os.ReadFile(f); err == nil && len(strings.TrimSpace(string(b))) >= 8 {
		return strings.TrimSpace(string(b))
	}
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	_ = writeFileAtomic(f, []byte(code+"\n"), 0o600)
	return code
}

// maySpeak: is ss heard at all (not silenced, allowed to talk, in a moderated channel only when let); h.mu held.
func (h *hub) maySpeak(ss *session) bool {
	if ss.bot {
		return true
	}
	if ss.vmuted || ss.comm == 0 || !h.may(ss, pSpeak) {
		return false
	}
	if c := h.chans[ss.ch]; c != nil && c.Moderated {
		return ss.talker || h.may(ss, pMuteVoice)
	}
	return true
}

func (h *hub) featureOp(ss *session, t string, m map[string]any) bool {
	switch t {
	case "ch-kick":
		h.mu.Lock()
		o := h.sessions[int(i64(m, "sid"))]
		if o == nil || o.comm != ss.comm || !h.may(ss, pKick) || !h.outranks(ss, o.uid) {
			h.mu.Unlock()
			ss.refuse(t, "role", "not allowed")
			return true
		}
		def := h.defaultChannel(o.comm)
		old := o.ch
		if old == def {
			h.mu.Unlock()
			return true
		}
		o.ch, o.talk, o.talker = def, false, false
		h.memberChanged(o)
		h.mu.Unlock()
		o.send(map[string]any{"t": "moved", "ch": def, "by": ss.name, "kick": true, "why": clip(str(m, "why"), 200)})
		log.Printf("ch-kick: %s by %s", o.uid, ss.uid)
		h.s.voice.sync(old, def)
	case "talker":
		on, _ := boolOf(m, "on")
		h.mu.Lock()
		o := h.sessions[int(i64(m, "sid"))]
		if o == nil || o.comm != ss.comm || !h.may(ss, pMuteVoice) {
			h.mu.Unlock()
			ss.refuse(t, "role", "not allowed")
			return true
		}
		o.talker = on
		h.memberChanged(o)
		ch := o.ch
		h.mu.Unlock()
		h.s.voice.sync(ch)
	case "claim":
		// One claim at a time: two right codes at once can't make two owners
		claimMu.Lock()
		defer claimMu.Unlock()
		code := h.s.claimCode()
		if code == "" || !constEq(strings.ToUpper(strings.TrimSpace(str(m, "code"))), code) {
			ss.refuse(t, "bad", "wrong code, or the server has an owner")
			return true
		}
		if err := h.s.store.setRole(ss.uid, roleOwner); err != nil {
			ss.refuse(t, "server", err.Error())
			return true
		}
		_ = os.Remove(filepath.Join(h.s.cfg.data, "claim.code"))
		log.Printf("owner: %s claimed the server", ss.uid)
		h.mu.Lock()
		h.refreshRoles(ss.comm)
		h.mu.Unlock()
		ss.send(map[string]any{"t": "done", "op": "claim"})
	default:
		return false
	}
	return true
}
