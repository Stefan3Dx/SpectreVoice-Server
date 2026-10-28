package main

import (
	"crypto/x509"
	"testing"
)

// A reconnecting app lands back in its channel (hello "ch") when it could join it now, and catches up on the chat
// with history "after" the last id it has; "around" gives the page about one message.
func TestResumeChannelAndHistoryAfter(t *testing.T) {
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	_, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	a, _ := dialV(t, hs, ownerKey, "Ana", "")
	lobby := chanID(t, a.wel, "Lobby")
	a.send(map[string]any{"t": "ch-save", "name": "Second"})
	second := int64(a.wait(isT("channel"))["c"].(map[string]any)["id"].(float64))
	a.send(map[string]any{"t": "ch-save", "name": "Secret", "password": "pw"})
	secret := int64(a.wait(isT("channel"))["c"].(map[string]any)["id"].(float64))

	bKey := newKey(t)
	b, _ := dialVWith(t, hs, bKey, "Bogdan", map[string]any{"dev": "dev-b"})
	b.send(map[string]any{"t": "join", "ch": second})
	b.wait(func(m map[string]any) bool {
		return m["t"] == "member" && m["m"].(map[string]any)["uid"] == b.uid && int64(m["m"].(map[string]any)["ch"].(float64)) == second
	})

	// Chat while "away": five lines in Second, the app has the first two
	var ids []int64
	for i := 0; i < 5; i++ {
		a.send(map[string]any{"t": "say", "to": "c:" + itoa(int(second)), "text": "m" + itoa(i)})
		m := b.wait(isT("msg"))
		ids = append(ids, int64(m["m"].(map[string]any)["id"].(float64)))
	}

	// Back with the same device and the channel: straight into Second
	b2, _ := dialVWith(t, hs, bKey, "Bogdan", map[string]any{"dev": "dev-b", "ch": second})
	if b2 == nil {
		t.Fatal("no welcome")
	}
	me := memberOf(b2.wel, b2.uid)
	if me == nil || int64(me["ch"].(float64)) != second {
		t.Fatalf("not back in Second: %v", me)
	}
	// Only what's newer than the last id it has
	b2.send(map[string]any{"t": "history", "to": "c:" + itoa(int(second)), "after": ids[1]})
	h := b2.wait(isT("history"))
	items := h["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["text"] != "m2" || items[2].(map[string]any)["text"] != "m4" || h["more"] != false || int64(h["after"].(float64)) != ids[1] {
		t.Fatal(h)
	}
	// Around the middle one: all five, nothing beyond
	b2.send(map[string]any{"t": "history", "to": "c:" + itoa(int(second)), "around": ids[2]})
	h = b2.wait(isT("history"))
	items = h["items"].([]any)
	if len(items) != 5 || items[2].(map[string]any)["text"] != "m2" || h["more"] != false || h["moreAfter"] != false || int64(h["around"].(float64)) != ids[2] {
		t.Fatal(h)
	}
	// A page before the newest: only the last two around it (older exist)
	b2.send(map[string]any{"t": "history", "to": "c:" + itoa(int(second)), "around": ids[4]})
	if h = b2.wait(isT("history")); len(h["items"].([]any)) != 5 || h["more"] != false {
		t.Fatal(h)
	}

	// A locked channel can't be resumed without its password: the default one instead
	b3, _ := dialVWith(t, hs, bKey, "Bogdan", map[string]any{"dev": "dev-b", "ch": secret})
	if me = memberOf(b3.wel, b3.uid); me == nil || int64(me["ch"].(float64)) != lobby {
		t.Fatalf("resumed into a locked channel: %v", me)
	}
	// Nor a text channel, nor one of another community
	a.send(map[string]any{"t": "ch-save", "name": "news", "kind": "text"})
	text := int64(a.wait(isT("channel"))["c"].(map[string]any)["id"].(float64))
	b4, _ := dialVWith(t, hs, bKey, "Bogdan", map[string]any{"dev": "dev-b", "ch": text})
	if me = memberOf(b4.wel, b4.uid); me == nil || int64(me["ch"].(float64)) != lobby {
		t.Fatalf("resumed into a text channel: %v", me)
	}
	b5, _ := dialVWith(t, hs, bKey, "Bogdan", map[string]any{"dev": "dev-b", "ch": 99999})
	if me = memberOf(b5.wel, b5.uid); me == nil || int64(me["ch"].(float64)) != lobby {
		t.Fatalf("resumed into no channel: %v", me)
	}
}

func memberOf(wel map[string]any, uid string) map[string]any {
	for _, m := range wel["members"].([]any) {
		if mm := m.(map[string]any); mm["uid"] == uid {
			return mm
		}
	}
	return nil
}
