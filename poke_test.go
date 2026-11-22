package main

import (
	"crypto/x509"
	"testing"
	"time"
)

// Muted from writing: no pokes either; and one poke per pokeGap to the same person.
func TestPokeMutedAndRate(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	mod, _ := dialV(t, hs, ownerKey, "Mod", "")
	a, _ := dialV(t, hs, newKey(t), "Ana", "")
	b, _ := dialV(t, hs, newKey(t), "Bob", "")
	c, _ := dialV(t, hs, newKey(t), "Cara", "")
	isPoke := func(m map[string]any) bool { return m["t"] == "poke" }

	mod.send(map[string]any{"t": "mute", "uid": a.uid})
	mod.wait(isDone("mute"))
	a.send(map[string]any{"t": "poke", "uid": b.uid, "text": "hey"})
	if e := a.wait(isErrOp("poke")); e["why"] != "muted" {
		t.Fatal(e)
	}
	if !b.quiet(300*time.Millisecond, isPoke) {
		t.Fatal("a muted person's poke arrived")
	}
	mod.send(map[string]any{"t": "unmute", "uid": a.uid})
	mod.wait(isDone("unmute"))
	a.send(map[string]any{"t": "poke", "uid": b.uid, "text": "hey"})
	if p := b.wait(isPoke); p["from"] != a.uid || p["text"] != "hey" {
		t.Fatal(p)
	}

	// Again at once: refused; someone else still can be poked
	a.send(map[string]any{"t": "poke", "uid": b.uid, "text": "hey"})
	if e := a.wait(isErrOp("poke")); e["why"] != "rate" {
		t.Fatal(e)
	}
	if !b.quiet(300*time.Millisecond, isPoke) {
		t.Fatal("a second poke within the gap arrived")
	}
	a.send(map[string]any{"t": "poke", "uid": c.uid})
	c.wait(isPoke)

	// After the gap it goes through
	setGap := func(d time.Duration) { s.hub.mu.Lock(); pokeGap = d; s.hub.mu.Unlock() } // read under h.mu
	old := pokeGap
	setGap(50 * time.Millisecond)
	t.Cleanup(func() { setGap(old) })
	time.Sleep(100 * time.Millisecond)
	a.send(map[string]any{"t": "poke", "uid": b.uid})
	b.wait(isPoke)
}
