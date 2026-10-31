package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func isP2P(t string, from int) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["t"] == t && int(m["from"].(float64)) == from }
}

func liveOn(room string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		s, _ := m["s"].(map[string]any)
		return m["t"] == "live" && m["on"] == true && s["room"] == room
	}
}

func liveOff(room string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		s, _ := m["s"].(map[string]any)
		return m["t"] == "live" && m["on"] == false && s["room"] == room
	}
}

// quiet: nothing matching comes within d.
func (c *tclient) quiet(d time.Duration, match func(map[string]any) bool) bool {
	for _, m := range c.seen {
		if match(m) {
			return false
		}
	}
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-c.in:
			if !ok {
				return true
			}
			if match(m) {
				return false
			}
			c.seen = append(c.seen, m)
		case <-deadline:
			return true
		}
	}
}

func p2pStatus(t *testing.T, hs string, tok, room string) message {
	t.Helper()
	res, err := http.Get(hs + "/streams/status?room=" + url.QueryEscape(room) + "&token=" + url.QueryEscape(tok))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m message
	_ = json.NewDecoder(res.Body).Decode(&m)
	return m
}

// The signalling of a P2P stream: announced with its mode and the streamer's session, passed on only between a
// streamer and a viewer that joined, nothing through the SFU, and cleaned up when either leaves.
func TestP2PSignalling(t *testing.T) {
	_, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	cara, _ := dialV(t, hs, newKey(t), "Cara", "")
	room := ana.uid
	sfuBefore := statStreamIn.Load()

	// Nothing live yet: a join is answered with a bye
	bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	if m := bob.wait(isT("p2p-bye")); m["why"] != "ended" {
		t.Fatal(m)
	}
	// Only as oneself
	ana.send(map[string]any{"t": "p2p-live", "room": bob.uid, "on": true})
	if e := ana.wait(isT("error")); e["op"] != "p2p-live" || e["why"] != "bad" {
		t.Fatal(e)
	}
	ana.send(map[string]any{"t": "p2p-live", "room": room, "on": true, "title": "Game"})
	lv := bob.wait(liveOn(room))
	s := lv["s"].(map[string]any)
	if s["mode"] != "p2p" || int(s["sid"].(float64)) != ana.sid || s["uid"] != ana.uid || s["title"] != "Game" {
		t.Fatal(lv)
	}
	cara.wait(liveOn(room))
	// A newcomer finds it in the live list of its welcome
	dan, _ := dialV(t, hs, newKey(t), "Dan", "")
	found := false
	for _, l := range dan.wel["live"].([]any) {
		if lm := l.(map[string]any); lm["room"] == room && lm["mode"] == "p2p" {
			found = true
		}
	}
	if !found {
		t.Fatal("not in the welcome's live list", dan.wel["live"])
	}

	// Before joining, nothing passes: a viewer can't answer, nobody but the streamer offers
	bob.send(map[string]any{"t": "p2p-answer", "sid": ana.sid, "room": room, "sdp": "v=0"})
	if m := bob.wait(isT("p2p-bye")); m["why"] != "gone" {
		t.Fatal(m)
	}
	cara.send(map[string]any{"t": "p2p-offer", "sid": bob.sid, "room": room, "sdp": "v=0"})
	if m := cara.wait(isT("p2p-bye")); m["why"] != "gone" {
		t.Fatal(m)
	}
	if !bob.quiet(300*time.Millisecond, isT("p2p-offer")) {
		t.Fatal("an offer from someone who isn't the streamer got through")
	}

	// Bob joins: Ana is told who; she offers, he answers, candidates both ways
	bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	j := ana.wait(isP2P("p2p-join", bob.sid))
	if j["uid"] != bob.uid || j["name"] != "Bob" || j["room"] != room {
		t.Fatal(j)
	}
	ana.send(map[string]any{"t": "p2p-offer", "sid": bob.sid, "room": room, "sdp": "v=0 offer"})
	if o := bob.wait(isP2P("p2p-offer", ana.sid)); o["sdp"] != "v=0 offer" || o["room"] != room || o["sid"] != nil {
		t.Fatal(o)
	}
	// The streamer can't offer to someone who didn't join
	ana.send(map[string]any{"t": "p2p-offer", "sid": cara.sid, "room": room, "sdp": "v=0"})
	ana.wait(isP2P("p2p-bye", cara.sid))
	if !cara.quiet(300*time.Millisecond, isT("p2p-offer")) {
		t.Fatal("offered to a session that never joined")
	}
	// Too big a description
	ana.send(map[string]any{"t": "p2p-offer", "sid": bob.sid, "room": room, "sdp": strings.Repeat("a", p2pMaxSDP+1)})
	if e := ana.wait(isT("error")); e["op"] != "p2p-offer" || e["why"] != "bad" {
		t.Fatal(e)
	}
	bob.send(map[string]any{"t": "p2p-answer", "sid": ana.sid, "room": room, "sdp": "v=0 answer"})
	if a := ana.wait(isP2P("p2p-answer", bob.sid)); a["sdp"] != "v=0 answer" {
		t.Fatal(a)
	}
	bob.send(map[string]any{"t": "p2p-ice", "sid": ana.sid, "room": room, "candidate": "candidate:1 1 udp 1 10.0.0.2 5000 typ host", "mid": "0", "mline": 0})
	if c := ana.wait(isP2P("p2p-ice", bob.sid)); c["mid"] != "0" || c["mline"] != float64(0) || !strings.HasPrefix(c["candidate"].(string), "candidate:") {
		t.Fatal(c)
	}
	ana.send(map[string]any{"t": "p2p-ice", "sid": bob.sid, "room": room, "candidate": "candidate:2 1 udp 1 10.0.0.1 5001 typ host", "mid": "1", "mline": 1})
	bob.wait(isP2P("p2p-ice", ana.sid))
	// A candidate too long is dropped
	ana.send(map[string]any{"t": "p2p-ice", "sid": bob.sid, "room": room, "candidate": strings.Repeat("c", maxCandLen+1)})
	if !bob.quiet(300*time.Millisecond, isT("p2p-ice")) {
		t.Fatal("an oversized candidate got through")
	}

	// The SFU carries nothing of it; the status says P2P with one viewer
	st := p2pStatus(t, hs.URL, bob.tok, room)
	if st.Live == nil || *st.Live || st.Mode != "p2p" || st.P2P == nil || *st.P2P != 1 || st.Bytes == nil || *st.Bytes != 0 || st.SFUBytes == nil || *st.SFUBytes != sfuBefore {
		t.Fatalf("%+v", st)
	}

	// Cara joins too; Bob leaves the server: Ana is told
	cara.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	ana.wait(isP2P("p2p-join", cara.sid))
	_ = bob.ws.Close()
	if m := ana.wait(isP2P("p2p-bye", bob.sid)); m["why"] != "left" {
		t.Fatal(m)
	}
	// A viewer's bye ends the pair: after it nothing more passes
	cara.send(map[string]any{"t": "p2p-bye", "sid": ana.sid, "room": room, "why": "stop"})
	ana.wait(isP2P("p2p-bye", cara.sid))
	ana.send(map[string]any{"t": "p2p-offer", "sid": cara.sid, "room": room, "sdp": "v=0"})
	ana.wait(isP2P("p2p-bye", cara.sid))
	cara.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	ana.wait(isP2P("p2p-join", cara.sid))

	// Ana leaves: her viewer is told, and the stream goes off the list
	_ = ana.ws.Close()
	if m := cara.wait(isP2P("p2p-bye", ana.sid)); m["why"] != "ended" {
		t.Fatal(m)
	}
	cara.wait(liveOff(room))
	if st := p2pStatus(t, hs.URL, cara.tok, room); st.Mode != "" {
		t.Fatalf("%+v", st)
	}
}

// A full stream says so; a second session of the same person can't take a live room over; at most two a person.
func TestP2PLimits(t *testing.T) {
	s, hs := newTestServer(t, nil)
	old := p2pMaxViewers
	p2pMaxViewers = 1
	t.Cleanup(func() { p2pMaxViewers = old })
	key := newKey(t)
	ana, _ := dialV(t, hs, key, "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	cara, _ := dialV(t, hs, newKey(t), "Cara", "")
	room := ana.uid
	ana.send(map[string]any{"t": "p2p-live", "room": room, "on": true})
	bob.wait(liveOn(room))
	bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	ana.wait(isP2P("p2p-join", bob.sid))
	cara.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	if m := cara.wait(isT("p2p-bye")); m["why"] != "full" {
		t.Fatal(m)
	}
	// Joining again is fine (a reconnect), it doesn't count twice
	bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	ana.wait(isP2P("p2p-join", bob.sid))
	// Her phone can't take the room over while the PC streams it
	phone, _ := dialV(t, hs, key, "Ana", "")
	phone.send(map[string]any{"t": "p2p-live", "room": room, "on": true})
	if e := phone.wait(isT("error")); e["why"] != "taken" {
		t.Fatal(e)
	}
	// Two streams a person
	ana.send(map[string]any{"t": "p2p-live", "room": room + ":camera", "on": true})
	bob.wait(liveOn(room + ":camera"))
	ana.send(map[string]any{"t": "p2p-live", "room": room + ":third", "on": true})
	if e := ana.wait(isT("error")); e["why"] != "full" {
		t.Fatal(e)
	}
	// Nor a third through the SFU
	if _, err := s.openRoom(room+":sfu", ana.uid, nil); err == nil {
		t.Fatal("a third stream through the SFU")
	}
	// Changing the title keeps the stream (no second one)
	ana.send(map[string]any{"t": "p2p-live", "room": room, "on": true, "title": "New"})
	if lv := bob.wait(liveOn(room)); lv["s"].(map[string]any)["title"] != "New" {
		t.Fatal(lv)
	}
	// Ending one by hand
	ana.send(map[string]any{"t": "p2p-live", "room": room + ":camera", "on": false})
	bob.wait(liveOff(room + ":camera"))
	// Someone else can't end it
	cara.send(map[string]any{"t": "p2p-live", "room": room, "on": false})
	if !bob.quiet(300*time.Millisecond, liveOff(room)) {
		t.Fatal("ended by someone else")
	}
}

// Signalling has a budget: a flood of candidates, or of joins, is refused.
func TestP2PRate(t *testing.T) {
	_, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	ana.send(map[string]any{"t": "p2p-live", "room": ana.uid, "on": true})
	bob.wait(liveOn(ana.uid))
	for i := 0; i < 8; i++ {
		bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": ana.uid})
	}
	if e := bob.wait(isT("error")); e["op"] != "p2p-join" || e["why"] != "rate" {
		t.Fatal(e)
	}
	cara, _ := dialV(t, hs, newKey(t), "Cara", "")
	for i := 0; i < int(p2pBurst)+20; i++ {
		cara.send(map[string]any{"t": "p2p-ice", "sid": ana.sid, "room": ana.uid, "candidate": "candidate:x"})
	}
	if e := cara.wait(isT("error")); e["op"] != "p2p-ice" || e["why"] != "rate" {
		t.Fatal(e)
	}
}

// Only those who may watch (mayWatch: the stream's space, bans): a join from someone who may not is refused.
func TestP2PVisibility(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	ana.send(map[string]any{"t": "p2p-live", "room": ana.uid, "on": true})
	bob.wait(liveOn(ana.uid))
	// The stream as if it were somewhere Bob isn't a member of
	s.hub.mu.Lock()
	ls := s.hub.live[ana.uid]
	ls.Comm = 999
	s.hub.live[ana.uid] = ls
	s.hub.mu.Unlock()
	bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": ana.uid})
	if m := bob.wait(isT("p2p-bye")); m["why"] != "ended" {
		t.Fatal(m)
	}
	if !ana.quiet(300*time.Millisecond, isT("p2p-join")) {
		t.Fatal("a join from someone who may not watch got through")
	}
}

// Switching while live: the SFU's end doesn't take a stream that moved to P2P off the list, and a P2P stream that
// moved back to the SFU tells its viewers "moved" instead of "ended".
func TestP2PSwitchWithSFU(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	room := ana.uid
	comm, _ := s.hub.streamComm(ana.uid)
	s.hub.setLive(room, ana.uid, "Game", "", comm, true) // as /streams does on publish
	if lv := bob.wait(liveOn(room)); lv["s"].(map[string]any)["mode"] != nil {
		t.Fatal(lv)
	}
	// Over to P2P, then the SFU stream ends
	ana.send(map[string]any{"t": "p2p-live", "room": room, "on": true, "title": "Game"})
	if lv := bob.wait(liveOn(room)); lv["s"].(map[string]any)["mode"] != "p2p" {
		t.Fatal(lv)
	}
	s.hub.setLive(room, "", "", "", 0, false)
	if !bob.quiet(400*time.Millisecond, liveOff(room)) {
		t.Fatal("the SFU's end took the P2P stream off")
	}
	bob.send(map[string]any{"t": "p2p-join", "sid": ana.sid, "room": room})
	ana.wait(isP2P("p2p-join", bob.sid))
	// Back to the SFU: published again, then P2P off
	s.hub.setLive(room, ana.uid, "Game", "", comm, true)
	bob.wait(liveOn(room))
	ana.send(map[string]any{"t": "p2p-live", "room": room, "on": false})
	if m := bob.wait(isP2P("p2p-bye", ana.sid)); m["why"] != "moved" {
		t.Fatal(m)
	}
	if !bob.quiet(400*time.Millisecond, liveOff(room)) {
		t.Fatal("the SFU stream went off the list")
	}
}
