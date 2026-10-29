package main

import (
	"testing"
	"time"
)

func isErrOp(op string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["t"] == "error" && m["op"] == op }
}

func isDone(op string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["t"] == "done" && m["op"] == op }
}

// memberIn waits for uid's member update in channel ch.
func memberIn(c *tclient, uid string, ch int64) {
	c.t.Helper()
	c.wait(func(m map[string]any) bool {
		if m["t"] != "member" {
			return false
		}
		mm, _ := m["m"].(map[string]any)
		return mm["uid"] == uid && int64(mm["ch"].(float64)) == ch
	})
}

func TestTempChannels(t *testing.T) {
	oldTick := tempTick
	tempTick = 100 * time.Millisecond
	t.Cleanup(func() { tempTick = oldTick })
	ownerKey, own := ownerKeyCfg(t)
	s, hs := newTestServer(t, func(c *config) { own(c); c.tempEmpty = 1 })
	ana, _ := dialV(t, hs, ownerKey, "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	cara, _ := dialV(t, hs, newKey(t), "Cara", "")
	if !hasPerm(bob.wel, "temp_channel") || hasPerm(bob.wel, "events") || hasPerm(bob.wel, "give_lei") {
		t.Fatalf("@everyone's permissions: %v", bob.wel["perms"])
	}
	lobby := chanID(t, bob.wel, "Lobby")

	// Bob makes one: everyone sees it (temporary, his), he's moved into it
	bob.send(map[string]any{"t": "temp-create", "name": "Bob's game", "password": "pw", "max": 3})
	d := bob.wait(isDone("temp-create"))
	id := int64(d["id"].(float64))
	if !isTempID(id) {
		t.Fatalf("id %d", id)
	}
	cm := cara.wait(func(m map[string]any) bool { return m["t"] == "channel" })["c"].(map[string]any)
	if cm["temp"] != true || cm["owner"] != bob.uid || cm["locked"] != true || cm["name"] != "Bob's game" {
		t.Fatal(cm)
	}
	memberIn(cara, bob.uid, id)

	// One at a time per person
	bob.send(map[string]any{"t": "temp-create", "name": "Another"})
	if e := bob.wait(isErrOp("temp-create")); e["why"] != "many" {
		t.Fatal(e)
	}
	// The password holds for others
	cara.send(map[string]any{"t": "join", "ch": id, "password": "no"})
	if e := cara.wait(isErrOp("join")); e["why"] != "password" {
		t.Fatal(e)
	}
	cara.send(map[string]any{"t": "join", "ch": id, "password": "pw"})
	memberIn(cara, cara.uid, id)

	// Not a channel of the server's management: ch-save leaves it, nothing goes under it
	ana.send(map[string]any{"t": "ch-save", "id": id, "name": "Mine now"})
	if e := ana.wait(isErrOp("ch-save")); e["why"] != "bad" {
		t.Fatal(e)
	}
	ana.send(map[string]any{"t": "ch-save", "name": "Child", "parent": id})
	if e := ana.wait(isErrOp("ch-save")); e["why"] != "bad" {
		t.Fatal(e)
	}
	// Its maker edits it, others can't
	cara.send(map[string]any{"t": "temp-edit", "id": id, "name": "Cara's"})
	if e := cara.wait(isErrOp("temp-edit")); e["why"] != "role" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "temp-edit", "id": id, "name": "Renamed", "password": ""})
	bob.wait(isDone("temp-edit"))
	cm = cara.wait(func(m map[string]any) bool {
		c, _ := m["c"].(map[string]any)
		return m["t"] == "channel" && c["name"] == "Renamed"
	})["c"].(map[string]any)
	if cm["locked"] == true {
		t.Fatal("password kept", cm)
	}
	// Its channel admin takes Cara out of it
	bob.send(map[string]any{"t": "ch-kick", "sid": cara.sid})
	memberIn(cara, cara.uid, lobby)
	// Nobody's database row: a restart has nothing of it
	var n int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM channels WHERE id=?`, id).Scan(&n)
	if n != 0 {
		t.Fatal("saved in the table")
	}
	// Its chat goes with it
	bob.send(map[string]any{"t": "say", "to": chanConv(id), "text": "hi"})
	bob.wait(isT("msg"))

	// Empty for SV_TEMP_EMPTY: gone, by itself
	bob.send(map[string]any{"t": "join", "ch": lobby})
	memberIn(bob, bob.uid, lobby)
	cara.wait(func(m map[string]any) bool { return m["t"] == "channel-del" && int64(m["id"].(float64)) == id })
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv=?`, chanConv(id)).Scan(&n)
	if n != 0 {
		t.Fatal("its chat stayed")
	}

	// Without the permission: refused
	var every int64
	for _, r := range ana.wel["roles"].([]any) {
		if rm := r.(map[string]any); rm["everyone"] == true {
			every = int64(rm["id"].(float64))
		}
	}
	ana.send(map[string]any{"t": "role-save", "id": every, "name": "@everyone", "perms": []string{"speak"}})
	cara.wait(func(m map[string]any) bool { return m["t"] == "role" && !hasPerm(m, "temp_channel") })
	cara.send(map[string]any{"t": "temp-create", "name": "x"})
	if e := cara.wait(isErrOp("temp-create")); e["why"] != "role" {
		t.Fatal(e)
	}
}

func TestTempChannelLimits(t *testing.T) {
	ownerKey, own := ownerKeyCfg(t)
	oldN := tempRateN
	tempRateN = 2
	t.Cleanup(func() { tempRateN = oldN })
	s, hs := newTestServer(t, func(c *config) { own(c); c.tempMax = 2 })
	ana, _ := dialV(t, hs, ownerKey, "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	cara, _ := dialV(t, hs, newKey(t), "Cara", "")
	dan, _ := dialV(t, hs, newKey(t), "Dan", "")
	// The server's limit
	bob.send(map[string]any{"t": "temp-create", "name": "b"})
	bid := int64(bob.wait(isDone("temp-create"))["id"].(float64))
	cara.send(map[string]any{"t": "temp-create", "name": "c"})
	cara.wait(isDone("temp-create"))
	dan.send(map[string]any{"t": "temp-create", "name": "d"})
	if e := dan.wait(isErrOp("temp-create")); e["why"] != "full" {
		t.Fatal(e)
	}
	// Deleted by its maker (ch-del too); a manager may delete anyone's
	bob.send(map[string]any{"t": "ch-del", "id": bid})
	bob.wait(isDone("temp-del"))
	// The rate: 2 made per window per person
	bob.send(map[string]any{"t": "temp-create", "name": "b2"})
	bid = int64(bob.wait(isDone("temp-create"))["id"].(float64))
	ana.send(map[string]any{"t": "temp-del", "id": bid})
	ana.wait(isDone("temp-del"))
	bob.send(map[string]any{"t": "temp-create", "name": "b3"})
	if e := bob.wait(isErrOp("temp-create")); e["why"] != "rate" {
		t.Fatal(e)
	}
	// Ids never meet a saved channel's; what a temporary one left (before a stop) is cleared at start
	_, _ = s.store.db.Exec(`INSERT INTO messages(conv,from_uid,from_name,text,at) VALUES(?,?,?,?,?), ('c:1','a','a','kept',1)`, chanConv(tempBase+7), "a", "a", "x", 1)
	s.store.dropTempLeftovers()
	var n int
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv=?`, chanConv(tempBase+7)).Scan(&n)
	if n != 0 {
		t.Fatal("left over")
	}
	_ = s.store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE conv='c:1'`).Scan(&n)
	if n != 1 {
		t.Fatal("a saved channel's chat went too")
	}
	// The default channel is never a temporary one, and the 500 counts only saved channels
	s.hub.mu.Lock()
	def := s.hub.defaultChannel(spaceID)
	saved := s.hub.savedChannels(spaceID)
	s.hub.mu.Unlock()
	if isTempID(def) || saved != 3 {
		t.Fatal(def, saved)
	}
}

// A server from before temp_channel and events: @everyone gets the first, moderators and admins the second, once.
func TestNewPermsMigration(t *testing.T) {
	s, _ := newTestServer(t, nil)
	st := s.store
	_, _ = st.db.Exec(`UPDATE roles SET perms = perms & ?`, int64(pAll&^(pTempChannel|pEvents)))
	_, _ = st.db.Exec(`DELETE FROM settings WHERE key='perm_temp_events'`)
	st.migrateNewPerms()
	rs, _ := st.rolesOf(spaceID)
	for _, r := range rs {
		switch r.Name {
		case "@everyone":
			if r.p&pTempChannel == 0 || r.p&pEvents != 0 {
				t.Fatal(r)
			}
		case "Moderator", "Admin":
			if r.p&pEvents == 0 || r.p&pGiveLei != 0 {
				t.Fatal(r)
			}
		}
	}
	// Taken off by an admin later: not given back
	_, _ = st.db.Exec(`UPDATE roles SET perms = perms & ? WHERE everyone=1`, int64(pAll&^pTempChannel))
	st.migrateNewPerms()
	var p int64
	_ = st.db.QueryRow(`SELECT perms FROM roles WHERE everyone=1`).Scan(&p)
	if perm(p)&pTempChannel != 0 {
		t.Fatal("given back")
	}
}
