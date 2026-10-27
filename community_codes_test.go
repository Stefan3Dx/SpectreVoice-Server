package main

import "testing"

// No public list; a code finds a community: open ones let you in, closed ones take a request a moderator answers.
func TestCommunityCodes(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.createCommunities = "anyone" })
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	carl, _ := dialV(t, hs, newKey(t), "Carl", "")
	ana.send(map[string]any{"t": "community-create", "name": "Clubul", "open": false})
	cm := ana.wait(func(m map[string]any) bool { return m["t"] == "community" && commID(m) != 1 })
	code := cm["community"].(map[string]any)["code"].(string)
	if len(code) != 9 || code[4] != '-' {
		t.Fatal(code)
	}
	// Nothing listed
	bob.send(map[string]any{"t": "discover"})
	if d := bob.wait(isT("discover")); len(d["items"].([]any)) != 0 {
		t.Fatal(d)
	}
	// Found by its code (typed any old way): the name, nothing about its people
	bob.send(map[string]any{"t": "community-find", "code": " " + lower(code[:4]) + lower(code[5:]) + " "})
	f := bob.wait(isT("community-found"))
	fc := f["community"].(map[string]any)
	if fc["name"] != "Clubul" || fc["open"] != false || fc["owner"] != nil || fc["members"] != nil {
		t.Fatal(f)
	}
	bob.send(map[string]any{"t": "community-find", "code": "ZZZZ-ZZZZ"})
	if e := bob.wait(isT("error")); e["why"] != "unknown" {
		t.Fatal(e)
	}
	// Closed: a request; Ana (its owner, a moderator) hears of it and lets Bob in, turns Carl down
	bob.send(map[string]any{"t": "community-join", "code": code})
	bob.wait(isT("community-requested"))
	if r := ana.wait(isT("community-request")); r["uid"] != bob.uid {
		t.Fatal(r)
	}
	carl.send(map[string]any{"t": "community-join", "code": code})
	carl.wait(isT("community-requested"))
	ana.send(map[string]any{"t": "community-requests"})
	if l := ana.wait(isT("community-requests")); len(l["items"].([]any)) != 2 {
		t.Fatal(l)
	}
	// Bob can't answer requests
	bob.send(map[string]any{"t": "community-requests"})
	if e := bob.wait(isT("error")); e["why"] != "role" {
		t.Fatal(e)
	}
	ana.send(map[string]any{"t": "community-answer", "uid": bob.uid, "accept": true})
	if a := bob.wait(isT("community-answered")); a["accepted"] != true {
		t.Fatal(a)
	}
	if !s.store.isMember(commID(cm), bob.uid) {
		t.Fatal("Bob isn't in")
	}
	// A member gets back into the closed community by its id (switching between his communities)
	bob.send(map[string]any{"t": "community-join", "c": commID(cm)})
	if c := bob.wait(isT("community")); commID(c) != commID(cm) {
		t.Fatal(c)
	}
	// A channel of another community: refused for history (the app keeps it quiet)
	bob.send(map[string]any{"t": "history", "to": "c:999999"})
	if e := bob.wait(isT("error")); e["op"] != "history" {
		t.Fatal(e)
	}
	ana.send(map[string]any{"t": "community-answer", "uid": carl.uid, "accept": false})
	if a := carl.wait(isT("community-answered")); a["accepted"] != false || s.store.isMember(commID(cm), carl.uid) {
		t.Fatal(a)
	}
	// A new code: the old one stops working
	ana.send(map[string]any{"t": "community-new-code"})
	info := ana.wait(isT("community-info"))
	if nc := info["community"].(map[string]any)["code"]; nc == code {
		t.Fatal("same code")
	}
	carl.send(map[string]any{"t": "community-find", "code": code})
	if e := carl.wait(isT("error")); e["why"] != "unknown" {
		t.Fatal(e)
	}
	// Open: straight in
	ana.send(map[string]any{"t": "community-save", "open": true})
	info = ana.wait(isT("community-info"))
	carl.send(map[string]any{"t": "community-join", "code": info["community"].(map[string]any)["code"]})
	if c := carl.wait(isT("community")); commID(c) != commID(cm) {
		t.Fatal(c)
	}
	// Guessing is slowed down
	for i := 0; i < 12; i++ {
		bob.send(map[string]any{"t": "community-find", "code": "AAAA-AAA" + string(codeAlphabet[i])})
	}
	bob.wait(func(m map[string]any) bool { return m["t"] == "error" && m["why"] == "slow" })
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
