package main

import (
	"crypto/x509"
	"math"
	"testing"
	"time"
)

// A ban's minutes: too many is forever, never an end that overflowed into the past.
func TestUntil(t *testing.T) {
	now := time.Now().UnixMilli()
	for _, m := range []int64{0, -5, maxModMinutes + 1, 9_000_000_000_000, math.MaxInt64} {
		if got := until(m); got != 0 {
			t.Fatalf("until(%d) = %d, want 0 (forever)", m, got)
		}
	}
	if got := until(60); got < now+3600_000-1000 || got > now+3600_000+1000 {
		t.Fatalf("until(60) = %d, now %d", got, now)
	}
	if got := until(maxModMinutes); got <= now {
		t.Fatalf("until(max) = %d, in the past", got)
	}
}

func TestHugeBanHolds(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	ana, _ := dialV(t, hs, ownerKey, "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	ana.send(map[string]any{"t": "ban", "uid": bob.uid, "minutes": float64(9e12)})
	ana.wait(isDone("ban"))
	if !s.mods.banned(bob.uid, spaceID) {
		t.Fatal("a ban for very long ran out at once")
	}
}
