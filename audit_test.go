package main

import (
	"crypto/x509"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Channel passwords: a few tries a minute, then a pause (bcrypt runs outside the hub's lock).
func TestJoinPasswordTries(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	_, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	a, _ := dialV(t, hs, ownerKey, "Ana", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	a.send(map[string]any{"t": "ch-save", "name": "Secret", "password": "pw"})
	secret := int64(a.wait(isT("channel"))["c"].(map[string]any)["id"].(float64))
	for i := 0; i < 5; i++ {
		b.send(map[string]any{"t": "join", "ch": secret, "password": "nope"})
		if e := b.wait(isT("error")); e["why"] != "password" {
			t.Fatal(i, e)
		}
	}
	b.send(map[string]any{"t": "join", "ch": secret, "password": "pw"})
	if e := b.wait(isT("error")); e["why"] != "rate" {
		t.Fatal("no pause after five tries:", e)
	}
}

// Managing a channel's files is decided in the file's community, not in the one the asking session is in.
func TestChannelFilesOtherCommunity(t *testing.T) {
	_, hs := newTestServer(t, func(c *config) { c.fileMB = 10; c.createCommunities = "anyone" })
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	eveKey := newKey(t)
	eve1, _ := dialV(t, hs, eveKey, "Eve", "") // a plain member of the main community
	eve2, _ := dialV(t, hs, eveKey, "Eve", "")
	lobby := chanID(t, bob.wel, "Lobby")
	req, _ := http.NewRequest("POST", hs.URL+"/cf/upload?ch="+itoa(int(lobby))+"&folder=0&name=a.txt", strings.NewReader("bob's"))
	req.Header.Set("Authorization", "Bearer "+bob.tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ ID int64 }
	_ = json.NewDecoder(res.Body).Decode(&got)
	res.Body.Close()
	// Eve's second session makes its own community (everything allowed there) and asks from it
	eve2.send(map[string]any{"t": "community-create", "name": "Mine", "open": true})
	eve2.wait(func(m map[string]any) bool { return m["t"] == "community" && commID(m) != 1 })
	_ = eve1
	for _, op := range []string{"cf-del", "cf-rename"} {
		eve2.send(map[string]any{"t": op, "id": got.ID, "name": "x"})
		if e := eve2.wait(isT("error")); e["why"] != "role" {
			t.Fatal(op, e)
		}
	}
}

// Lifting a channel ban: only in the moderator's own community's channels, and never for oneself.
func TestChannelUnbanScoped(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki); c.createCommunities = "anyone" })
	a, _ := dialV(t, hs, ownerKey, "Ana", "")
	eve, _ := dialV(t, hs, newKey(t), "Eve", "")
	lobby := chanID(t, a.wel, "Lobby")
	a.send(map[string]any{"t": "ch-ban", "uid": eve.uid, "ch": lobby})
	eve.send(map[string]any{"t": "community-create", "name": "Mine", "open": true})
	eve.wait(func(m map[string]any) bool { return m["t"] == "community" && commID(m) != 1 })
	eve.send(map[string]any{"t": "ch-unban", "uid": eve.uid, "ch": lobby})
	if e := eve.wait(isT("error")); e["why"] != "role" {
		t.Fatal(e)
	}
	if !s.mods.channelBanned(eve.uid, lobby) {
		t.Fatal("the ban was lifted from another community")
	}
}
