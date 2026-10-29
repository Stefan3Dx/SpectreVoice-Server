package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type lclient struct {
	t  *testing.T
	ws *websocket.Conn
}

func linkDial(t *testing.T, hs *httptest.Server) *lclient {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+"/v1/link", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return &lclient{t, ws}
}

func (c *lclient) send(m map[string]any) {
	c.t.Helper()
	if err := c.ws.WriteJSON(m); err != nil {
		c.t.Fatal(err)
	}
}

// next: the next message of type tt (others skipped), or fails.
func (c *lclient) next(tt string) map[string]any {
	c.t.Helper()
	_ = c.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m map[string]any
		if err := c.ws.ReadJSON(&m); err != nil {
			c.t.Fatalf("waiting for %s: %v", tt, err)
		}
		if m["t"] == tt {
			return m
		}
	}
}

func (c *lclient) open() string {
	c.t.Helper()
	c.send(map[string]any{"t": "link-open"})
	m := c.next("link-opened")
	id, _ := m["id"].(string)
	if len(id) != linkIDLen {
		c.t.Fatalf("id %q", id)
	}
	return id
}

func TestLinkRelay(t *testing.T) {
	s, hs := newTestServer(t, nil)
	a := linkDial(t, hs)
	id := a.open()
	for _, ch := range id {
		if !strings.ContainsRune(linkAlphabet, ch) {
			t.Fatalf("id %q outside the alphabet", id)
		}
	}
	b := linkDial(t, hs)
	b.send(map[string]any{"t": "link-join", "id": id, "m": "hello-from-b"})
	b.next("link-joined")
	if m := a.next("link-peer"); m["m"] != "hello-from-b" {
		t.Fatalf("peer %v", m)
	}
	a.send(map[string]any{"t": "link-msg", "m": "to-b"})
	if m := b.next("link-msg"); m["m"] != "to-b" {
		t.Fatalf("msg %v", m)
	}
	b.send(map[string]any{"t": "link-msg", "m": "to-a"})
	if m := a.next("link-msg"); m["m"] != "to-a" {
		t.Fatalf("msg %v", m)
	}
	// A third socket can't join a busy rendezvous, nor talk into it
	c := linkDial(t, hs)
	c.send(map[string]any{"t": "link-join", "id": id, "m": "x"})
	if m := c.next("link-error"); m["why"] != "busy" {
		t.Fatalf("busy: %v", m)
	}
	c.send(map[string]any{"t": "link-msg", "m": "x"})
	if m := c.next("link-error"); m["why"] != "state" {
		t.Fatalf("state: %v", m)
	}
	// Only strings are relayed, and not big ones
	a.send(map[string]any{"t": "link-msg", "m": map[string]any{"x": 1}})
	if m := a.next("link-error"); m["why"] != "bad" {
		t.Fatalf("object: %v", m)
	}
	a.send(map[string]any{"t": "link-msg", "m": strings.Repeat("A", linkMaxMsg+1)})
	if m := a.next("link-error"); m["why"] != "size" {
		t.Fatalf("size: %v", m)
	}
	// The opener closes: the joiner is told, the id is gone
	a.send(map[string]any{"t": "link-close", "why": "done"})
	if m := b.next("link-closed"); m["why"] != "done" {
		t.Fatalf("closed: %v", m)
	}
	time.Sleep(50 * time.Millisecond)
	s.link.mu.Lock()
	n := len(s.link.open)
	s.link.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d rendezvous left", n)
	}
	d := linkDial(t, hs)
	d.send(map[string]any{"t": "link-join", "id": id, "m": "x"})
	if m := d.next("link-error"); m["why"] != "gone" {
		t.Fatalf("gone: %v", m)
	}
}

// Each join is one guess at the code: three in all, then the rendezvous ends.
func TestLinkTries(t *testing.T) {
	_, hs := newTestServer(t, nil)
	a := linkDial(t, hs)
	id := a.open()
	for i := 0; i < linkMaxJoins; i++ {
		b := linkDial(t, hs)
		b.send(map[string]any{"t": "link-join", "id": id, "m": "guess"})
		b.next("link-joined")
		a.next("link-peer")
		if i < linkMaxJoins-1 {
			// The opener sends the wrong guess away and waits for another
			a.send(map[string]any{"t": "link-close", "peer": true, "why": "code"})
			if m := b.next("link-closed"); m["why"] != "code" {
				t.Fatalf("kicked: %v", m)
			}
		} else {
			// The last one leaves by itself: no try is left, so the opener's rendezvous ends too
			b.send(map[string]any{"t": "link-close", "why": "code"})
			if m := a.next("link-closed"); m["why"] != "tries" {
				t.Fatalf("tries: %v", m)
			}
		}
	}
	b := linkDial(t, hs)
	b.send(map[string]any{"t": "link-join", "id": id, "m": "guess"})
	if m := b.next("link-error"); m["why"] != "gone" {
		t.Fatalf("after the tries: %v", m)
	}
}

func TestLinkTTL(t *testing.T) {
	old := linkTTL
	linkTTL = 300 * time.Millisecond
	t.Cleanup(func() { linkTTL = old })
	_, hs := newTestServer(t, nil)
	a := linkDial(t, hs)
	id := a.open()
	if m := a.next("link-closed"); m["why"] != "expired" {
		t.Fatalf("expired: %v", m)
	}
	b := linkDial(t, hs)
	b.send(map[string]any{"t": "link-join", "id": id, "m": "x"})
	if m := b.next("link-error"); m["why"] != "gone" {
		t.Fatalf("after expiry: %v", m)
	}
}

func TestLinkRate(t *testing.T) {
	_, hs := newTestServer(t, nil)
	// Opens per address: a burst, then refused
	refused := false
	for i := 0; i < linkOpenBurst+2 && !refused; i++ {
		a := linkDial(t, hs)
		a.send(map[string]any{"t": "link-open"})
		_ = a.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		var m map[string]any
		if a.ws.ReadJSON(&m) != nil {
			t.Fatal("no answer")
		}
		if m["t"] == "link-error" && m["why"] == "rate" {
			refused = true
		}
		_ = a.ws.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if !refused {
		t.Fatal("opens weren't limited")
	}
	// Guessing ids from one socket: refused after a few
	b := linkDial(t, hs)
	got := ""
	for i := 0; i < linkSessionMax+2; i++ {
		b.send(map[string]any{"t": "link-join", "id": "ZZZ", "m": "x"})
		m := b.next("link-error")
		got, _ = m["why"].(string)
	}
	if got != "rate" {
		t.Fatalf("joins weren't limited: %s", got)
	}
	// Sockets at once from one address
	var last error
	for i := 0; i < linkPerIP+2; i++ {
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+"/v1/link", nil)
		if err != nil {
			last = err
			break
		}
		t.Cleanup(func() { _ = ws.Close() })
	}
	if last == nil {
		t.Fatal("sockets weren't limited")
	}
}

// The relay needs no sign-in, keeps nothing and doesn't touch the database: an opener that goes takes its rendezvous.
func TestLinkNoSignInNothingKept(t *testing.T) {
	s, hs := newTestServer(t, nil)
	a := linkDial(t, hs)
	a.open()
	_ = a.ws.Close()
	time.Sleep(100 * time.Millisecond)
	s.link.mu.Lock()
	n := len(s.link.open)
	s.link.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d rendezvous kept after the opener left", n)
	}
	var members int
	if err := s.store.db.QueryRow(`SELECT COUNT(*) FROM members`).Scan(&members); err == nil && members != 0 {
		t.Fatalf("%d members made by linking", members)
	}
}
