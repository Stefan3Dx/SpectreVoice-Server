package main

import (
	"crypto/x509"
	"testing"
	"time"
)

func chOf(s *server, sid int) int64 {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if o := s.hub.sessions[sid]; o != nil {
		return o.ch
	}
	return -1
}

func isMoved(m map[string]any) bool { return m["t"] == "moved" }

// Banned from a channel: out to the default channel, or nowhere when that's the channel (or banned from it too);
// a reconnect or a temporary channel's end doesn't put them back in it.
func TestChannelBanLanding(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	ana, _ := dialV(t, hs, ownerKey, "Ana", "")
	eveKey := newKey(t)
	eve, _ := dialV(t, hs, eveKey, "Eve", "")
	cara, _ := dialV(t, hs, newKey(t), "Cara", "")
	lobby, afk := chanID(t, ana.wel, "Lobby"), chanID(t, ana.wel, "AFK")
	if chOf(s, eve.sid) != lobby {
		t.Fatal("not in the lobby")
	}

	// (c) banned from another channel, not the default: to the default, as before
	cara.send(map[string]any{"t": "join", "ch": afk})
	memberIn(ana, cara.uid, afk)
	ana.send(map[string]any{"t": "ch-ban", "uid": cara.uid, "ch": afk})
	ana.wait(isDone("ch-ban"))
	if mv := cara.wait(isMoved); int64(mv["ch"].(float64)) != lobby || chOf(s, cara.sid) != lobby {
		t.Fatal(mv)
	}

	// (a) banned from the default channel while in it: nowhere, and not let back
	ana.send(map[string]any{"t": "ch-ban", "uid": eve.uid, "ch": lobby})
	ana.wait(isDone("ch-ban"))
	if mv := eve.wait(isMoved); mv["ch"].(float64) != 0 || chOf(s, eve.sid) != 0 {
		t.Fatal(mv, chOf(s, eve.sid))
	}
	eve.send(map[string]any{"t": "join", "ch": lobby})
	if e := eve.wait(isErrOp("join")); e["why"] != "banned" {
		t.Fatal(e)
	}

	// (b) banned from another channel, and from the default too: nowhere
	eve.send(map[string]any{"t": "join", "ch": afk})
	memberIn(ana, eve.uid, afk)
	ana.send(map[string]any{"t": "ch-ban", "uid": eve.uid, "ch": afk})
	ana.wait(isDone("ch-ban"))
	if mv := eve.wait(isMoved); mv["ch"].(float64) != 0 || chOf(s, eve.sid) != 0 {
		t.Fatal(mv)
	}
	ana.send(map[string]any{"t": "ch-unban", "uid": eve.uid, "ch": afk})
	ana.wait(isDone("ch-unban"))

	// (d) a reconnect: not into the default channel
	_ = eve.ws.Close()
	time.Sleep(300 * time.Millisecond)
	eve, _ = dialV(t, hs, eveKey, "Eve", "")
	if eve == nil || chOf(s, eve.sid) != 0 {
		t.Fatal("back in the default channel")
	}

	// (e) a temporary channel ends: nowhere for her, the default for the others
	eve.send(map[string]any{"t": "temp-create", "name": "Eve's"})
	id := int64(eve.wait(isDone("temp-create"))["id"].(float64))
	cara.send(map[string]any{"t": "join", "ch": id})
	memberIn(ana, cara.uid, id)
	if chOf(s, eve.sid) != id {
		t.Fatal("not in her channel")
	}
	s.hub.tempDelete(id, "test")
	if chOf(s, eve.sid) != 0 || chOf(s, cara.sid) != lobby {
		t.Fatal(chOf(s, eve.sid), chOf(s, cara.sid))
	}
}
