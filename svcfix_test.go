package main

import (
	"context"
	"database/sql"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A connected app gets a fresh token over its socket before the welcome's runs out.
func TestTokenRefreshedOverSocket(t *testing.T) {
	old := tokenRefresh
	tokenRefresh = 100 * time.Millisecond
	defer func() { tokenRefresh = old }()
	s, hs := newTestServer(t, nil)
	tokenRefresh = old
	a, _ := dialV(t, hs, newKey(t), "Ana", "")
	m := a.wait(isT("token"))
	tok, _ := m["token"].(string)
	if uid, ok := s.checkToken(tok); !ok || uid != a.uid {
		t.Fatalf("refreshed token: %q -> %q %v", tok, uid, ok)
	}
}

// The role command writes the database from another process: the running server takes it at the next hello.
func TestRoleFromDatabaseAtHello(t *testing.T) {
	s, hs := newTestServer(t, nil)
	key := newKey(t)
	a, _ := dialV(t, hs, key, "Ana", "")
	if _, err := s.store.db.Exec(`UPDATE users SET role='admin' WHERE uid=?`, a.uid); err != nil {
		t.Fatal(err)
	}
	if r := s.store.role(a.uid); r != roleMember {
		t.Fatalf("cached before the hello: %q", r)
	}
	_ = a.ws.Close()
	b, _ := dialV(t, hs, key, "Ana", "")
	if r := s.store.role(b.uid); r != roleAdmin {
		t.Fatalf("after the hello: %q", r)
	}
	// And taken away again
	if _, err := s.store.db.Exec(`UPDATE users SET role='' WHERE uid=?`, b.uid); err != nil {
		t.Fatal(err)
	}
	_ = b.ws.Close()
	dialV(t, hs, key, "Ana", "")
	s.store.mu.Lock()
	_, ok := s.store.roles[b.uid]
	s.store.mu.Unlock()
	if ok {
		t.Fatal("the cache keeps a role the database no longer has")
	}
}

// A hello turned away because the server is full keeps its invite's use; one that gets in counts it.
func TestInviteNotSpentWhenFull(t *testing.T) {
	s, hs := newTestServer(t, func(c *config) { c.maxUsers = 1 })
	in := &invite{Comm: spaceID, By: "x", Created: time.Now().UnixMilli(), MaxUses: 1}
	if err := s.store.addInvite(in); err != nil {
		t.Fatal(err)
	}
	a, _ := dialV(t, hs, newKey(t), "Ana", "")
	key := newKey(t)
	if c, e := dialVWith(t, hs, key, "Bob", map[string]any{"invite": in.Code}); c != nil || e["why"] != "full" {
		t.Fatalf("let in on a full server: %v", e)
	}
	if got, _ := s.store.invite(in.Code); got.Uses != 0 {
		t.Fatalf("a refused hello spent the invite: %d uses", got.Uses)
	}
	_ = a.ws.Close()
	deadline := time.Now().Add(5 * time.Second)
	key2 := newKey(t)
	for {
		c, e := dialVWith(t, hs, key2, "Carl", map[string]any{"invite": in.Code})
		if c != nil {
			break
		}
		if e["why"] != "full" || time.Now().After(deadline) {
			t.Fatalf("not let in once there was room: %v", e)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got, _ := s.store.invite(in.Code); got.Uses != 1 {
		t.Fatalf("uses after getting in: %d", got.Uses)
	}
}

// A restart in the middle of an upload: its row (with the reserved size) and partial blob go at the next start;
// finished files stay, and an empty folder (unmounted?) wipes nothing.
func TestChannelFilesSweepPartial(t *testing.T) {
	s, _ := newTestServer(t, nil)
	cf := s.chfiles
	add := func(size int64, blob int) int64 {
		res, err := cf.db.Exec(`INSERT INTO chfiles(ch,comm,parent,name,size,by,at) VALUES(1,?,0,'f',?,'u',1)`, spaceID, size)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if blob >= 0 {
			if err := os.WriteFile(filepath.Join(cf.dir, strconv.FormatInt(id, 10)), make([]byte, blob), 0o640); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	has := func(id int64) bool {
		_, _, _, err := cf.item(id)
		return err == nil
	}
	// No blobs at all yet: the rows stay
	lone := add(7, -1)
	cf.sweepPartial()
	if !has(lone) {
		t.Fatal("rows removed although the folder is empty")
	}
	partial, whole, empty := add(1000, 10), add(5, 5), add(0, 0)
	if _, err := newChannelFiles(s, cf.dir); err != nil {
		t.Fatal(err)
	}
	if has(partial) || has(lone) {
		t.Fatal("an unfinished upload stays listed")
	}
	if _, err := os.Stat(filepath.Join(cf.dir, strconv.FormatInt(partial, 10))); !os.IsNotExist(err) {
		t.Fatal("the partial blob stays", err)
	}
	if !has(whole) || !has(empty) {
		t.Fatal("a finished file was removed")
	}
}

// The backup copies the database on a connection of its own: it finishes while the server's only connection is
// busy (a long query, or the watchdog's probe waiting behind the copy), and the copy holds the data.
func TestBackupCopyOwnConnection(t *testing.T) {
	data := t.TempDir()
	st, err := openStore(filepath.Join(data, backupDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	if _, err := st.db.Exec(`INSERT INTO users(uid,pub,name,created,seen) VALUES('u1','p','Ana',1,1)`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := st.db.Conn(ctx) // the pool's only one, held
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy.db")
	done := make(chan error, 1)
	go func() { done <- copyDB(st.db, data, dst) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		_ = conn.Close()
		t.Fatal("the copy waited for the server's connection")
	}
	_ = conn.Close()
	if !strings.Contains(dumpDB(t, dst), "Ana") {
		t.Fatal("the copy lacks the data")
	}
	// The usual way, the server's pool free: the file found from the database itself
	if f := dbFile(st.db, ""); !strings.HasSuffix(filepath.ToSlash(f), "/"+backupDBName) {
		t.Fatalf("database file %q", f)
	}
	// In memory: copied on the database itself
	mem, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	mem.SetMaxOpenConns(1)
	_, _ = mem.Exec(`CREATE TABLE x(a TEXT); INSERT INTO x VALUES('mem')`)
	dst2 := filepath.Join(t.TempDir(), "mem.db")
	if err := copyDB(mem, "", dst2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dumpDB(t, dst2), "mem") {
		t.Fatal("the in-memory copy lacks the data")
	}
}

// A song whose stream stays open but sends nothing is given up after botStall, not after maxSongTime.
func TestMusicBotStall(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // open, silent
		}
	}()
	old := botStall
	botStall = 700 * time.Millisecond
	defer func() { botStall = old }()
	s, hs := newTestServer(t, func(c *config) { c.music = true; c.ytdlp = "ffmpeg"; c.ffmpeg = "ffmpeg" })
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	s.music.resolveFn = func(q string) (song, error) { return song{Title: "Mut", URL: "tcp"}, nil }
	ffmpegInput = func(u string) []string { return []string{"-f", "s16le", "-i", "tcp://" + ln.Addr().String()} }
	defer func() { ffmpegInput = defaultFFmpegInput }()
	start := time.Now()
	ana.send(map[string]any{"t": "say", "to": "c:" + itoa(int(chanID(t, ana.wel, "Lobby"))), "text": "!play ceva"})
	ana.wait(func(m map[string]any) bool {
		mm, _ := m["m"].(map[string]any)
		return m["t"] == "msg" && strings.Contains(str(mm, "text"), "Nu pot reda")
	})
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("given up only after %v", d)
	}
}

// A paused song isn't taken for a stalled one: only the wait for ffmpeg counts.
func TestMusicBotPauseNotStall(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	old := botStall
	botStall = 1500 * time.Millisecond
	defer func() { botStall = old }()
	s, hs := newTestServer(t, func(c *config) { c.music = true; c.ytdlp = "ffmpeg"; c.ffmpeg = "ffmpeg" })
	ana, _ := dialV(t, hs, newKey(t), "Ana", "")
	va := newVoiceClient(t, ana)
	stop := make(chan struct{})
	defer close(stop)
	go va.pump(stop)
	time.Sleep(300 * time.Millisecond)
	s.music.resolveFn = func(q string) (song, error) { return song{Title: "Ton", URL: "lavfi"}, nil }
	ffmpegInput = func(u string) []string { return []string{"-f", "lavfi", "-i", "sine=frequency=440:duration=4"} }
	defer func() { ffmpegInput = defaultFFmpegInput }()
	lobby := "c:" + itoa(int(chanID(t, ana.wel, "Lobby")))
	heard := func() int {
		va.mu.Lock()
		defer va.mu.Unlock()
		n := 0
		for k, v := range va.got {
			if k != "s"+itoa(ana.sid) {
				n += v
			}
		}
		return n
	}
	waitHeard := func(n int) {
		deadline := time.Now().Add(8 * time.Second)
		for heard() < n {
			if time.Now().After(deadline) {
				t.Fatalf("heard %d packets, want %d", heard(), n)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	ana.send(map[string]any{"t": "say", "to": lobby, "text": "!play ceva"})
	waitHeard(10)
	ana.send(map[string]any{"t": "say", "to": lobby, "text": "!pause"})
	time.Sleep(3 * botStall)
	before := heard()
	ana.send(map[string]any{"t": "say", "to": lobby, "text": "!resume"})
	waitHeard(before + 40)
	for _, m := range ana.seen {
		if mm, _ := m["m"].(map[string]any); m["t"] == "msg" && strings.Contains(str(mm, "text"), "Nu pot reda") {
			t.Fatal("a paused song was given up")
		}
	}
}

// A donation token that doesn't match this start's machine parts is kept (just not used); expired and broken ones go.
func TestDonationKeptOnHardwareMismatch(t *testing.T) {
	ls := newFakeLS(t)
	defer ls.Close()
	s, _ := newTestServer(t, func(c *config) { c.licenseURL = ls.URL; c.vendorKey = ls.vendor })
	dir := filepath.Join(s.cfg.data, "slots")
	sl := 150
	now := time.Now().UnixMilli()
	write := func(name, tok string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	other := write("other.tok", ls.sign(slotClaims{V: 2, Lic: "D-1", Tier: "gift", Srv: s.lic.fp, Hw: []string{"x", "y", "z"}, Iat: now, Exp: now + 3600_000, Slots: &sl}))
	ended := write("ended.tok", ls.sign(slotClaims{V: 2, Lic: "D-2", Tier: "gift", Srv: s.lic.fp, Hw: s.lic.hw, Iat: now - 7200_000, Exp: now - 3600_000, Slots: &sl}))
	junk := write("junk.tok", "not a token")
	good := write("good.tok", ls.sign(slotClaims{V: 2, Lic: "D-3", Tier: "gift", Srv: s.lic.fp, Hw: s.lic.hw, Iat: now, Exp: now + 3600_000, Slots: &sl}))
	l, err := newLicensing(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("a token for other machine parts was deleted:", err)
	}
	for _, p := range []string{ended, junk} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s kept: %v", p, err)
		}
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	_, loadedOther := l.tokens["other.tok"]
	_, loadedGood := l.tokens["good.tok"]
	l.mu.Unlock()
	if loadedOther || !loadedGood {
		t.Fatalf("loaded: other %v, good %v", loadedOther, loadedGood)
	}
}

// A long name cut on a space or a dot comes out the same when cleaned again: the link the upload gives works.
func TestCleanFileNameIdempotent(t *testing.T) {
	a, b := strings.Repeat("a", 119), strings.Repeat("b", 10)
	for _, in := range []string{
		a + " " + b,
		a + "." + strings.Repeat("b", 20),
		strings.Repeat("c", 200) + ".txt",
		strings.Repeat("d", 115) + ". " + strings.Repeat("e", 20) + ".txt",
		"photo.jpg", "my file (1).pdf", "...", " . ", "",
		"../../etc/passwd", `a\b\c.txt`, "x/y/z.tar.gz",
		strings.Repeat("f", 120), strings.Repeat("g", 118) + " .",
	} {
		once := cleanFileName(in)
		if twice := cleanFileName(once); twice != once {
			t.Errorf("%q: %q then %q", in, once, twice)
		}
		if len(once) > 120 || once == "" || strings.HasSuffix(once, ".") || strings.HasSuffix(once, " ") {
			t.Errorf("%q: %q", in, once)
		}
	}
	// Short names as before
	if got := cleanFileName("my file (1).pdf"); got != "my file (1).pdf" {
		t.Fatal(got)
	}
}

// The data folder's size is remembered between the admin page's refreshes, and walked again once it's old.
func TestCachedDirSize(t *testing.T) {
	old := diskCacheFor
	defer func() { diskCacheFor = old }()
	diskCacheFor = time.Hour
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o600)
	if n := cachedDirSize(dir); n != 100 {
		t.Fatalf("first: %d", n)
	}
	_ = os.WriteFile(filepath.Join(dir, "b"), make([]byte, 50), 0o600)
	if n := cachedDirSize(dir); n != 100 {
		t.Fatalf("not remembered: %d", n)
	}
	diskCacheFor = 0
	if n := cachedDirSize(dir); n != 100 {
		t.Fatalf("an old one is shown while walked again: %d", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for cachedDirSize(dir) != 150 {
		if time.Now().After(deadline) {
			t.Fatal("never walked again")
		}
		time.Sleep(20 * time.Millisecond)
	}
	diskCacheFor = time.Hour
	for { // the walk the last call may have started, done before the folder goes
		diskCache.mu.Lock()
		b := diskCache.busy
		diskCache.mu.Unlock()
		if b == "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Another folder (another server): its own size at once
	other := t.TempDir()
	_ = os.WriteFile(filepath.Join(other, "c"), make([]byte, 7), 0o600)
	if n := cachedDirSize(other); n != 7 {
		t.Fatalf("other folder: %d", n)
	}
}
