package main

import (
	"crypto/x509"
	"testing"
)

// Chat search: only where one may read (a locked channel's chat not, someone else's private chat not), every
// conversation at once with "*", case-insensitive with % and _ taken literally, paged by "before", 2–64 characters,
// rate-limited.
func TestSearch(t *testing.T) {
	_, hs := newTestServer(t, nil)
	a, _ := dialV(t, hs, newKey(t), "Ana", "")
	b, _ := dialV(t, hs, newKey(t), "Bogdan", "")
	c, _ := dialV(t, hs, newKey(t), "Cristi", "")
	lobby := chanID(t, a.wel, "Lobby")
	lc := "c:" + itoa(int(lobby))

	a.send(map[string]any{"t": "say", "to": lc, "text": "Hello World"})
	a.send(map[string]any{"t": "say", "to": lc, "text": "100% sure_thing"})
	a.send(map[string]any{"t": "say", "to": lc, "text": "nothing here"})
	for i := 0; i < 3; i++ {
		b.wait(isT("msg"))
	}
	b.send(map[string]any{"t": "say", "to": "u:" + a.uid, "text": "hello privately"})
	a.wait(isT("msg"))
	comm := int(a.wel["community"].(map[string]any)["id"].(float64))
	b.send(map[string]any{"t": "say", "to": "g:" + itoa(comm), "text": "hello community"})
	a.wait(func(m map[string]any) bool {
		return m["t"] == "msg" && m["m"].(map[string]any)["text"] == "hello community"
	})

	// In one channel, case-insensitive
	a.send(map[string]any{"t": "search", "q": "hello", "to": lc})
	r := a.wait(isT("search"))
	if items := r["items"].([]any); len(items) != 1 || items[0].(map[string]any)["text"] != "Hello World" || r["more"] != false {
		t.Fatal(r)
	}
	// Everywhere: the channel, the community chat and the private one (newest first)
	a.send(map[string]any{"t": "search", "q": "hello", "to": "*"})
	r = a.wait(isT("search"))
	if items := r["items"].([]any); len(items) != 3 || items[0].(map[string]any)["text"] != "hello community" || items[2].(map[string]any)["text"] != "Hello World" {
		t.Fatal(r)
	}
	// Someone else's private chat isn't searched, even by name
	c.send(map[string]any{"t": "search", "q": "privately", "to": "*"})
	if r = c.wait(isT("search")); len(r["items"].([]any)) != 0 {
		t.Fatal("private chat leaked", r)
	}
	c.send(map[string]any{"t": "search", "q": "privately", "to": "u:" + a.uid})
	if r = c.wait(isT("search")); len(r["items"].([]any)) != 0 {
		t.Fatal("private chat leaked", r)
	}
	// % and _ mean themselves
	a.send(map[string]any{"t": "search", "q": "100%", "to": lc})
	if r = a.wait(isT("search")); len(r["items"].([]any)) != 1 {
		t.Fatal(r)
	}
	a.send(map[string]any{"t": "search", "q": "e_t", "to": lc})
	if r = a.wait(isT("search")); len(r["items"].([]any)) != 1 || r["items"].([]any)[0].(map[string]any)["text"] != "100% sure_thing" {
		t.Fatal(r)
	}
	// Too short, too long
	a.send(map[string]any{"t": "search", "q": "h", "to": lc})
	if e := a.wait(isT("error")); e["op"] != "search" || e["why"] != "bad" {
		t.Fatal(e)
	}
	// Paging: 60 matches, 50 then the rest
	for i := 0; i < 60; i++ {
		a.send(map[string]any{"t": "say", "to": lc, "text": "page " + itoa(i)})
		a.wait(func(m map[string]any) bool {
			return m["t"] == "msg" && m["m"].(map[string]any)["text"] == "page "+itoa(i)
		})
	}
	b.send(map[string]any{"t": "search", "q": "page", "to": lc})
	r = b.wait(isT("search"))
	items := r["items"].([]any)
	if len(items) != 50 || r["more"] != true || items[0].(map[string]any)["text"] != "page 59" {
		t.Fatal(len(items), r["more"])
	}
	oldest := items[49].(map[string]any)["id"]
	b.send(map[string]any{"t": "search", "q": "page", "to": lc, "before": oldest})
	r = b.wait(isT("search"))
	if items = r["items"].([]any); len(items) != 10 || r["more"] != false || items[9].(map[string]any)["text"] != "page 0" {
		t.Fatal(len(items), r["more"])
	}
	// Two a second (bursts of five): the sixth in a row is refused
	rate := false
	for i := 0; i < 8; i++ {
		c.send(map[string]any{"t": "search", "q": "page", "to": lc})
		if m := c.wait(func(m map[string]any) bool { return m["t"] == "search" || m["t"] == "error" }); m["t"] == "error" && m["why"] == "rate" {
			rate = true
			break
		}
	}
	if !rate {
		t.Fatal("no rate limit")
	}
}

// A locked channel's chat is searched only by who is in it.
func TestSearchLocked(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	_, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	a, _ := dialV(t, hs, ownerKey, "Ana", "")
	b, _ := dialV(t, hs, newKey(t), "Bogdan", "")
	a.send(map[string]any{"t": "ch-save", "name": "Secret", "password": "pw"})
	secret := int64(a.wait(isT("channel"))["c"].(map[string]any)["id"].(float64))
	b.wait(isT("channel"))
	a.send(map[string]any{"t": "join", "ch": secret})
	a.wait(func(m map[string]any) bool {
		return m["t"] == "member" && m["m"].(map[string]any)["uid"] == a.uid && int64(m["m"].(map[string]any)["ch"].(float64)) == secret
	})
	a.send(map[string]any{"t": "say", "to": "c:" + itoa(int(secret)), "text": "the secret word"})
	a.wait(isT("msg"))
	b.send(map[string]any{"t": "search", "q": "secret", "to": "c:" + itoa(int(secret))})
	if e := b.wait(isT("error")); e["op"] != "search" {
		t.Fatal(e)
	}
	b.send(map[string]any{"t": "search", "q": "secret", "to": "*"})
	if r := b.wait(isT("search")); len(r["items"].([]any)) != 0 {
		t.Fatal("locked chat searched from outside", r)
	}
	b.send(map[string]any{"t": "join", "ch": secret, "password": "pw"})
	b.wait(func(m map[string]any) bool {
		return m["t"] == "member" && m["m"].(map[string]any)["uid"] == b.uid && int64(m["m"].(map[string]any)["ch"].(float64)) == secret
	})
	b.send(map[string]any{"t": "search", "q": "secret", "to": "*"})
	if r := b.wait(isT("search")); len(r["items"].([]any)) != 1 {
		t.Fatal(r)
	}
}
