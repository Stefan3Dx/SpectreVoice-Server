package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// testWav: a mono 16-bit WAV of a tone, [sec] seconds at 44.1 kHz.
func testWav(sec float64) []byte {
	const rate = 44100
	n := int(sec * rate)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+n*2))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(n*2))
	for i := 0; i < n; i++ {
		_ = binary.Write(&b, binary.LittleEndian, int16(8000*math.Sin(2*math.Pi*440*float64(i)/rate)))
	}
	return b.Bytes()
}

func sbUpload(t *testing.T, base, token, name, emoji string, body []byte) (int, map[string]any) {
	t.Helper()
	u := base + "/sb/upload?token=" + url.QueryEscape(token) + "&name=" + url.QueryEscape(name) + "&emoji=" + url.QueryEscape(emoji)
	res, err := http.Post(u, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

func isSB(m map[string]any) bool { return m["t"] == "sb" }

func isSBErr(m map[string]any) bool { return m["t"] == "error" && m["op"] == "sb" }

func sbItems(m map[string]any) []any {
	if v, ok := m["items"].([]any); ok {
		return v
	}
	return nil
}

func TestSoundboardHelpers(t *testing.T) {
	for in, want := range map[string]string{"  Air   horn \n": "Air horn", "": "Sound", strings.Repeat("x", 40): strings.Repeat("x", 32), "a\x00b": "a b"} {
		if got := sbName(in, "Sound"); got != want {
			t.Errorf("sbName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"📯": "📯", "👍🏽": "👍🏽", "abc": "🔊", "": "🔊", "<script>": "🔊", "🔥 x": "🔊", strings.Repeat("😀", 9): "🔊"} {
		if got := sbEmoji(in, "🔊"); got != want {
			t.Errorf("sbEmoji(%q) = %q, want %q", in, got, want)
		}
	}
	if sbFormat([]byte("OggS\x00\x02")) != "ogg" || sbFormat(testWav(0.01)) != "wav" || sbFormat([]byte("ID3\x04")) != "mp3" || sbFormat([]byte{0xFF, 0xFB, 0x90}) != "mp3" || sbFormat([]byte("<html>")) != "" {
		t.Fatal("sbFormat")
	}
	if oggOpusMillis([]byte("not ogg at all, no")) != 0 {
		t.Fatal("oggOpusMillis of junk")
	}
	// The limits: a cool-down per person, a minute's count, a channel's gap
	sb := &soundboard{uses: map[string]*sbUse{}, chLast: map[int64]time.Time{}}
	now := time.Unix(1000, 0)
	if why, _ := sb.take("a", 1, now); why != "" {
		t.Fatal(why)
	}
	if why, wait := sb.take("a", 1, now.Add(time.Second)); why != "cooldown" || wait != 2*time.Second {
		t.Fatalf("cooldown: %q %v", why, wait)
	}
	if why, _ := sb.take("b", 1, now.Add(100*time.Millisecond)); why != "busy" {
		t.Fatalf("channel gap: %q", why)
	}
	if why, _ := sb.take("b", 2, now.Add(100*time.Millisecond)); why != "" {
		t.Fatalf("another channel: %q", why)
	}
	at := now
	for i := 1; i < sbPerMinute; i++ {
		at = at.Add(sbCooldown)
		if why, _ := sb.take("a", 3, at); why != "" {
			t.Fatalf("play %d: %q", i, why)
		}
	}
	if why, _ := sb.take("a", 3, at.Add(sbCooldown)); why != "rate" {
		t.Fatalf("minute: %q", why)
	}
}

func TestSoundboard(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	adminKey := newKey(t)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOfKey(adminKey); c.soundsMax = 2 })
	admin, _ := dialV(t, hs, adminKey, "Boss", "")
	user, _ := dialV(t, hs, newKey(t), "User", "")
	other, _ := dialV(t, hs, newKey(t), "Other", "")
	wsb, _ := user.wel["sb"].(map[string]any)
	if wsb == nil || len(sbItems(wsb)) != 0 || wsb["max"] != 2.0 {
		t.Fatalf("welcome sb: %v", user.wel["sb"])
	}
	// @everyone plays, only admins upload
	var perms []any
	for _, r := range user.wel["roles"].([]any) {
		if rm := r.(map[string]any); rm["everyone"] == true {
			perms = rm["perms"].([]any)
		}
	}
	if !strings.Contains(strings.Join(func() []string {
		var o []string
		for _, p := range perms {
			o = append(o, p.(string))
		}
		return o
	}(), ","), "soundboard") {
		t.Fatalf("@everyone: %v", perms)
	}
	if code, m := sbUpload(t, hs.URL, user.tok, "Mine", "😀", testWav(1)); code != http.StatusForbidden || m["why"] != "role" {
		t.Fatalf("user upload: %d %v", code, m)
	}
	if code, m := sbUpload(t, hs.URL, admin.tok, "Page", "📄", []byte("<html><body>hi</body></html>")); code != http.StatusUnsupportedMediaType {
		t.Fatalf("html upload: %d %v", code, m)
	}
	code, up := sbUpload(t, hs.URL, admin.tok, "  Air horn ", "📯", testWav(12))
	if code != 200 || up["cut"] != true {
		t.Fatalf("admin upload: %d %v", code, up)
	}
	if ms := up["ms"].(float64); ms < 9900 || ms > 10100 {
		t.Fatalf("ms %v", ms)
	}
	hash := up["hash"].(string)
	m := user.wait(func(m map[string]any) bool { return isSB(m) && len(sbItems(m)) == 1 })
	it := sbItems(m)[0].(map[string]any)
	if it["name"] != "Air horn" || it["emoji"] != "📯" || it["hash"] != hash || it["byName"] != "Boss" {
		t.Fatalf("item: %v", it)
	}
	id := it["id"].(float64)
	// The file, by its hash, for those on the server only
	res, err := http.Get(hs.URL + "/sb/f/" + hash + ".opus?token=" + url.QueryEscape(user.tok))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !bytes.HasPrefix(data, []byte("OggS")) || res.Header.Get("Content-Type") != "audio/ogg" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("file: %d %q %v", res.StatusCode, res.Header, len(data))
	}
	if oggOpusMillis(data) < 9900 {
		t.Fatalf("file length %d", oggOpusMillis(data))
	}
	for _, u := range []string{"/sb/f/" + hash + ".opus", "/sb/f/" + strings.Repeat("0", 64) + ".opus?token=" + url.QueryEscape(user.tok), "/sb/f/../spectre.db?token=" + url.QueryEscape(user.tok)} {
		if res, err := http.Get(hs.URL + u); err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				t.Fatalf("%s: 200", u)
			}
		}
	}
	// Everyone lands in the Lobby; the other goes to AFK
	lobby := chanID(t, user.wel, "Lobby")
	other.send(map[string]any{"t": "join", "ch": chanID(t, other.wel, "AFK")})
	other.wait(func(m map[string]any) bool {
		mm, _ := m["m"].(map[string]any)
		return m["t"] == "member" && mm["uid"] == other.uid && mm["ch"] == float64(chanID(t, other.wel, "AFK"))
	})
	user.send(map[string]any{"t": "sb", "op": "play", "id": id})
	for _, c := range []*tclient{user, admin} {
		p := c.wait(isT("sb-play"))
		if p["hash"] != hash || p["by"] != user.uid || p["byName"] != "User" || p["ch"] != float64(lobby) {
			t.Fatalf("sb-play: %v", p)
		}
	}
	// Again at once: the cool-down, with how long
	user.send(map[string]any{"t": "sb", "op": "play", "id": id})
	if m := user.wait(isSBErr); m["why"] != "cooldown" || m["wait"].(float64) <= 0 {
		t.Fatalf("cooldown: %v", m)
	}
	// The other channel heard nothing
	other.send(map[string]any{"t": "ping", "n": 7})
	other.wait(isT("pong"))
	for _, m := range other.seen {
		if m["t"] == "sb-play" {
			t.Fatalf("other channel heard: %v", m)
		}
	}
	// Without the permission
	s.hub.mu.Lock()
	_, _ = s.store.db.Exec(`UPDATE roles SET perms = perms & ? WHERE everyone=1`, int64(pAll&^pSoundboard))
	s.hub.forget(spaceID)
	s.hub.mu.Unlock()
	other.send(map[string]any{"t": "sb", "op": "play", "id": id})
	if m := other.wait(isSBErr); m["why"] != "role" {
		t.Fatalf("no permission: %v", m)
	}
	// Managing: only with soundboard_manage
	user.send(map[string]any{"t": "sb", "op": "edit", "id": id, "name": "x"})
	if m := user.wait(isSBErr); m["why"] != "role" {
		t.Fatalf("user edit: %v", m)
	}
	admin.send(map[string]any{"t": "sb", "op": "edit", "id": id, "name": "Horn", "emoji": "🎺"})
	m = user.wait(func(m map[string]any) bool {
		return isSB(m) && len(sbItems(m)) == 1 && sbItems(m)[0].(map[string]any)["name"] == "Horn"
	})
	if sbItems(m)[0].(map[string]any)["emoji"] != "🎺" {
		t.Fatalf("edit: %v", m)
	}
	// The limit on how many
	if code, _ := sbUpload(t, hs.URL, admin.tok, "Two", "", testWav(0.5)); code != 200 {
		t.Fatalf("second: %d", code)
	}
	if code, m := sbUpload(t, hs.URL, admin.tok, "Three", "", testWav(0.5)); code != http.StatusConflict || m["why"] != "full" {
		t.Fatalf("third: %d %v", code, m)
	}
	// A wipe keeps the sound, without the name
	if _, err := s.store.wipeUser(admin.uid); err != nil {
		t.Fatal(err)
	}
	s.sb.wiped()
	for _, x := range s.sb.items() {
		if x.By != "" || x.ByName != "" {
			t.Fatalf("after the wipe: %+v", x)
		}
	}
	// Removed: the file goes too
	admin.send(map[string]any{"t": "sb", "op": "del", "id": id})
	user.wait(func(m map[string]any) bool {
		return isSB(m) && len(sbItems(m)) == 1 && sbItems(m)[0].(map[string]any)["id"] != id
	})
	if _, err := os.Stat(s.sb.path(hash)); !os.IsNotExist(err) {
		t.Fatalf("file kept: %v", err)
	}
}

func TestSoundboardMigration(t *testing.T) {
	s, _ := newTestServer(t, nil)
	var every, admin int64
	_ = s.store.db.QueryRow(`SELECT perms FROM roles WHERE everyone=1`).Scan(&every)
	_ = s.store.db.QueryRow(`SELECT perms FROM roles WHERE name='Admin'`).Scan(&admin)
	if perm(every)&pSoundboard == 0 || perm(every)&pSoundboardManage != 0 || perm(admin)&pSoundboardManage == 0 {
		t.Fatalf("roles: everyone %b admin %b", every, admin)
	}
	// A server from before: its roles get them once
	_, _ = s.store.db.Exec(`UPDATE roles SET perms = perms & ?`, int64(pAll&^(pSoundboard|pSoundboardManage)))
	s.store.delSetting("perm_sb")
	if _, err := newSoundboard(s); err != nil {
		t.Fatal(err)
	}
	_ = s.store.db.QueryRow(`SELECT perms FROM roles WHERE everyone=1`).Scan(&every)
	_ = s.store.db.QueryRow(`SELECT perms FROM roles WHERE name='Admin'`).Scan(&admin)
	if perm(every)&pSoundboard == 0 || perm(admin)&pSoundboardManage == 0 {
		t.Fatalf("migrated: everyone %b admin %b", every, admin)
	}
	// Once: taken away after, it stays away
	_, _ = s.store.db.Exec(`UPDATE roles SET perms = perms & ? WHERE everyone=1`, int64(pAll&^pSoundboard))
	if _, err := newSoundboard(s); err != nil {
		t.Fatal(err)
	}
	_ = s.store.db.QueryRow(`SELECT perms FROM roles WHERE everyone=1`).Scan(&every)
	if perm(every)&pSoundboard != 0 {
		t.Fatal("given again")
	}
}

func TestSoundboardFormats(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	adminKey := newKey(t)
	_, hs := newTestServer(t, func(c *config) { c.owners = uidOfKey(adminKey) })
	admin, _ := dialV(t, hs, adminKey, "Boss", "")
	dir := t.TempDir()
	for _, f := range []struct{ name, codec string }{{"a.mp3", "libmp3lame"}, {"a.ogg", "libopus"}, {"b.ogg", "libvorbis"}} {
		out := dir + "/" + f.name
		if err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "sine=f=330:d=1.5", "-c:a", f.codec, out).Run(); err != nil {
			t.Logf("%s: no %s here", f.name, f.codec)
			continue
		}
		data, _ := os.ReadFile(out)
		code, m := sbUpload(t, hs.URL, admin.tok, f.name, "", data)
		if code != 200 || m["cut"] != false {
			t.Fatalf("%s: %d %v", f.name, code, m)
		}
		if ms := m["ms"].(float64); ms < 1400 || ms > 1600 {
			t.Fatalf("%s: %v ms", f.name, ms)
		}
	}
	// Something that only starts like a sound
	if code, _ := sbUpload(t, hs.URL, admin.tok, "junk", "", append([]byte("RIFF\x00\x00\x00\x00WAVE"), bytes.Repeat([]byte{7}, 500)...)); code != http.StatusUnprocessableEntity {
		t.Fatalf("junk: %d", code)
	}
}
