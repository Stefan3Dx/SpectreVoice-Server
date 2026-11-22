package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A call's ring isn't held back by the general queue: every general sender stuck on a slow relay, the ring still
// goes out at once.
func TestPushCallNotStuckBehindGeneral(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if err := s.store.migratePush(); err != nil {
		t.Fatal(err)
	}
	_, _ = s.store.db.Exec(`INSERT INTO push_tokens(token,uid,platform,updated) VALUES(?,?,?,?)`, strings.Repeat("g", 40), "G1", "android", 1)
	release := make(chan struct{})
	defer close(release)
	calls := make(chan string, 4)
	p := &pusher{st: s.store, queue: make(chan pushJob, 1000)}
	p.relay = func(token string, data map[string]string, ttl string) error {
		if data["kind"] == "call" {
			calls <- token
			return nil
		}
		<-release
		return nil
	}
	p.start()
	for range pushWorkers * 3 {
		p.notify("G1", map[string]string{"kind": "mention"})
	}
	time.Sleep(100 * time.Millisecond)
	p.notifyOne("C1", strings.Repeat("c", 40), map[string]string{"kind": "call"}, "45s")
	select {
	case tok := <-calls:
		if tok != strings.Repeat("c", 40) {
			t.Fatalf("token %q", tok)
		}
	case <-time.After(time.Second):
		t.Fatal("the ring waited behind the general pushes")
	}
}

// A call's pushes to one phone keep their order: its end never overtakes its ring.
func TestPushCallOrderPerDevice(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	done := make(chan struct{}, 1000)
	p := &pusher{queue: make(chan pushJob, 16)}
	p.relay = func(token string, data map[string]string, ttl string) error {
		mu.Lock()
		seen[token] = append(seen[token], data["n"])
		mu.Unlock()
		done <- struct{}{}
		return nil
	}
	p.start()
	toks := []string{strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)}
	const rounds = 10 // 60 in all: room for them even if every token hashes to the same lane
	for i := range rounds {
		for _, tk := range toks {
			p.notifyOne("U", tk, map[string]string{"kind": "call", "n": fmt.Sprint(2 * i)}, "45s")
			p.notifyOne("U", tk, map[string]string{"kind": "call-end", "n": fmt.Sprint(2*i + 1)}, "")
		}
	}
	for range rounds * 2 * len(toks) {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("pushes missing")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, tk := range toks {
		for i, n := range seen[tk] {
			if n != fmt.Sprint(i) {
				t.Fatalf("%s out of order: %v", tk[:1], seen[tk])
			}
		}
	}
}

// A pusher without lanes (made by hand in tests) still gets a call's push in its queue.
func TestPushCallWithoutLanesUsesQueue(t *testing.T) {
	p := &pusher{queue: make(chan pushJob, 1)}
	p.notifyOne("U", "tok", map[string]string{"kind": "call"}, "45s")
	select {
	case j := <-p.queue:
		if j.only != "tok" || j.ttl != "45s" {
			t.Fatalf("job %+v", j)
		}
	default:
		t.Fatal("not queued")
	}
}

// A relay that doesn't answer holds a call's lane no longer than callPushWait.
func TestPushCallRelayDeadline(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	p := &pusher{}
	p.relay = func(token string, data map[string]string, ttl string) error { <-stuck; return nil }
	start := time.Now()
	if err := p.sendWithin("tok", nil, "45s", 100*time.Millisecond); err == nil {
		t.Fatal("no error for a relay that never answered")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("waited past the deadline")
	}
}
