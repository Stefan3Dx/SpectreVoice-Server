package main

import (
	"testing"
)

// Moving people: who may (move, ranking above), only into voice channels, past a password and a limit,
// not past a channel ban without channel_ban; the moved person is told who moved them and where.
func TestMovePeople(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	mo, _ := dialV(t, hs, newKey(t), "Mo", "")
	mia, _ := dialV(t, hs, newKey(t), "Mia", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	cara, _ := dialV(t, hs, newKey(t), "Cara", "")
	_ = s.store.setRole(ana.uid, roleOwner)
	s.hub.mu.Lock()
	s.hub.forget(1)
	s.hub.mu.Unlock()
	ana.ws.Close()
	ana, _ = dialV(t, hs, ana.key, "Ana", "")
	// Mo and Mia get the server's Moderator role, from the owner
	rs, _ := s.store.rolesOf(spaceID)
	var modID int64
	for _, r := range rs {
		if r.Name == "Moderator" {
			modID = r.ID
		}
	}
	for _, c := range []*tclient{mo, mia} {
		ana.send(map[string]any{"t": "member-roles", "uid": c.uid, "roles": []any{modID}})
		c.wait(func(m map[string]any) bool { return m["t"] == "role" && hasPerm(m, "move") })
	}
	if hasPerm(bob.wel, "move") {
		t.Fatal(bob.wel["perms"])
	}
	afk := chanID(t, ana.wel, "AFK")
	ana.send(map[string]any{"t": "ch-save", "name": "Secret", "password": "pw", "max": 1})
	secret := int64(ana.wait(func(m map[string]any) bool {
		return m["t"] == "channel" && m["c"].(map[string]any)["name"] == "Secret"
	})["c"].(map[string]any)["id"].(float64))
	ana.send(map[string]any{"t": "ch-save", "name": "news", "kind": "text"})
	text := int64(ana.wait(func(m map[string]any) bool {
		return m["t"] == "channel" && m["c"].(map[string]any)["name"] == "news"
	})["c"].(map[string]any)["id"].(float64))

	// A member can't move anyone
	bob.send(map[string]any{"t": "move", "sid": cara.sid, "ch": afk})
	if e := bob.wait(isErrOp("move")); e["why"] != "role" {
		t.Fatal(e)
	}
	// A moderator moves a member; the member is told who and where
	mo.send(map[string]any{"t": "move", "sid": bob.sid, "ch": afk})
	if mv := bob.wait(isT("moved")); int64(mv["ch"].(float64)) != afk || mv["by"] != "Mo" || mv["name"] != "AFK" {
		t.Fatal(mv)
	}
	s.hub.mu.Lock()
	in := s.hub.sessions[bob.sid].ch
	s.hub.mu.Unlock()
	if in != afk {
		t.Fatal("not moved", in)
	}
	// Nor the owner, nor another moderator (as high as them)
	mo.send(map[string]any{"t": "move", "sid": ana.sid, "ch": afk})
	if e := mo.wait(isErrOp("move")); e["why"] != "role" {
		t.Fatal(e)
	}
	mo.send(map[string]any{"t": "move", "sid": mia.sid, "ch": afk})
	if e := mo.wait(isErrOp("move")); e["why"] != "role" {
		t.Fatal(e)
	}
	// Only into a voice channel; someone gone is said so
	mo.send(map[string]any{"t": "move", "sid": bob.sid, "ch": text})
	if e := mo.wait(isErrOp("move")); e["why"] != "bad" {
		t.Fatal(e)
	}
	mo.send(map[string]any{"t": "move", "sid": 99999, "ch": afk})
	if e := mo.wait(isErrOp("move")); e["why"] != "gone" {
		t.Fatal(e)
	}
	// Past the password and the limit (one place, taken by Cara first)
	mo.send(map[string]any{"t": "move", "sid": cara.sid, "ch": secret})
	cara.wait(isT("moved"))
	mo.send(map[string]any{"t": "move", "sid": bob.sid, "ch": secret})
	if mv := bob.wait(isT("moved")); int64(mv["ch"].(float64)) != secret {
		t.Fatal(mv)
	}
	// The owner may move a moderator, and oneself
	ana.send(map[string]any{"t": "move", "sid": mo.sid, "ch": afk})
	if mv := mo.wait(isT("moved")); mv["by"] != "Ana" {
		t.Fatal(mv)
	}

	// A channel ban: a mover without channel_ban can't bring them in; a moderator (who has it) can
	ana.send(map[string]any{"t": "ch-ban", "uid": cara.uid, "ch": afk})
	ana.wait(isDone("ch-ban"))
	ana.send(map[string]any{"t": "role-save", "name": "Mover", "pos": 5, "perms": []string{"move"}})
	r := ana.wait(func(m map[string]any) bool { return m["t"] == "roles" })
	var rid float64
	for _, x := range r["items"].([]any) {
		if x.(map[string]any)["name"] == "Mover" {
			rid = x.(map[string]any)["id"].(float64)
		}
	}
	ana.send(map[string]any{"t": "member-roles", "uid": bob.uid, "roles": []any{rid}})
	bob.wait(func(m map[string]any) bool { return m["t"] == "role" && hasPerm(m, "move") })
	bob.send(map[string]any{"t": "move", "sid": cara.sid, "ch": afk})
	if e := bob.wait(isErrOp("move")); e["why"] != "banned" {
		t.Fatal(e)
	}
	mia.send(map[string]any{"t": "move", "sid": cara.sid, "ch": afk})
	if mv := cara.wait(isT("moved")); int64(mv["ch"].(float64)) != afk || mv["by"] != "Mia" {
		t.Fatal(mv)
	}
}
