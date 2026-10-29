package main

// Backups of the server's data: the database (a consistent copy through SQLite's VACUUM INTO, taken while the
// server runs), and every file in the data folder (the server's keys and secrets, the bus with Lei and profiles,
// shared files and channel files, the TV's playlist, the places' tokens), and the settings file
// (/etc/spectrevoice/server.env, SV_ENV_FILE) when it can be read. Not: the backups themselves, the update cache, the
// downloaded Plus program, the database's own -wal/-shm.
//
// One compressed tar per backup, spectrevoice-backup-YYYYMMDD-HHMMSS.tar.gz, mode 0600 in a 0700 folder
// (SV_BACKUP_DIR, default <data>/backups): it holds the server's secrets, so only the server's own user reads it.
// The server makes one a day by itself at SV_BACKUP_HOUR (3; -1: never) and keeps the newest SV_BACKUP_KEEP (14).
// A manifest.json in it names the version, the time and the database's SHA-256.
//
//	spectrevoice-server backup [dir]     one now (into dir, or SV_BACKUP_DIR); works while the server runs
//	spectrevoice-server restore <file>   refuses while the server runs (it holds <data>/server.lock); checks the
//	                                     archive (names, the database's checksum and PRAGMA integrity_check), moves
//	                                     what's in the data folder now to <data>/before-restore-<time>/ and puts the
//	                                     backup in its place. The settings file in it is left to install.sh --restore.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	backupPrefix = "spectrevoice-backup-"
	backupSuffix = ".tar.gz"
	backupDBName = "spectre.db"
	backupEnvIn  = "etc/server.env" // the settings file, inside the archive
	lockName     = "server.lock"
)

// What a backup leaves out of the data folder (top-level names).
var backupSkip = map[string]bool{"spectre.db": true, "spectre.db-wal": true, "spectre.db-shm": true, "updates": true, "bin": true, lockName: true}

type backupManifest struct {
	Version  string   `json:"version"`
	Created  int64    `json:"created"`
	DBSHA256 string   `json:"db_sha256"`
	Files    int      `json:"files"`
	Env      bool     `json:"env,omitempty"`
	Skipped  []string `json:"skipped,omitempty"`
}

// backupInfo: what the admin panel shows.
type backupInfo struct {
	Last  int64  `json:"last"` // ms of the newest backup (0: none)
	Size  int64  `json:"size"`
	Count int    `json:"count"`
	Dir   string `json:"dir"`
	Error string `json:"error,omitempty"`
	Next  int64  `json:"next,omitempty"`
}

var backupState struct {
	mu   sync.Mutex
	err  string
	busy bool
}

func envFilePath() string { return env("SV_ENV_FILE", "/etc/spectrevoice/server.env") }

// backupList: the backups in dir, newest first.
func backupList(dir string) []os.FileInfo {
	entries, _ := os.ReadDir(dir)
	var out []os.FileInfo
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, backupPrefix) && strings.HasSuffix(n, backupSuffix) {
			if fi, err := e.Info(); err == nil {
				out = append(out, fi)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() > out[j].Name() })
	return out
}

func (s *server) backupStatus() backupInfo {
	list := backupList(s.cfg.backupDir)
	bi := backupInfo{Dir: s.cfg.backupDir, Count: len(list)}
	if len(list) > 0 {
		bi.Last, bi.Size = list[0].ModTime().UnixMilli(), list[0].Size()
	}
	backupState.mu.Lock()
	bi.Error = backupState.err
	backupState.mu.Unlock()
	if s.cfg.backupHour >= 0 {
		bi.Next = nextBackupAt(time.Now(), s.cfg.backupHour).UnixMilli()
	}
	return bi
}

func nextBackupAt(now time.Time, hour int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// backupLoop: the daily backup at SV_BACKUP_HOUR; one is made soon after a start when the last is over a day old.
func (s *server) backupLoop() {
	if s.cfg.backupHour < 0 || s.cfg.backupHour > 23 {
		log.Printf("backup: no automatic backups (SV_BACKUP_HOUR=%d)", s.cfg.backupHour)
		return
	}
	log.Printf("backup: daily at %02d:00 into %s, the newest %d kept", s.cfg.backupHour, s.cfg.backupDir, s.cfg.backupKeep)
	time.Sleep(2 * time.Minute)
	for {
		last := time.Time{}
		if l := backupList(s.cfg.backupDir); len(l) > 0 {
			last = l[0].ModTime()
		}
		now := time.Now()
		age := now.Sub(last)
		if age > 20*time.Hour && (now.Hour() == s.cfg.backupHour || age > 26*time.Hour) {
			s.backupNow()
		}
		time.Sleep(10 * time.Minute)
	}
}

// backupNow makes one backup with the running server's database.
func (s *server) backupNow() (string, error) {
	backupState.mu.Lock()
	if backupState.busy {
		backupState.mu.Unlock()
		return "", errors.New("a backup is being made")
	}
	backupState.busy = true
	backupState.mu.Unlock()
	f, err := makeBackup(s.store.db, s.cfg.data, s.cfg.backupDir, s.cfg.backupKeep)
	backupState.mu.Lock()
	backupState.busy = false
	if err != nil {
		backupState.err = time.Now().Format("2006-01-02 15:04") + ": " + err.Error()
		log.Printf("backup: !!! %v", err)
	} else {
		backupState.err = ""
		log.Printf("backup: %s", f)
	}
	backupState.mu.Unlock()
	return f, err
}

// makeBackup writes one backup of data (db is its database, open) into dir and keeps the newest keep there.
func makeBackup(db *sql.DB, data, dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	if need := estimateBackup(data, dir); need > 0 {
		if free, ok := freeBytes(dir); ok && free < need+need/2 {
			return "", fmt.Errorf("not enough room in %s (%d MB free, ~%d MB needed)", dir, free>>20, need>>20)
		}
	}
	tmp, err := os.MkdirTemp(dir, ".work-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	dbCopy := filepath.Join(tmp, backupDBName)
	if _, err := db.Exec(`VACUUM INTO ?`, dbCopy); err != nil {
		return "", fmt.Errorf("database copy: %w", err)
	}
	sum, err := fileSHA256(dbCopy)
	if err != nil {
		return "", err
	}
	name := backupPrefix + time.Now().Format("20060102-150405") + backupSuffix
	final := filepath.Join(dir, name)
	for i := 1; ; i++ { // two in the same second
		if _, err := os.Stat(final); errors.Is(err, fs.ErrNotExist) {
			break
		}
		final = filepath.Join(dir, fmt.Sprintf("%s%s-%d%s", backupPrefix, time.Now().Format("20060102-150405"), i, backupSuffix))
	}
	part := final + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = out.Close()
			_ = os.Remove(part)
		}
	}()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	man := backupManifest{Version: version, Created: time.Now().UnixMilli(), DBSHA256: sum}
	if err := tarFile(tw, dbCopy, backupDBName); err != nil {
		return "", err
	}
	absDir, _ := filepath.Abs(dir)
	absData, _ := filepath.Abs(data)
	err = filepath.WalkDir(absData, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			man.Skipped = append(man.Skipped, p)
			return nil
		}
		rel, _ := filepath.Rel(absData, p)
		if rel == "." {
			return nil
		}
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if p == absDir || strings.HasPrefix(p, absDir+string(filepath.Separator)) || backupSkip[top] || strings.HasPrefix(top, "before-restore-") || strings.HasPrefix(top, ".restore-") || strings.Contains(d.Name(), ".tmp-") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		in := "data/" + filepath.ToSlash(rel)
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: in + "/", Mode: 0o750, ModTime: time.Now()})
		}
		if !d.Type().IsRegular() {
			return nil // links and the like aren't the server's
		}
		if err := tarFile(tw, p, in); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // gone meanwhile (a file swept away)
			}
			return err
		}
		man.Files++
		return nil
	})
	if err != nil {
		return "", err
	}
	if b, err := os.ReadFile(envFilePath()); err == nil {
		if err := tarBytes(tw, backupEnvIn, b, 0o600); err != nil {
			return "", err
		}
		man.Env = true
	}
	mb, _ := json.MarshalIndent(man, "", "  ")
	if err := tarBytes(tw, "manifest.json", mb, 0o600); err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	ok = true
	pruneOldBackups(dir, keep)
	return final, nil
}

// estimateBackup: the data folder's size (less what's left out), as a guess at what a backup needs.
func estimateBackup(data, dir string) int64 {
	absDir, _ := filepath.Abs(dir)
	var n int64
	_ = filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ap, _ := filepath.Abs(p); ap == absDir {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func pruneOldBackups(dir string, keep int) {
	list := backupList(dir)
	for i, fi := range list {
		if i >= keep {
			_ = os.Remove(filepath.Join(dir, fi.Name()))
		}
	}
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func tarFile(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(fi.Mode().Perm() & 0o750), Size: fi.Size(), ModTime: fi.ModTime()}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	// A file still growing is cut at the size its header said (the tar stays valid)
	_, err = io.CopyN(tw, f, fi.Size())
	return err
}

func tarBytes(tw *tar.Writer, name string, b []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

// backupCLI: `spectrevoice-server backup [dir]`, with the database opened by itself (the server may be running).
func backupCLI(cfg config, dir string) error {
	if dir == "" {
		dir = cfg.backupDir
	}
	if dir == "" {
		dir = filepath.Join(cfg.data, "backups")
	}
	dbPath := filepath.Join(cfg.data, backupDBName)
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no database at %s (SV_DATA?)", dbPath)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	keep := cfg.backupKeep
	if keep <= 0 {
		keep = 14
	}
	f, err := makeBackup(db, cfg.data, dir, keep)
	if err != nil {
		return err
	}
	fmt.Println(f)
	return nil
}

// restoreBackup puts the backup file in data's place (the server must be stopped). It returns where the data
// folder's previous contents went.
func restoreBackup(data, backupDir, file string) (string, error) {
	if _, err := os.Stat(file); err != nil {
		return "", err
	}
	unlock, err := lockData(data)
	if err != nil {
		return "", err
	}
	defer unlock()
	work, err := os.MkdirTemp(data, ".restore-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	man, err := extractBackup(file, work)
	if err != nil {
		return "", fmt.Errorf("%s: %w", file, err)
	}
	dbPath := filepath.Join(work, backupDBName)
	sum, err := fileSHA256(dbPath)
	if err != nil {
		return "", fmt.Errorf("no database in the backup")
	}
	if sum != man.DBSHA256 {
		return "", errors.New("the database in the backup doesn't match its checksum: damaged")
	}
	if err := sqliteIntegrity(dbPath); err != nil {
		return "", err
	}
	// What's there now goes aside (not deleted), then the backup takes its place
	aside := filepath.Join(data, "before-restore-"+time.Now().Format("20060102-150405"))
	if err := os.Mkdir(aside, 0o700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(data)
	if err != nil {
		return "", err
	}
	workBase, asideBase := filepath.Base(work), filepath.Base(aside)
	if backupDir == "" {
		backupDir = filepath.Join(data, "backups")
	}
	backups, _ := filepath.Abs(backupDir)
	for _, e := range entries {
		n := e.Name()
		if n == workBase || n == asideBase || n == lockName || strings.HasPrefix(n, "before-restore-") {
			continue
		}
		if abs, _ := filepath.Abs(filepath.Join(data, n)); abs == backups || n == "backups" {
			continue // the backups stay where they are
		}
		if err := os.Rename(filepath.Join(data, n), filepath.Join(aside, n)); err != nil {
			return aside, err
		}
	}
	if err := os.Rename(dbPath, filepath.Join(data, backupDBName)); err != nil {
		return aside, err
	}
	inner := filepath.Join(work, "data")
	if list, err := os.ReadDir(inner); err == nil {
		for _, e := range list {
			if e.Name() == "backups" {
				continue
			}
			if err := os.Rename(filepath.Join(inner, e.Name()), filepath.Join(data, e.Name())); err != nil {
				return aside, err
			}
		}
	}
	return aside, nil
}

// extractBackup unpacks a backup into dir, refusing anything that isn't one (absolute paths, "..", links).
func extractBackup(file, dir string) (*backupManifest, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("not a backup (gzip): %w", err)
	}
	tr := tar.NewReader(gz)
	var man *backupManifest
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("damaged: %w", err)
		}
		name := hdr.Name
		clean := path.Clean(name)
		if strings.HasPrefix(name, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(name, "\\") {
			return nil, fmt.Errorf("bad name in the backup: %q", name)
		}
		if clean != backupDBName && clean != "manifest.json" && clean != backupEnvIn && clean != "etc" && clean != "data" && !strings.HasPrefix(clean, "data/") {
			return nil, fmt.Errorf("not a Spectre Voice backup (%q)", name)
		}
		target := filepath.Join(dir, filepath.FromSlash(clean))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if clean == "manifest.json" {
				b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
				if err != nil {
					return nil, err
				}
				man = &backupManifest{}
				if err := json.Unmarshal(b, man); err != nil {
					return nil, fmt.Errorf("bad manifest: %w", err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return nil, err
			}
			mode := os.FileMode(hdr.Mode).Perm() & 0o750
			if mode&0o600 != 0o600 {
				mode |= 0o600
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return nil, err
			}
			n, err := io.Copy(out, tr)
			cerr := out.Close()
			if err != nil || cerr != nil || n != hdr.Size {
				return nil, fmt.Errorf("damaged at %q", name)
			}
			_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		default:
			return nil, fmt.Errorf("not a Spectre Voice backup (%q is not a file)", name)
		}
	}
	if man == nil {
		return nil, errors.New("not a Spectre Voice backup (no manifest)")
	}
	return man, nil
}

func sqliteIntegrity(p string) error {
	db, err := sql.Open("sqlite", "file:"+p+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil {
		return fmt.Errorf("the database in the backup can't be read: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("the database in the backup is damaged: %s", res)
	}
	return nil
}
