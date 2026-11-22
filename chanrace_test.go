package main

import (
	"fmt"
	"sync"
	"testing"
)

// chanAdmins: two admins' sessions on s, for ch-save and ch-del called side by side.
func chanAdmins(t *testing.T) (*server, *session, *session) {
	s, hs := newTestServer(t, nil)
	a, _ := dialV(t, hs, newKey(t), "Ana", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	_ = s.store.setRole(a.uid, roleOwner)
	_ = s.store.setRole(b.uid, roleAdmin)
	h := s.hub
	h.mu.Lock()
	h.refreshRoles(1)
	sa, sb := h.sessions[a.sid], h.sessions[b.sid]
	h.mu.Unlock()
	if sa == nil || sb == nil {
		t.Fatal("no sessions")
	}
	return s, sa, sb
}

// chanNamed: the id of the channel called name (0: none).
func chanNamed(h *hub, name string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, c := range h.chans {
		if c.Name == name {
			return id
		}
	}
	return 0
}

// A ch-save (slowed by a password's hashing) and a ch-del at once: the channel is either in both memory and the
// database, or in neither; never brought back in memory only.
func TestChannelSaveDeleteRace(t *testing.T) {
	s, sa, sb := chanAdmins(t)
	h := s.hub
	n := 8
	if raceOn {
		n = 3
	}
	for i := range n {
		name := fmt.Sprintf("Race%d", i)
		h.saveChannel(sa, map[string]any{"name": name})
		id := chanNamed(h, name)
		if id == 0 {
			t.Fatal("not made")
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			h.saveChannel(sa, map[string]any{"id": float64(id), "name": name, "password": "pw"})
		}()
		go func() { defer wg.Done(); h.deleteChannel(sb, id) }()
		wg.Wait()
		h.mu.Lock()
		_, inMem := h.chans[id]
		h.mu.Unlock()
		var rows int
		_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM channels WHERE id=?`, id).Scan(&rows)
		if inMem != (rows == 1) {
			t.Fatalf("round %d: in memory %v, rows %d", i, inMem, rows)
		}
	}
}

// Two ch-saves at once: what's kept here is what the database has.
func TestChannelSaveSaveRace(t *testing.T) {
	s, sa, sb := chanAdmins(t)
	h := s.hub
	h.saveChannel(sa, map[string]any{"name": "Both"})
	id := chanNamed(h, "Both")
	if id == 0 {
		t.Fatal("not made")
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		h.saveChannel(sa, map[string]any{"id": float64(id), "name": "Both", "password": "pw"})
	}()
	go func() { defer wg.Done(); h.saveChannel(sb, map[string]any{"id": float64(id), "name": "Renamed"}) }()
	wg.Wait()
	var name, pass string
	if err := s.store.db.QueryRow(`SELECT name, pass FROM channels WHERE id=?`, id).Scan(&name, &pass); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	c := *h.chans[id]
	h.mu.Unlock()
	if c.Name != name || c.pass != pass || c.HasPass != (pass != "") {
		t.Fatalf("memory %q pass %v, database %q pass %v", c.Name, c.pass != "", name, pass != "")
	}
}
