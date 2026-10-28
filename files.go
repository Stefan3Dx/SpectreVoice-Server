package main

// Files shared in the chat: the app uploads them here (signed in) and posts a link; anyone with the link can
// download (in a browser too). Links are unguessable; old files are removed.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type fileStore struct {
	dir     string
	maxSize int64
	keep    time.Duration
	quota   int64 // bytes all files may take together (0: no limit)
	// bytes one space (community) may take (0: no limit); files say theirs in <id>/.space
	spaceQuota int64

	mu        sync.Mutex
	used      int64 // bytes on disk, as last counted plus what was uploaded since
	reserved  int64 // bytes promised to uploads still going
	spaceUsed map[string]int64
	spaceRes  map[string]int64
}

const spaceMark = ".space"

// diskUsage adds up the shared files' sizes.
func (fs *fileStore) diskUsage() int64 {
	var n int64
	_ = filepath.WalkDir(fs.dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// spaceUsage adds up each space's files (by the mark in each file's folder; "" for files without one).
func (fs *fileStore) spaceUsage() map[string]int64 {
	out := map[string]int64{}
	entries, _ := os.ReadDir(fs.dir)
	for _, e := range entries {
		if !e.IsDir() || !hexID.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(fs.dir, e.Name())
		space := ""
		if b, err := os.ReadFile(filepath.Join(dir, spaceMark)); err == nil {
			space = string(b)
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if info, err := f.Info(); err == nil && !f.IsDir() && f.Name() != spaceMark {
				out[space] += info.Size()
			}
		}
	}
	return out
}

// recount sets used from what's really on disk (uploads still going may be counted once too much until the
// next count, which only errs on the safe side).
func (fs *fileStore) recount() {
	n := fs.diskUsage()
	per := fs.spaceUsage()
	fs.mu.Lock()
	fs.used = n
	fs.spaceUsed = per
	fs.mu.Unlock()
}

// reserve promises an upload n bytes of the quota (and of its space's); false when they don't fit.
func (fs *fileStore) reserve(space string, n int64) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.quota > 0 && fs.used+fs.reserved+n > fs.quota {
		return false
	}
	if fs.spaceUsed == nil {
		fs.spaceUsed = map[string]int64{}
	}
	if fs.spaceRes == nil {
		fs.spaceRes = map[string]int64{}
	}
	if space != "" && fs.spaceQuota > 0 && fs.spaceUsed[space]+fs.spaceRes[space]+n > fs.spaceQuota {
		return false
	}
	fs.reserved += n
	fs.spaceRes[space] += n
	return true
}

// done ends an upload's reservation; kept is how many bytes it really left on disk.
func (fs *fileStore) done(space string, reserved, kept int64) {
	fs.mu.Lock()
	fs.reserved -= reserved
	fs.used += kept
	fs.spaceRes[space] -= reserved
	fs.spaceUsed[space] += kept
	fs.mu.Unlock()
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9._ ()\-\[\]]+`)

func cleanFileName(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, "\\", "/"))
	n = safeName.ReplaceAllString(n, "_")
	n = strings.Trim(n, ". ")
	if n == "" {
		n = "file"
	}
	if len(n) > 120 {
		ext := filepath.Ext(n)
		if len(ext) > 12 {
			ext = ""
		}
		n = n[:120-len(ext)] + ext
	}
	return n
}

// POST /files?token=…&name=…[&space=…] with the bytes as the body (space: the community, for its quota) -> {"url":"/f/<id>/<name>","size":n}
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	fs := s.files
	if fs == nil {
		http.Error(w, "file sharing isn't set up on this server", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "POST the file", http.StatusMethodNotAllowed)
		return
	}
	maxSize := min(fs.maxSize, s.limits().FileMB<<20)
	if r.ContentLength > maxSize {
		http.Error(w, "too big", http.StatusRequestEntityTooLarge)
		return
	}
	// Without a total limit, any signed-in app could fill the disk
	want := r.ContentLength
	if want < 0 {
		want = maxSize // not said up front: it could be as big as allowed
	}
	// The space the file is for, by its quota: one the server has (any other name would be a fresh quota of its
	// own, and one entry more in the counters, for whoever makes names up)
	space := r.URL.Query().Get("space")
	if !s.knownSpace(space) {
		space = mainSpace
	}
	if !fs.reserve(space, want) {
		http.Error(w, "the server's space for files is full", http.StatusInsufficientStorage)
		return
	}
	kept := int64(0)
	defer func() { fs.done(space, want, kept) }()
	name := cleanFileName(r.URL.Query().Get("name"))
	idb := make([]byte, 16)
	if _, err := rand.Read(idb); err != nil {
		http.Error(w, "no randomness", 500)
		return
	}
	id := hex.EncodeToString(idb)
	dir := filepath.Join(fs.dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if space != "" {
		_ = os.WriteFile(filepath.Join(dir, spaceMark), []byte(space), 0o644)
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// A trickling upload can't hold its reserved space for ever
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Minute))
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxSize))
	f.Close()
	if err != nil {
		os.RemoveAll(dir)
		http.Error(w, "upload failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	kept = n
	log.Printf("file %s/%s (%d KB)", id, name, n>>10)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"url": "/f/" + id + "/" + name, "size": n})
}

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// GET /f/<id>/<name>: no key, the id is the secret
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	fs := s.files
	if fs == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodDelete {
		s.deleteFile(w, r, strings.SplitN(strings.TrimPrefix(r.URL.Path, "/f/"), "/", 2)[0])
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/f/"), "/", 2)
	if len(parts) != 2 || !hexID.MatchString(parts[0]) || cleanFileName(parts[1]) != parts[1] {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(fs.dir, parts[0], parts[1])
	ext := strings.ToLower(filepath.Ext(parts[1]))
	ct := mime.TypeByExtension(ext)
	inline := map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".mp4": true, ".webm": true, ".mp3": true, ".ogg": true, ".pdf": true, ".txt": true}[ext]
	if ct == "" || !inline {
		ct = "application/octet-stream"
	}
	if ext == ".txt" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'")
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", disp+`; filename="`+parts[1]+`"`)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, path)
}

// sweep removes files older than keep, every hour.
func (fs *fileStore) sweep() {
	for {
		cut := time.Now().Add(-fs.keep)
		entries, _ := os.ReadDir(fs.dir)
		for _, e := range entries {
			if !e.IsDir() || !hexID.MatchString(e.Name()) {
				continue
			}
			if info, err := e.Info(); err == nil && info.ModTime().Before(cut) {
				os.RemoveAll(filepath.Join(fs.dir, e.Name()))
			}
		}
		fs.recount()
		time.Sleep(time.Hour)
	}
}
