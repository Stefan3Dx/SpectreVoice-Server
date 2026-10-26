package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Crash and freeze reports from the apps: PUT /crash?name&file&v stores one (any signed-in app),
// GET /crash lists them and GET /crash/<report> gives one (owners and admins only: reports hold
// other people's logs). Kept in <bus>/crashes, the newest 300.

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func (s *server) handleCrash(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if s.bus.dir == "" {
		http.Error(w, "nothing kept on disk", http.StatusServiceUnavailable)
		return
	}
	dir := filepath.Join(s.bus.dir, "crashes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "no", http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodPut, http.MethodPost:
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if err != nil || len(data) == 0 || len(data) > 1<<20 {
			http.Error(w, "too big", http.StatusRequestEntityTooLarge)
			return
		}
		q := r.URL.Query()
		clean := func(v string, n int) string {
			v = strings.Trim(unsafeChars.ReplaceAllString(v, "_"), "_.")
			if len(v) > n {
				v = v[:n]
			}
			if v == "" {
				v = "x"
			}
			return v
		}
		// A random part, so two reports in the same second don't overwrite each other
		rb := make([]byte, 4)
		_, _ = rand.Read(rb)
		name := fmt.Sprintf("%s-%s-%s-%s", time.Now().Format("20060102-150405"), hex.EncodeToString(rb), clean(q.Get("name"), 32), clean(q.Get("file"), 64))
		if !strings.HasSuffix(name, ".txt") && !strings.HasSuffix(name, ".log") {
			name += ".txt"
		}
		header := fmt.Sprintf("From: %s · Spectre Voice %s · %s\n\n", q.Get("name"), q.Get("v"), q.Get("file"))
		if err := writeFileAtomic(filepath.Join(dir, name), append([]byte(header), data...), 0o600); err != nil {
			http.Error(w, "not saved", http.StatusInternalServerError)
			return
		}
		log.Printf("crash report: %s", name)
		files, _ := os.ReadDir(dir)
		// 30 days at most, and the newest 300
		for _, f := range files {
			if fi, err := f.Info(); err == nil && time.Since(fi.ModTime()) > 30*24*time.Hour {
				_ = os.Remove(filepath.Join(dir, f.Name()))
			}
		}
		files, _ = os.ReadDir(dir)
		if len(files) > 300 {
			names := make([]string, 0, len(files))
			for _, f := range files {
				names = append(names, f.Name())
			}
			sort.Strings(names)
			for _, n := range names[:len(names)-300] {
				_ = os.Remove(filepath.Join(dir, n))
			}
		}
		_, _ = io.WriteString(w, "ok\n")
	case http.MethodGet:
		if !s.isAdmin(r) {
			http.Error(w, "owners and admins only", http.StatusForbidden)
			return
		}
		one := strings.TrimPrefix(r.URL.Path, "/crash")
		one = strings.TrimPrefix(one, "/")
		if one == "" {
			files, _ := os.ReadDir(dir)
			names := make([]string, 0, len(files))
			for _, f := range files {
				names = append(names, f.Name())
			}
			sort.Sort(sort.Reverse(sort.StringSlice(names)))
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, strings.Join(names, "\n")+"\n")
			return
		}
		if filepath.Base(one) != one || strings.HasPrefix(one, ".") {
			http.Error(w, "no", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(filepath.Join(dir, one))
		if err != nil {
			http.Error(w, "none", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(data)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}
