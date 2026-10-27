package main

// The official Plus build: built by Spectre Voice from this source and its private part, signed with the donation
// service's key, and given only to servers with an active donation. It is kept in <data>/bin and run in place of
// this build (the same process, exec), so an update of the public build (install.sh --update) keeps it.
//
//	POST /v1/plus  (signed by the server, {arch})  -> the file; X-SV-Version, X-SV-SHA256, X-SV-Sig
//
// The signature covers "spectrevoice-plus|<version>|<arch>|<sha256>"; a file that doesn't match it never runs.

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const plusName = "spectrevoice-server-plus"

type plusMeta struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
	Sig     string `json:"sig"`
}

func plusSigned(version, arch, sum string) []byte {
	return []byte("spectrevoice-plus|" + version + "|" + arch + "|" + sum)
}

// execPlus runs the Plus build in this process's place (tests change it).
var execPlus = func(path string) error {
	return syscall.Exec(path, append([]string{path}, os.Args[1:]...), append(os.Environ(), "SV_EXEC_PLUS=1"))
}

// cmpVersion compares "1.2.3" versions ("dev" and other words count as 0).
func cmpVersion(a, b string) int {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(strings.SplitN(pa[i], "-", 2)[0])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(strings.SplitN(pb[i], "-", 2)[0])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// verifyPlus: the kept Plus build, when it is genuine and for this machine's architecture.
func verifyPlus(data string, vendor *ecdsa.PublicKey) (string, *plusMeta, error) {
	dir := filepath.Join(data, "bin")
	b, err := os.ReadFile(filepath.Join(dir, plusName+".json"))
	if err != nil {
		return "", nil, err
	}
	var m plusMeta
	if json.Unmarshal(b, &m) != nil {
		return "", nil, errors.New("bad Plus description")
	}
	sig, _ := base64.RawURLEncoding.DecodeString(m.Sig)
	if m.Arch != runtime.GOARCH || !verifyRaw(vendor, plusSigned(m.Version, m.Arch, m.SHA256), sig) {
		return "", nil, errors.New("the Plus build isn't genuine")
	}
	path := filepath.Join(dir, plusName)
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return "", nil, errors.New("the Plus build was changed")
	}
	return path, &m, nil
}

// runPlusIfPresent: at start, the public build hands over to a kept, genuine Plus build that isn't older than it.
func runPlusIfPresent(cfg config) {
	if newEdition().name() != "free" || os.Getenv("SV_EXEC_PLUS") != "" {
		return
	}
	vk := cfg.vendorKey
	if vk == "" {
		vk = vendorKey
	}
	vendor, err := parseVendor(vk)
	if err != nil {
		return
	}
	path, m, err := verifyPlus(cfg.data, vendor)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("plus: %v", err)
		}
		return
	}
	if cmpVersion(m.Version, version) < 0 {
		log.Printf("plus: the kept Plus build (%s) is older than this one (%s); a newer one comes with the next check", m.Version, version)
		return
	}
	log.Printf("plus: running the official Plus build %s", m.Version)
	if err := execPlus(path); err != nil {
		log.Printf("plus: %v", err)
	}
}

func (l *licensing) hasActive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.activeLocked(time.Now().UnixMilli())) > 0
}

func (l *licensing) upgradeErr() error {
	l.upMu.Lock()
	defer l.upMu.Unlock()
	return l.upLast
}

// upgradeLoop fetches the Plus build and runs it; tried again every 15 minutes while a donation is active.
func (l *licensing) upgradeLoop() {
	l.upMu.Lock()
	if l.upgrading {
		l.upMu.Unlock()
		return
	}
	l.upgrading = true
	l.upMu.Unlock()
	defer func() { l.upMu.Lock(); l.upgrading = false; l.upMu.Unlock() }()
	for l.hasActive() {
		path, err := l.fetchPlus()
		l.upMu.Lock()
		l.upLast = err
		l.upMu.Unlock()
		if err == nil {
			log.Printf("plus: installed, restarting into it")
			if err = execPlus(path); err == nil {
				return
			}
			log.Printf("plus: %v", err)
		} else {
			log.Printf("plus: %v", err)
		}
		time.Sleep(upgradeRetry)
	}
}

var upgradeRetry = 15 * time.Minute

// fetchPlus downloads the Plus build for this machine and keeps it once its signature checks out.
func (l *licensing) fetchPlus() (string, error) {
	if l.url == "" {
		return "", errors.New("no donation service set (SV_LICENSE_URL)")
	}
	resp, err := l.http.Post(l.url+"/v1/plus", "application/json", strings.NewReader(string(l.signed("plus", map[string]any{"arch": runtime.GOARCH}))))
	if err != nil {
		return "", fmt.Errorf("donation service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return "", errors.New(e.Error)
	}
	m := plusMeta{Version: resp.Header.Get("X-SV-Version"), Arch: runtime.GOARCH, SHA256: strings.ToLower(resp.Header.Get("X-SV-SHA256")), Sig: resp.Header.Get("X-SV-Sig")}
	sig, _ := base64.RawURLEncoding.DecodeString(m.Sig)
	if !verifyRaw(l.vendor, plusSigned(m.Version, m.Arch, m.SHA256), sig) {
		return "", errors.New("the Plus build's signature doesn't check out")
	}
	if cmpVersion(m.Version, version) < 0 {
		return "", fmt.Errorf("the Plus build offered (%s) is older than this server (%s)", m.Version, version)
	}
	dir := filepath.Join(l.data, "bin")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".plus-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, 200<<20))
	_ = tmp.Close()
	if err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return "", errors.New("the Plus build arrived damaged")
	}
	if err := os.Chmod(tmp.Name(), 0o750); err != nil {
		return "", err
	}
	path := filepath.Join(dir, plusName)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	mb, _ := json.Marshal(m)
	if err := writeFileAtomic(filepath.Join(dir, plusName+".json"), mb, 0o640); err != nil {
		return "", err
	}
	return path, nil
}
