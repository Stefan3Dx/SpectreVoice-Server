package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// A viewer that came while only some of a share's layers had arrived is told the whole list once the others come
// (the browser's quality picker stayed hidden, or short of a size, until the layer it got changed).
func TestTellLayerNewLayers(t *testing.T) {
	up := websocket.Upgrader{}
	got := make(chan message, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			var m message
			if c.ReadJSON(&m) != nil {
				return
			}
			got <- m
		}
	}))
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", "v")
	if err != nil {
		t.Fatal(err)
	}
	sim := &simulcast{layers: map[string]*layer{"l": {rid: "l"}}, viewers: map[*subscriber]*switcher{}}
	rm := &room{sim: sim}
	sub := &subscriber{c: &conn{ws: ws}, asked: "auto"}
	sub.sw.Store(newSwitcher(track, webrtc.MimeTypeVP8))
	next := func() message {
		t.Helper()
		select {
		case m := <-got:
			return m
		case <-time.After(3 * time.Second):
			t.Fatal("not told")
		}
		return message{}
	}
	rm.tellLayer(sub, false)
	if m := next(); len(m.Layers) != 1 {
		t.Fatalf("first: %v", m.Layers)
	}
	sim.mu.Lock()
	sim.layers["m"], sim.layers["h"] = &layer{rid: "m"}, &layer{rid: "h"}
	sim.mu.Unlock()
	rm.tellLayer(sub, false)
	if m := next(); len(m.Layers) != 3 {
		t.Fatalf("after the other layers came: %v", m.Layers)
	}
	rm.tellLayer(sub, false) // nothing changed: nothing said
	select {
	case m := <-got:
		t.Fatalf("told again: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}
