//go:build !plus

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// The public build: a donation is redeemed and kept, but its places come only with the official Plus build, which
// it fetches (signed by the service) and hands over to.
func TestDonationSlots(t *testing.T) {
	ls := newFakeLS(t)
	ran := make(chan string, 4)
	execPlus = func(path string) error { ran <- path; return nil }
	upgradeRetry = 50 * time.Millisecond
	s, hs := newTestServer(t, func(c *config) { c.licenseURL = ls.URL; c.vendorKey = ls.vendor; c.owners = "" })
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	lim := ana.wel["tier"].(map[string]any)["limits"].(map[string]any)
	if lim["maxUsers"].(float64) != 30 || lim["audioMax"].(float64) != 6 || lim["videoMax"].(float64) != 8 || lim["cosmetics"] != true {
		t.Fatal(lim)
	}
	// Only the server's admins redeem
	ana.send(map[string]any{"t": "slots-redeem", "code": "SVD-GOOD"})
	if e := ana.wait(isT("error")); e["why"] != "role" {
		t.Fatal(e)
	}
	_ = s.store.setRole(ana.uid, roleOwner)
	ana.send(map[string]any{"t": "slots-redeem", "code": "SVD-BAD1"})
	if e := ana.wait(isT("error")); e["why"] != "slots" {
		t.Fatal(e)
	}
	ana.send(map[string]any{"t": "slots-redeem", "code": "SVD-GOOD"})
	st := ana.wait(func(m map[string]any) bool { return m["t"] == "slots" && len(m["donations"].([]any)) == 1 })
	if st["edition"] != "free" || st["note"] == nil || st["limits"].(map[string]any)["maxUsers"].(float64) != 30 {
		t.Fatal(st)
	}
	if s.maxUsers() != 30 {
		t.Fatal(s.maxUsers())
	}
	// The Plus build was fetched, checked and run
	select {
	case p := <-ran:
		if p != filepath.Join(s.cfg.data, "bin", plusName) {
			t.Fatal(p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Plus not run")
	}
	if _, m, err := verifyPlus(s.cfg.data, s.lic.vendor); err != nil || m.Version != "9.9.9" {
		t.Fatal(err, m)
	}
	if s.lic.keptClock() == nil {
		t.Fatal("no clock kept")
	}
	// At the next start the public build hands over to it
	os.Unsetenv("SV_EXEC_PLUS")
	runPlusIfPresent(s.cfg)
	if p := <-ran; p == "" {
		t.Fatal(p)
	}
	// Changed by a byte: it doesn't run
	path := filepath.Join(s.cfg.data, "bin", plusName)
	b, _ := os.ReadFile(path)
	_ = os.WriteFile(path, append(b, 'x'), 0o750)
	if _, _, err := verifyPlus(s.cfg.data, s.lic.vendor); err == nil {
		t.Fatal("a changed Plus build passed")
	}
	runPlusIfPresent(s.cfg)
	select {
	case p := <-ran:
		t.Fatal("ran a changed build", p)
	case <-time.After(100 * time.Millisecond):
	}
}

// A token is for one server key and one machine.
func TestDonationTokenBinding(t *testing.T) {
	ls := newFakeLS(t)
	s, _ := newTestServer(t, func(c *config) { c.licenseURL = ls.URL; c.vendorKey = ls.vendor })
	sl := 50
	exp := time.Now().Add(time.Hour).UnixMilli()
	if err := s.lic.add(ls.sign(slotClaims{V: 2, Lic: "a", Tier: "gift", Srv: "other", Hw: s.lic.hw, Exp: exp, Slots: &sl})); err == nil {
		t.Fatal("another server's token was taken")
	}
	if err := s.lic.add(ls.sign(slotClaims{V: 2, Lic: "b", Tier: "gift", Srv: s.lic.fp, Hw: []string{hwPart("machine", "x"), hwPart("mac", "y")}, Exp: exp, Slots: &sl})); err == nil {
		t.Fatal("another machine's token was taken")
	}
	// One part changed of three still matches; two of three don't
	hw := append([]string{hwPart("disk", "z")}, s.lic.hw...)
	if err := s.lic.add(ls.sign(slotClaims{V: 2, Lic: "c", Tier: "gift", Srv: s.lic.fp, Hw: hw, Exp: exp, Slots: &sl})); err != nil {
		t.Fatal(err)
	}
	if hwMatch([]string{s.lic.hw[0], hwPart("x", "1"), hwPart("y", "2")}, s.lic.hw) {
		t.Fatal("one of three matched")
	}
	// Not signed by the service
	other := newKey(t)
	c := slotClaims{V: 2, Lic: "d", Tier: "gift", Srv: s.lic.fp, Hw: s.lic.hw, Exp: exp, Slots: &sl}
	fake := (&fakeLS{vk: other}).sign(c)
	if err := s.lic.add(fake); err == nil {
		t.Fatal("a self-made token was taken")
	}
	if s.limits().MaxUsers != 30 {
		t.Fatal(s.limits())
	}
	// The service's time counts only for the request that asked (an old answer replayed doesn't)
	clk := ls.sign(slotClaims{V: 2, Lic: "clock:n1", Tier: "time", Srv: s.lic.fp, Hw: s.lic.hw, Iat: time.Now().UnixMilli(), Exp: exp})
	if s.lic.keepClock(clk, "n2") != nil || s.lic.keepClock(clk, "n1") == nil {
		t.Fatal("clock nonce")
	}
}

func TestCmpVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"0.5.0", "0.4.9", 1}, {"0.5.0", "0.5.0", 0}, {"dev", "0.1.0", -1}, {"1.10.0", "1.9.3", 1}, {"v0.5.1", "0.5.1-rc1", 0}} {
		if got := cmpVersion(c.a, c.b); got != c.want {
			t.Fatal(c, got)
		}
	}
}

// A hosting plan's code that can't be redeemed at start (the service busy, the network not up yet) is tried again.
func TestPartnerCodeRetried(t *testing.T) {
	ls := newFakeLS(t)
	execPlus = func(string) error { return nil }
	upgradeRetry = 50 * time.Millisecond
	partnerRetry = 50 * time.Millisecond
	defer func() { partnerRetry = time.Minute }()
	var calls atomic.Int32
	target, _ := url.Parse(ls.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/gift" && calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"try again later"}`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer busy.Close()
	s, _ := newTestServer(t, func(c *config) { c.licenseURL = busy.URL; c.vendorKey = ls.vendor; c.partnerCode = "SVD-GOOD" })
	deadline := time.Now().Add(5 * time.Second)
	for len(s.lic.donations()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(s.lic.donations()) != 1 || calls.Load() < 3 {
		t.Fatalf("plan not redeemed after the service came back (%d calls, %v)", calls.Load(), s.lic.donations())
	}
}
