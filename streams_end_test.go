package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

// fakeRoom puts a stream room in place as /streams would, without WebRTC; live: told live too.
func fakeRoom(s *server, name, uid string, live bool) *room {
	rm := &room{name: name, subs: map[*subscriber]bool{}, stop: make(chan struct{})}
	s.mu.Lock()
	s.rooms[name] = rm
	s.mu.Unlock()
	if live {
		s.hub.setLive(name, uid, "", "", spaceID, true)
	}
	return rm
}

func roomUp(s *server, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rooms[name] != nil
}

func liveUp(s *server, name string) bool {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	_, ok := s.hub.live[name]
	return ok
}

// waitGone waits for a room to end (the role path ends it apart).
func waitGone(t *testing.T, s *server, name string) {
	t.Helper()
	for range 100 {
		if !roomUp(s, name) && !liveUp(s, name) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s still live", name)
}

// ownerOf signs in Ana, the server's owner.
func ownerOf(t *testing.T, s *server, hs *httptest.Server) *tclient {
	t.Helper()
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	_ = s.store.setRole(ana.uid, roleOwner)
	s.hub.mu.Lock()
	s.hub.forget(spaceID)
	s.hub.mu.Unlock()
	return ana
}

func TestEndStreamsOf(t *testing.T) {
	s, _ := newTestServer(t, nil)
	call := "U" + callRoomPart + "x"
	fakeRoom(s, "U", "U", true)
	fakeRoom(s, "U:cam", "U", true)
	fakeRoom(s, call, "U", false)
	fakeRoom(s, "U2", "U2", true)
	tv := &room{name: tvRoom, subs: map[*subscriber]bool{}, local: true}
	s.mu.Lock()
	s.rooms[tvRoom] = tv
	s.mu.Unlock()
	s.endStreamsOf("U", "", 0, false)
	if roomUp(s, "U") || roomUp(s, "U:cam") || liveUp(s, "U") || liveUp(s, "U:cam") {
		t.Fatal("streams still up")
	}
	if !roomUp(s, call) || !roomUp(s, tvRoom) || !roomUp(s, "U2") || !liveUp(s, "U2") {
		t.Fatal("ended more than U's streams")
	}
	s.endStreamsOf("U", "", 0, true)
	if roomUp(s, call) || !roomUp(s, tvRoom) || !roomUp(s, "U2") {
		t.Fatal("the call's room not ended, or the TV / another's ended")
	}
}

// A streamer banned mid-stream is off the air: their stream socket is apart from the session the ban closes.
func TestBanEndsStream(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana := ownerOf(t, s, hs)
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	fakeRoom(s, bob.uid, bob.uid, true)
	ana.send(map[string]any{"t": "ban", "uid": bob.uid})
	ana.wait(isDone("ban"))
	waitGone(t, s, bob.uid)
}

// A kick ends only the kicked device's stream.
func TestKickEndsThatDevicesStream(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana := ownerOf(t, s, hs)
	key := newKey(t)
	pc, _ := dialVWith(t, hs, key, "Bob", map[string]any{"dev": "pc"})
	phone, _ := dialVWith(t, hs, key, "Bob", map[string]any{"dev": "phone"})
	if pc == nil || phone == nil {
		t.Fatal("not in")
	}
	fakeRoom(s, pc.uid, pc.uid, false)
	s.hub.setLive(pc.uid, pc.uid, "", "pc", spaceID, true)
	ana.send(map[string]any{"t": "kick", "sid": float64(phone.sid)})
	phone.wait(isT("kicked"))
	time.Sleep(200 * time.Millisecond)
	if !roomUp(s, pc.uid) || !liveUp(s, pc.uid) {
		t.Fatal("the phone's kick ended the PC's stream")
	}
	ana.send(map[string]any{"t": "kick", "sid": float64(pc.sid)})
	pc.wait(isT("kicked"))
	waitGone(t, s, pc.uid)
}

// Losing the stream permission (a role change) ends the stream.
func TestRoleChangeEndsStream(t *testing.T) {
	s, hs := newTestServer(t, nil)
	ana := ownerOf(t, s, hs)
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	fakeRoom(s, bob.uid, bob.uid, true)
	fakeRoom(s, ana.uid, ana.uid, true)
	roles, err := s.store.rolesOf(spaceID)
	if err != nil {
		t.Fatal(err)
	}
	var every int64
	for _, r := range roles {
		if r.Everyone {
			every = r.ID
		}
	}
	if every == 0 {
		t.Fatal("no everyone role")
	}
	ana.send(map[string]any{"t": "role-save", "id": float64(every), "perms": []string{"speak", "invite"}})
	ana.wait(isT("roles"))
	waitGone(t, s, bob.uid)
	if !roomUp(s, ana.uid) {
		t.Fatal("the owner's stream ended")
	}
}
