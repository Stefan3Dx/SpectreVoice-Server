package main

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestTVLogosAccounting: the logo cache counts only the logos it keeps. A logo still downloading when it's pushed
// out (a long channel list scrolled through) or when the playlist changes (reset) was added to the byte count once
// it came, and never taken off: the count grew for good, every new logo then emptied the cache, and once it was
// over the limit with nothing left to push out, the next logo panicked with the cache's lock held (every logo
// request after that hung).
func TestTVLogosAccounting(t *testing.T) {
	tvLoopbackOK = true
	defer func() { tvLoopbackOK = false }()
	var pic bytes.Buffer
	_ = png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 64, 64)))
	release := make(chan struct{})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("slow") == "1" {
			<-release
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pic.Bytes())
	}))
	defer hs.Close()
	l := newTVLogos(false)

	// A logo on its way when the playlist changes: once it comes, it isn't counted (nor kept)
	slow := hs.URL + "/a.png?slow=1"
	got := make(chan bool, 1)
	go func() { _, _, ok := l.get(slow); got <- ok }()
	time.Sleep(200 * time.Millisecond)
	l.reset()
	close(release)
	<-got
	time.Sleep(100 * time.Millisecond)
	l.mu.Lock()
	n, kept := l.bytes, len(l.m)
	l.mu.Unlock()
	if n != 0 || kept != 0 {
		t.Fatalf("after a reset, a late logo is counted: %d bytes, %d kept", n, kept)
	}

	// A logo comes and is counted
	if _, _, ok := l.get(hs.URL + "/b.png"); !ok {
		t.Fatal("logo not fetched")
	}
	l.mu.Lock()
	if l.bytes != pic.Len() {
		t.Errorf("bytes %d, want %d", l.bytes, pic.Len())
	}
	// Counted over the limit with nothing to push out: a new logo mustn't panic (nor leave the lock held)
	l.bytes = tvLogoBytes + 1
	l.order = nil
	l.m = map[string]*tvLogo{}
	l.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panicked: %v", r)
			}
		}()
		l.get(hs.URL + "/c.png?n=" + strconv.Itoa(1))
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("get hung")
	}
}
