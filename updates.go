package main

// App updates for the Spectre Voice apps installed outside Google Play: the server follows the GitHub repo's
// latest release (private repos need a token), keeps its file in <data>/updates, and answers
// GET /update/<index>.json and GET /update/<file> to signed-in apps.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type updateInfo struct {
	Version string `json:"version"`
	File    string `json:"file"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Notes   string `json:"notes,omitempty"`
	Date    string `json:"date,omitempty"`
	// The recent versions' notes, newest first: an app a few versions behind shows everything it missed
	History []histEntry `json:"history,omitempty"`
}

type histEntry struct {
	Version string `json:"version"`
	Notes   string `json:"notes,omitempty"`
	Date    string `json:"date,omitempty"`
}

// history lists the recent releases' notes (best effort: none when GitHub doesn't answer).
func (u *updater) history() []histEntry {
	res, err := u.get(u.api+"/repos/"+u.repo+"/releases?per_page=40", "application/vnd.github+json")
	if err != nil {
		log.Printf("update history: %v", err)
		return nil
	}
	defer res.Body.Close()
	var rels []ghRelease
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&rels); err != nil {
		return nil
	}
	var out []histEntry
	for _, r := range rels {
		if !u.mine(r) {
			continue
		}
		v := strings.TrimLeftFunc(r.TagName, func(c rune) bool { return c < '0' || c > '9' })
		if v == "" {
			continue
		}
		n := cleanNotes(r.Body)
		if len(n) > 4000 {
			n = n[:4000]
		}
		out = append(out, histEntry{Version: v, Notes: n, Date: r.PublishedAt})
	}
	return out
}

type updater struct {
	dir    string
	index  string // latest.json (Windows) or android.json
	asset  string // ".msi" or ".apk"
	prefix string // the releases' tag prefix ("android-v", "windows-v"); "" takes any
	repo   string // owner/name
	token  string
	api    string // https://api.github.com (tests change it)
	every  time.Duration
	http   *http.Client
}

type ghRelease struct {
	TagName     string `json:"tag_name"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func (u *updater) current() (*updateInfo, error) {
	b, err := os.ReadFile(filepath.Join(u.dir, u.index))
	if err != nil {
		return nil, err
	}
	var i updateInfo
	if err := json.Unmarshal(b, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

func (u *updater) get(url, accept string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	res, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	// A token for another repo: a public repo answers without one
	if (res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 404) && u.token != "" {
		res.Body.Close()
		req.Header.Del("Authorization")
		if res, err = u.http.Do(req); err != nil {
			return nil, err
		}
	}
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		res.Body.Close()
		return nil, fmt.Errorf("%s: %s %s", url, res.Status, strings.TrimSpace(string(b)))
	}
	return res, nil
}

// mine: a release of this updater's app (its tag prefix), published.
func (u *updater) mine(r ghRelease) bool {
	return !r.Draft && !r.Prerelease && strings.HasPrefix(r.TagName, u.prefix)
}

// latest is the newest published release of this updater's app that has its file.
func (u *updater) latest() (*ghRelease, error) {
	res, err := u.get(u.api+"/repos/"+u.repo+"/releases?per_page=40", "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	var rels []ghRelease
	err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&rels)
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	for i := range rels {
		if !u.mine(rels[i]) {
			continue
		}
		for _, a := range rels[i].Assets {
			if strings.HasSuffix(strings.ToLower(a.Name), u.asset) {
				return &rels[i], nil
			}
		}
	}
	return nil, errors.New("no release with a " + u.asset + " (tags " + u.prefix + "…)")
}

// sync fetches the newest release's file when it's newer than what we have.
func (u *updater) sync() error {
	rel, err := u.latest()
	if err != nil {
		return err
	}
	ver := strings.TrimLeftFunc(rel.TagName, func(r rune) bool { return r < '0' || r > '9' }) // v1.2.3, apk-1.2.3
	if cur, err := u.current(); err == nil && cur.Version == ver {
		return nil
	}
	idx := -1
	for i, a := range rel.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), u.asset) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("release " + rel.TagName + " has no " + u.asset)
	}
	a := rel.Assets[idx]
	name := filepath.Base(a.Name)
	res, err := u.get(a.URL, "application/octet-stream")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	part := filepath.Join(u.dir, name+".part")
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	if err := os.Rename(part, filepath.Join(u.dir, name)); err != nil {
		return err
	}
	info := updateInfo{Version: ver, File: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), Notes: cleanNotes(rel.Body), Date: rel.PublishedAt, History: u.history()}
	b, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(filepath.Join(u.dir, u.index+".tmp"), b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(u.dir, u.index+".tmp"), filepath.Join(u.dir, u.index)); err != nil {
		return err
	}
	// Only the newest installer is kept
	old, _ := filepath.Glob(filepath.Join(u.dir, "*"+u.asset))
	for _, o := range old {
		if filepath.Base(o) != name {
			os.Remove(o)
		}
	}
	log.Printf("update %s ready (%s, %d MB)", ver, name, n>>20)
	return nil
}

func (u *updater) loop() {
	for {
		if err := u.sync(); err != nil {
			log.Printf("update check: %v", err)
		}
		time.Sleep(u.every)
	}
}

// handle serves latest.json and the installer it names.
func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if len(s.updates) == 0 {
		http.Error(w, "updates aren't set up on this server", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/update/")
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp") {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".json") {
		w.Header().Set("Cache-Control", "no-store")
	}
	if strings.HasSuffix(name, ".apk") {
		w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	}
	http.ServeFile(w, r, filepath.Join(s.updates[0].dir, name))
}

// cleanNotes drops the "Changelog:" prefix some release notes carry on each line.
func cleanNotes(s string) string { return strings.ReplaceAll(s, "- Changelog: ", "- ") }
