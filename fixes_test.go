package main

// Regression tests for the debugging session's fixes (DEBUGGING.md in the apps' repo lists them).

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A new subchannel (no id yet) under a top-level channel is allowed (it was refused as "bad parent").
func TestNewSubchannel(t *testing.T) {
	anaKey, own := ownerKeyCfg(t)
	_, hs := newTestServer(t, own)
	ana, _ := dialV(t, hs, anaKey, "Ana", "")
	lobby := chanID(t, ana.wel, "Lobby")
	ana.send(map[string]any{"t": "ch-save", "name": "Sub", "parent": lobby})
	c := ana.wait(func(m map[string]any) bool { return m["t"] == "channel" || m["t"] == "error" })
	if c["t"] != "channel" || int64(c["c"].(map[string]any)["parent"].(float64)) != lobby {
		t.Fatal(c)
	}
}

// The space's owner (all its permissions) is no server admin: no wiping people server-wide, no reports.
func TestCommunityOwnerCantWipe(t *testing.T) {
	s, hs := newTestServer(t, nil)
	t.Cleanup(s.bus.conns.Wait)
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	_, _ = s.store.db.Exec(`UPDATE communities SET owner=? WHERE id=1`, ana.uid)
	s.hub.mu.Lock()
	s.hub.forget(spaceID)
	s.hub.mu.Unlock()
	u := "ws" + strings.TrimPrefix(hs.URL, "http") + "/bus?space=" + mainSpace + "&token=" + url.QueryEscape(s.makeToken(ana.uid))
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	read := func() map[string]any {
		var m map[string]any
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if err := c.ReadJSON(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if st := read(); st["t"] != "state" || st["admin"] != true {
		t.Fatalf("the owner manages its own space: %v", st)
	}
	for _, op := range []map[string]any{{"t": "wipe", "uid": "victimvictimvictim01"}, {"t": "reports"}} {
		_ = c.WriteJSON(op)
		for {
			m := read()
			if m["t"] == "error" {
				break
			}
			if m["t"] == "wiped" || m["t"] == "reports" {
				t.Fatalf("%v went through: %v", op["t"], m)
			}
		}
	}
}

// "!play" typed in a text channel puts the bot in the sender's voice channel, not in the text channel.
func TestBotGoesToVoice(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.music = true; c.ytdlp = "true"; c.ffmpeg = "true" })
	if !s.music.enabled() {
		t.Skip("music off")
	}
	s.music.resolveFn = func(q string) (song, error) { return song{Title: "Ton", URL: "x"}, nil }
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	general, lobby := chanID(t, ana.wel, "general"), chanID(t, ana.wel, "Lobby")
	ana.send(map[string]any{"t": "say", "to": "c:" + itoa(int(general)), "text": "!play ceva"})
	m := ana.wait(func(m map[string]any) bool { return m["t"] == "music" && m["ch"] != nil })
	if int64(m["ch"].(float64)) != lobby {
		t.Fatalf("bot in %v, Ana in %d", m["ch"], lobby)
	}
	s.music.stopIn(1)
}

// A text channel is never where people land, nor where they're moved.
func TestNobodyLandsInText(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.hub.mu.Lock()
	for _, c := range s.hub.chans {
		c.Default = false
	}
	def := s.hub.defaultChannel(spaceID)
	kind := s.hub.chans[def].Kind
	s.hub.mu.Unlock()
	if def == 0 || kind != "" {
		t.Fatalf("default channel %d is %q", def, kind)
	}
}

// Wiping someone takes them out of everyone's records too: daily counters, weekly winners.
func TestWipeScrubsCounters(t *testing.T) {
	st := map[string]map[string]json.RawMessage{
		"scores": {
			"_day:2026-10-27": json.RawMessage(`{"t":{"U1":60,"U2":30},"v":{"U1":5},"n":{"U1":"Ana","U2":"Bob"}}`),
			"_hist:2026-43":   json.RawMessage(`{"CHESS":{"u":"U1","n":"Ana","s":3},"POKER":{"u":"U2","n":"Bob","s":1}}`),
		},
		"donors": {"U1": json.RawMessage(`{"n":"Ana","m":"pentru voi","a":5}`)},
	}
	wipeState(st, "U1")
	day, hist, don := string(st["scores"]["_day:2026-10-27"]), string(st["scores"]["_hist:2026-43"]), string(st["donors"]["U1"])
	if strings.Contains(day, "U1") || strings.Contains(day, "Ana") || !strings.Contains(day, "U2") {
		t.Fatal(day)
	}
	if strings.Contains(hist, "U1") || !strings.Contains(hist, "U2") {
		t.Fatal(hist)
	}
	if strings.Contains(don, "Ana") || strings.Contains(don, "pentru") {
		t.Fatal(don)
	}
}

// A quiet "play" (the guessing game) keeps the song's title out of the bot's status and chat lines.
func TestQuietPlayHidesTitle(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.music = true; c.ytdlp = "true"; c.ffmpeg = "true" })
	if !s.music.enabled() {
		t.Skip("music off")
	}
	s.music.resolveFn = func(q string) (song, error) {
		return song{Title: "Secret Song", Page: "https://x/secret", URL: "x"}, nil
	}
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	ana.send(map[string]any{"t": "music", "cmd": "play", "q": "secret", "quiet": true})
	m := ana.wait(func(m map[string]any) bool { return m["t"] == "music" && m["now"] != nil })
	now := m["now"].(map[string]any)
	if now["title"] != hiddenTitle || now["page"] != nil {
		t.Fatalf("the title leaked: %v", now)
	}
	// The chat line too
	line := ana.wait(func(m map[string]any) bool {
		if m["t"] != "msg" {
			return false
		}
		text, _ := m["m"].(map[string]any)["text"].(string)
		return strings.HasPrefix(text, "▶")
	})
	if text, _ := line["m"].(map[string]any)["text"].(string); strings.Contains(text, "Secret") {
		t.Fatalf("the title in chat: %s", text)
	}
	s.music.stopIn(1)
}

// The music bot's yt-dlp and ffmpeg get our proxy (public addresses only) and none of the server's settings.
func TestBotEnv(t *testing.T) {
	t.Setenv("SV_TURN_SECRET", "x")
	t.Setenv("HTTPS_PROXY", "http://zz-other-proxy.invalid:1")
	env := strings.Join(botEnv("http://sv:p@127.0.0.1:9"), "\n")
	if strings.Contains(env, "SV_TURN_SECRET") || strings.Contains(env, "zz-other-proxy.invalid") || !strings.Contains(env, "https_proxy=http://sv:p@127.0.0.1:9") {
		t.Fatal(env)
	}
	mb := &musicBots{}
	u, err := mb.proxyURL()
	if err != nil || !strings.HasPrefix(u, "http://sv:") {
		t.Fatal(u, err)
	}
	mb.proxy.close()
}
