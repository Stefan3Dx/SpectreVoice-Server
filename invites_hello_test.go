package main

import (
	"testing"
	"time"
)

// Refused because the server is full: the invite's use and the admission it gave are handed back, so the retry
// that gets in is the one counted.
func TestInviteNotUsedWhenFull(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.joinPass = "pw"; c.maxUsers = 1 })
	in := &invite{Comm: spaceID, By: "U", Created: time.Now().UnixMilli(), MaxUses: 1, Bypass: true}
	if err := s.store.addInvite(in); err != nil {
		t.Fatal(err)
	}
	first, _ := dialV(t, hs, newKey(t), "First", "pw")
	if first == nil {
		t.Fatal("first refused")
	}
	key := newKey(t)
	if c, e := dialVWith(t, hs, key, "Second", map[string]any{"invite": in.Code}); c != nil || e["why"] != "full" {
		t.Fatalf("not refused: %v", e)
	}
	if got, _ := s.store.invite(in.Code); got.Uses != 0 {
		t.Fatalf("a refused hello used the invite: %d", got.Uses)
	}
	uid := uidOfKey(key)
	if s.store.admitted(uid) {
		t.Fatal("admitted though refused")
	}
	_ = first.ws.Close()
	time.Sleep(300 * time.Millisecond)
	if c, e := dialVWith(t, hs, key, "Second", map[string]any{"invite": in.Code}); c == nil {
		t.Fatalf("refused with a slot free: %v", e)
	}
	if got, _ := s.store.invite(in.Code); got.Uses != 1 {
		t.Fatalf("uses %d, want 1", got.Uses)
	}
	if !s.store.admitted(uid) {
		t.Fatal("not admitted")
	}
}

// A member coming back with the invite still in the app's hello doesn't use it again.
func TestInviteNotUsedOnReconnect(t *testing.T) {
	s, hs := newTestServer(t, nil)
	in := &invite{Comm: spaceID, By: "U", Created: time.Now().UnixMilli(), MaxUses: 2}
	if err := s.store.addInvite(in); err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	c, e := dialVWith(t, hs, key, "Ana", map[string]any{"invite": in.Code})
	if c == nil {
		t.Fatal(e)
	}
	if got, _ := s.store.invite(in.Code); got.Uses != 1 {
		t.Fatalf("uses %d, want 1", got.Uses)
	}
	_ = c.ws.Close()
	time.Sleep(200 * time.Millisecond)
	for range 3 {
		c, e = dialVWith(t, hs, key, "Ana", map[string]any{"invite": in.Code})
		if c == nil {
			t.Fatal(e)
		}
		_ = c.ws.Close()
		time.Sleep(100 * time.Millisecond)
	}
	if got, _ := s.store.invite(in.Code); got.Uses != 1 {
		t.Fatalf("reconnects used the invite: %d", got.Uses)
	}
}
