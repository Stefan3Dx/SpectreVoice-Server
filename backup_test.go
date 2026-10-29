package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dumpDB: every table's rows, in order, as text (two databases with the same dump hold the same data).
func dumpDB(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type IN ('table','index') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	var tables []string
	for rows.Next() {
		var n string
		var q sql.NullString
		_ = rows.Scan(&n, &q)
		fmt.Fprintf(&b, "schema %s: %s\n", n, q.String)
		if strings.HasPrefix(q.String, "CREATE TABLE") {
			tables = append(tables, n)
		}
	}
	rows.Close()
	for _, tb := range tables {
		r, err := db.Query(`SELECT * FROM "` + tb + `" ORDER BY rowid`)
		if err != nil {
			r, err = db.Query(`SELECT * FROM "` + tb + `"`)
			if err != nil {
				t.Fatal(err)
			}
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = r.Scan(ptrs...)
			fmt.Fprintf(&b, "%s %v\n", tb, vals)
		}
		r.Close()
	}
	return b.String()
}

// filesOf: the data folder's files (path → content), less the database and what a backup leaves out.
func filesOf(t *testing.T, dir string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if d.IsDir() && (top == "backups" || strings.HasPrefix(top, "before-restore-")) {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && !backupSkip[top] {
			b, _ := os.ReadFile(p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

func TestBackupRestore(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "server.env")
	_ = os.WriteFile(envFile, []byte("SV_NAME=Test\n"), 0o640)
	t.Setenv("SV_ENV_FILE", envFile)
	ownerKey, own := ownerKeyCfg(t)
	s, hs := newTestServer(t, func(c *config) { own(c); c.backupKeep = 2 })
	ana, _ := dialV(t, hs, ownerKey, "Ana", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	lobby := chanID(t, ana.wel, "Lobby")
	ana.send(map[string]any{"t": "say", "to": chanConv(lobby), "text": "before the backup"})
	ana.wait(isT("msg"))
	ana.send(map[string]any{"t": "event-save", "title": "Kept", "start": time.Now().Add(time.Hour).UnixMilli()})
	ana.wait(isDone("event-save"))
	ana.send(map[string]any{"t": "lei-give", "uid": bob.uid, "amount": 42, "reason": "kept"})
	ana.wait(isDone("lei-give"))
	s.bus.saveDirty()
	_ = os.MkdirAll(filepath.Join(s.cfg.data, "files", "ab"), 0o755)
	_ = os.WriteFile(filepath.Join(s.cfg.data, "files", "ab", "upload.bin"), bytes.Repeat([]byte{7}, 5000), 0o644)
	_ = os.MkdirAll(filepath.Join(s.cfg.data, "updates"), 0o755)
	_ = os.WriteFile(filepath.Join(s.cfg.data, "updates", "big.apk"), []byte("not kept"), 0o644)

	f, err := s.backupNow()
	if err != nil {
		t.Fatal(err)
	}
	// Only the server's user reads it: the folder 0700, the file 0600
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(s.cfg.backupDir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v", fi.Mode())
	}
	if st := s.backupStatus(); st.Count != 1 || st.Last == 0 || st.Error != "" {
		t.Fatal(st)
	}
	// What's in it: the database, the settings file, the files; not the update cache
	names := tarNames(t, f)
	for _, want := range []string{"spectre.db", "manifest.json", "etc/server.env", "data/secret.key", "data/files/ab/upload.bin"} {
		if !names[want] {
			t.Fatalf("%s missing from %v", want, names)
		}
	}
	if names["data/updates/big.apk"] || names["data/backups/"] {
		t.Fatal("left-out files in the backup", names)
	}
	wantDB := dumpDB(t, filepath.Join(s.cfg.data, "spectre.db"))
	wantFiles := filesOf(t, s.cfg.data)

	// Restored into a new folder: the same database, the same files
	dst := t.TempDir()
	aside, err := restoreBackup(dst, "", f)
	if err != nil {
		t.Fatal(err)
	}
	if got := dumpDB(t, filepath.Join(dst, "spectre.db")); got != wantDB {
		t.Fatalf("the restored database differs:\n%s\n---\n%s", got, wantDB)
	}
	gotFiles := filesOf(t, dst)
	delete(gotFiles, "updates/big.apk")
	delete(wantFiles, "updates/big.apk")
	if fmt.Sprint(gotFiles) != fmt.Sprint(wantFiles) {
		t.Fatalf("the restored files differ:\n%v\n%v", keys(gotFiles), keys(wantFiles))
	}
	if !strings.Contains(aside, "before-restore-") {
		t.Fatal(aside)
	}
	// The restored folder starts a server with the same data
	s2, hs2 := newTestServer(t, func(c *config) { c.data = dst; own(c) })
	_ = s2
	ana2, _ := dialV(t, hs2, ownerKey, "Ana", "")
	ana2.send(map[string]any{"t": "history", "to": chanConv(lobby)})
	if h := ana2.wait(isT("history")); !strings.Contains(fmt.Sprint(h["items"]), "before the backup") {
		t.Fatal(h)
	}
	ana2.send(map[string]any{"t": "lei-log"})
	if l := ana2.wait(isT("lei-log")); len(l["items"].([]any)) != 1 {
		t.Fatal(l)
	}

	// Refused while a server holds the folder
	unlock, err := lockData(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoreBackup(dst, "", f); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatal("restored over a running server:", err)
	}
	unlock()
	// A damaged archive, or one with a way out of the folder, is refused before anything moves
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	raw, _ := os.ReadFile(f)
	_ = os.WriteFile(bad, raw[:len(raw)/2], 0o600)
	if _, err := restoreBackup(t.TempDir(), "", bad); err == nil {
		t.Fatal("a cut archive restored")
	}
	evil := filepath.Join(t.TempDir(), "evil.tar.gz")
	writeTar(t, evil, map[string]string{"../escape": "x", "manifest.json": "{}"})
	victim := t.TempDir()
	_ = os.WriteFile(filepath.Join(victim, "keep"), []byte("1"), 0o600)
	if _, err := restoreBackup(victim, "", evil); err == nil {
		t.Fatal("an archive with .. restored")
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatal("the folder was touched by a refused restore")
	}
	// A database that doesn't match its checksum
	wrong := filepath.Join(t.TempDir(), "wrong.tar.gz")
	writeTar(t, wrong, map[string]string{"spectre.db": "not a database", "manifest.json": `{"db_sha256":"00"}`})
	if _, err := restoreBackup(t.TempDir(), "", wrong); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatal(err)
	}

	// The newest SV_BACKUP_KEEP are kept
	for i := 0; i < 2; i++ {
		time.Sleep(1100 * time.Millisecond)
		if _, err := s.backupNow(); err != nil {
			t.Fatal(err)
		}
	}
	if l := backupList(s.cfg.backupDir); len(l) != 2 || l[1].Name() == filepath.Base(f) {
		t.Fatal(len(l))
	}
	// The CLI makes one with its own database connection, into a given folder
	other := filepath.Join(t.TempDir(), "elsewhere")
	if err := backupCLI(s.cfg, other); err != nil {
		t.Fatal(err)
	}
	if len(backupList(other)) != 1 {
		t.Fatal("CLI backup missing")
	}
	// The admin panel's overview carries the last backup
	if bi, ok := s.adminOverview()["backup"].(backupInfo); !ok || bi.Last == 0 {
		t.Fatal(s.adminOverview()["backup"])
	}
	if !nextBackupAt(time.Date(2026, 1, 1, 4, 0, 0, 0, time.UTC), 3).Equal(time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)) {
		t.Fatal("next backup time")
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func tarNames(t *testing.T, f string) map[string]bool {
	t.Helper()
	fh, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]bool{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		out[h.Name] = true
	}
	return out
}

func writeTar(t *testing.T, f string, files map[string]string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, c := range files {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: n, Mode: 0o600, Size: int64(len(c))})
		_, _ = tw.Write([]byte(c))
	}
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(f, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
