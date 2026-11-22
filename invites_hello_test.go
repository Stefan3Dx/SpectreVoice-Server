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

// Refused because the server is full: a newcomer isn't left a member (they'd pass member checks and their next
// hello wouldn't count the invite), while a member who is refused stays one.
func TestRefusedHelloNotMember(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.maxUsers = 1 })
	in := &invite{Comm: spaceID, By: "U", Created: time.Now().UnixMilli(), MaxUses: 1}
	if err := s.store.addInvite(in); err != nil {
		t.Fatal(err)
	}
	keyC := newKey(t)
	c, e := dialVWith(t, hs, keyC, "Cara", nil)
	if c == nil {
		t.Fatal(e)
	}
	_ = c.ws.Close()
	time.Sleep(300 * time.Millisecond)
	first, _ := dialV(t, hs, newKey(t), "First", "")
	if first == nil {
		t.Fatal("first refused")
	}
	key := newKey(t)
	uid := uidOfKey(key)
	if c, e := dialVWith(t, hs, key, "Second", map[string]any{"invite": in.Code}); c != nil || e["why"] != "full" {
		t.Fatalf("not refused: %v", e)
	}
	if s.store.isMember(spaceID, uid) {
		t.Fatal("a refused newcomer was made a member")
	}
	if got, _ := s.store.invite(in.Code); got.Uses != 0 {
		t.Fatalf("a refused hello used the invite: %d", got.Uses)
	}
	if c, e := dialVWith(t, hs, keyC, "Cara", nil); c != nil || e["why"] != "full" {
		t.Fatalf("member not refused: %v", e)
	}
	if !s.store.isMember(spaceID, uidOfKey(keyC)) {
		t.Fatal("a refused member lost the membership")
	}
	_ = first.ws.Close()
	time.Sleep(300 * time.Millisecond)
	if c, e := dialVWith(t, hs, key, "Second", map[string]any{"invite": in.Code}); c == nil {
		t.Fatalf("refused with a slot free: %v", e)
	}
	if got, _ := s.store.invite(in.Code); got.Uses != 1 {
		t.Fatalf("uses %d, want 1", got.Uses)
	}
}

// An invite opened by a member already on the server takes them to its channel without using it up.
func TestInviteUseByMemberNotCounted(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	afk := chanID(t, ana.wel, "AFK")
	in := &invite{Comm: spaceID, By: "U", Created: time.Now().UnixMilli(), MaxUses: 2, Channel: afk}
	if err := s.store.addInvite(in); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		ana.send(map[string]any{"t": "invite-use", "invite": in.Code})
	}
	time.Sleep(300 * time.Millisecond)
	if got, _ := s.store.invite(in.Code); got.Uses != 0 {
		t.Fatalf("a member used the invite: %d", got.Uses)
	}
	s.hub.mu.Lock()
	ch := s.hub.sessions[ana.sid].ch
	s.hub.mu.Unlock()
	if ch != afk {
		t.Fatalf("in %d, want the invite's channel %d", ch, afk)
	}
	if c, e := dialVWith(t, hs, newKey(t), "Bob", map[string]any{"invite": in.Code}); c == nil {
		t.Fatal(e)
	}
	if got, _ := s.store.invite(in.Code); got.Uses != 1 {
		t.Fatalf("uses %d, want 1", got.Uses)
	}
	// Bad and used-up codes are still refused (after the rate limit's second)
	time.Sleep(1200 * time.Millisecond)
	ana.send(map[string]any{"t": "invite-use", "invite": "nope!"})
	if e := ana.wait(isT("error")); e["op"] != "invite-use" || e["why"] != "invite" {
		t.Fatal(e)
	}
	used := &invite{Comm: spaceID, By: "U", Created: time.Now().UnixMilli(), MaxUses: 1}
	if err := s.store.addInvite(used); err != nil {
		t.Fatal(err)
	}
	if _, err := s.useInvite(used.Code); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	ana.send(map[string]any{"t": "invite-use", "invite": used.Code})
	if e := ana.wait(isT("error")); e["op"] != "invite-use" || e["why"] != "invite" {
		t.Fatal(e)
	}
}
