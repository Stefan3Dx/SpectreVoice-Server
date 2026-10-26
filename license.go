package main

// The server's tier: Free, or Pro with a licence. A licence is bought once (a key, "SV-…") and activated for one
// server: the licence service signs a token naming this server's own key (data/server.key). Moving the licence to
// another server (or a new key here) is the same activation again; the service then lets the old one go.
//
//	<- {"t":"license"}                      (staff)  -> {"t":"license","tier","limits","status",…}
//	<- {"t":"license-activate","key":"SV-…"} (staff)  activates through the licence service
//	<- {"t":"license-release"}              (staff)  back to Free here (the key can go to another server)
//
// The apps check it themselves too: the welcome carries the token and the server's key, and the server signs the
// app's own nonce with that key, so a server can't show another's licence as its own.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The licence service's public key: tokens it signs are the only ones taken.
const vendorKey = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEPvZ/XYRaeTZGQc9FvvU/yosAKgcqe1/cYJVSDrUncVf9+hPl32D5MW243ErADXL14t3uLaubaHljbDxzix/c1w=="

const (
	serverSigPrefix = "spectrevoice-server-v1:"
	licenseGrace    = 40 * 24 * time.Hour // a token stays good this long without reaching the service
	licenseRefresh  = 24 * time.Hour
)

// What each tier allows (the server's own settings can only lower these).
type limits struct {
	Tier           string `json:"tier"`
	AudioMax       int    `json:"audioMax"`
	VideoMax       int    `json:"videoMax"`
	OwnCommunities int    `json:"ownCommunities"` // per person
	FileMB         int64  `json:"fileMB"`         // one file in the chat
	ChannelFilesMB int64  `json:"channelFilesMB"` // all of a community's channel files
	RecordMin      int    `json:"recordMin"`      // 0: no limit
	Soundboard     int    `json:"soundboard"`
	Cosmetics      bool   `json:"cosmetics"` // badges, themes, profile effects
	MusicQueue     int    `json:"musicQueue"`
}

var (
	freeLimits = limits{Tier: "free", AudioMax: 4, VideoMax: 5, OwnCommunities: 2, FileMB: 25, ChannelFilesMB: 1024, RecordMin: 30, Soundboard: 12, MusicQueue: 25}
	proLimits  = limits{Tier: "pro", AudioMax: 6, VideoMax: 8, OwnCommunities: 50, FileMB: 500, ChannelFilesMB: 20480, RecordMin: 0, Soundboard: 120, Cosmetics: true, MusicQueue: 500}
)

type licenseClaims struct {
	V    int    `json:"v"`
	Lic  string `json:"lic"`
	Tier string `json:"tier"`
	Srv  string `json:"srv"`
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp,omitempty"` // the licence's own end (0: none)
	Name string `json:"name,omitempty"`
}

type licensing struct {
	s      *server
	key    *ecdsa.PrivateKey
	spki   []byte
	fp     string
	file   string
	vendor *ecdsa.PublicKey
	url    string
	http   *http.Client

	mu     sync.Mutex
	token  string
	claims *licenseClaims
	status string // "free", "pro", "rebind" (a token for another server), "expired"
	last   error
}

func fingerprint(spki []byte) string {
	h := sha256.Sum256(spki)
	return base64.RawURLEncoding.EncodeToString(h[:18])
}

func parseVendor(b64 string) (*ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("vendor key isn't ECDSA")
	}
	return ek, nil
}

// loadServerKey reads the server's own key, made the first time.
func loadServerKey(dir string) (*ecdsa.PrivateKey, error) {
	f := filepath.Join(dir, "server.key")
	if b, err := os.ReadFile(f); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, errors.New("server.key isn't PEM")
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("server.key isn't ECDSA")
		}
		return ek, nil
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	if err := writeFileAtomic(f, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func newLicensing(s *server) (*licensing, error) {
	k, err := loadServerKey(s.cfg.data)
	if err != nil {
		return nil, fmt.Errorf("server key: %w", err)
	}
	spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	vendor, err := parseVendor(s.cfg.vendorKey)
	if err != nil {
		return nil, fmt.Errorf("vendor key: %w", err)
	}
	l := &licensing{s: s, key: k, spki: spki, fp: fingerprint(spki), file: filepath.Join(s.cfg.data, "license.tok"),
		vendor: vendor, url: strings.TrimRight(s.cfg.licenseURL, "/"), http: &http.Client{Timeout: 20 * time.Second}, status: "free"}
	if b, err := os.ReadFile(l.file); err == nil {
		l.apply(strings.TrimSpace(string(b)), false)
	}
	log.Printf("licence: %s (server %s)", l.status, l.fp)
	return l, nil
}

// signRaw signs data (SHA-256) as r||s, 64 bytes, the form the apps check.
func signRaw(k *ecdsa.PrivateKey, data []byte) []byte {
	h := sha256.Sum256(data)
	r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
	if err != nil {
		return nil
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

func verifyRaw(k *ecdsa.PublicKey, data, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	h := sha256.Sum256(data)
	return ecdsa.Verify(k, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

// checkToken gives a token's claims when the vendor signed it (whichever server it names).
func (l *licensing) parse(tok string) (*licenseClaims, error) {
	body, sigPart, ok := strings.Cut(tok, ".")
	if !ok {
		return nil, errors.New("not a licence token")
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(body)
	sig, err2 := base64.RawURLEncoding.DecodeString(sigPart)
	if err1 != nil || err2 != nil || !verifyRaw(l.vendor, []byte(body), sig) {
		return nil, errors.New("the licence isn't genuine")
	}
	var c licenseClaims
	if json.Unmarshal(raw, &c) != nil || c.V != 1 {
		return nil, errors.New("unknown licence format")
	}
	return &c, nil
}

// apply takes a token (from the file or the service) and sets the tier; save keeps it on disk.
func (l *licensing) apply(tok string, save bool) error {
	c, err := l.parse(tok)
	if err != nil {
		return err
	}
	now := time.Now()
	status := "pro"
	switch {
	case c.Srv != l.fp:
		status = "rebind"
	case c.Exp != 0 && now.UnixMilli() > c.Exp:
		status = "expired"
	case now.Sub(time.UnixMilli(c.Iat)) > licenseGrace:
		status = "expired"
	case c.Tier != "pro":
		status = "free"
	}
	l.mu.Lock()
	l.token, l.claims, l.status = tok, c, status
	l.mu.Unlock()
	if save {
		if err := writeFileAtomic(l.file, []byte(tok+"\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (l *licensing) limits() limits {
	if l == nil {
		return freeLimits
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status == "pro" {
		return proLimits
	}
	return freeLimits
}

// welcome: what the app gets to see the tier and check it itself.
func (l *licensing) welcome(nonce string) map[string]any {
	out := map[string]any{"key": base64.StdEncoding.EncodeToString(l.spki), "limits": l.limits()}
	if n, err := base64.StdEncoding.DecodeString(nonce); err == nil && len(n) >= 16 && len(n) <= 64 {
		out["sig"] = base64.StdEncoding.EncodeToString(signRaw(l.key, append([]byte(serverSigPrefix), n...)))
	}
	l.mu.Lock()
	if l.status == "pro" {
		out["token"] = l.token
	}
	l.mu.Unlock()
	return out
}

func (l *licensing) statusMsg() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]any{"t": "license", "status": l.status, "server": l.fp, "limits": l.limitsLocked()}
	if l.claims != nil {
		out["lic"] = l.claims.Lic
		out["exp"] = l.claims.Exp
		out["issued"] = l.claims.Iat
	}
	if l.last != nil {
		out["error"] = l.last.Error()
	}
	return out
}

func (l *licensing) limitsLocked() limits {
	if l.status == "pro" {
		return proLimits
	}
	return freeLimits
}

var licenseKeyRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// call posts to the licence service and takes the token it gives.
func (l *licensing) call(path string, body map[string]any) error {
	if l.url == "" {
		return errors.New("no licence service set (SV_LICENSE_URL)")
	}
	b, _ := json.Marshal(body)
	resp, err := l.http.Post(l.url+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("licence service: %w", err)
	}
	defer resp.Body.Close()
	var r struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r)
	if resp.StatusCode != 200 || r.Token == "" {
		if r.Error == "" {
			r.Error = resp.Status
		}
		return errors.New(r.Error)
	}
	if err := l.apply(r.Token, true); err != nil {
		return err
	}
	l.mu.Lock()
	st := l.status
	l.mu.Unlock()
	if st != "pro" {
		return fmt.Errorf("the service gave a %s licence", st)
	}
	return nil
}

func (l *licensing) activate(key string) error {
	if !licenseKeyRe.MatchString(key) {
		return errors.New("that isn't a licence key")
	}
	err := l.call("/v1/activate", map[string]any{"key": key, "server": l.fp, "pub": base64.StdEncoding.EncodeToString(l.spki), "name": l.s.cfg.name})
	l.mu.Lock()
	l.last = err
	l.mu.Unlock()
	return err
}

// refreshLoop renews the token now and then (so a licence moved elsewhere, or refunded, stops here too).
func (l *licensing) refreshLoop() {
	for {
		time.Sleep(licenseRefresh)
		l.mu.Lock()
		tok, st := l.token, l.status
		l.mu.Unlock()
		if tok == "" || st == "rebind" {
			continue
		}
		proof := base64.StdEncoding.EncodeToString(signRaw(l.key, []byte("refresh:"+tok)))
		if err := l.call("/v1/refresh", map[string]any{"token": tok, "proof": proof}); err != nil {
			log.Printf("licence refresh: %v", err)
			if strings.Contains(err.Error(), "revoked") || strings.Contains(err.Error(), "moved") {
				l.mu.Lock()
				l.status = "free"
				l.mu.Unlock()
				_ = os.Remove(l.file)
			}
		}
		l.s.hub.licenseChanged()
	}
}

func (l *licensing) release() error {
	l.mu.Lock()
	tok := l.token
	l.mu.Unlock()
	if tok != "" && l.url != "" {
		proof := base64.StdEncoding.EncodeToString(signRaw(l.key, []byte("release:"+tok)))
		b, _ := json.Marshal(map[string]any{"token": tok, "proof": proof})
		if resp, err := l.http.Post(l.url+"/v1/release", "application/json", bytes.NewReader(b)); err == nil {
			resp.Body.Close()
		}
	}
	l.mu.Lock()
	l.token, l.claims, l.status, l.last = "", nil, "free", nil
	l.mu.Unlock()
	_ = os.Remove(l.file)
	return nil
}

func (s *server) limits() limits { return s.lic.limits() }

// licenseChanged tells everyone the tier (the apps change what they offer).
func (h *hub) licenseChanged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, o := range h.sessions {
		w := h.s.lic.welcome(o.nonce)
		w["t"] = "tier"
		o.send(w)
	}
}

func (s *server) licenseOp(ss *session, t string, m map[string]any) bool {
	switch t {
	case "license", "license-activate", "license-release":
	default:
		return false
	}
	h := s.hub
	h.mu.Lock()
	staff := h.staff(ss.uid)
	h.mu.Unlock()
	if !staff {
		ss.refuse(t, "role", "only the server's owners")
		return true
	}
	switch t {
	case "license-activate":
		go func() {
			if err := s.lic.activate(strings.TrimSpace(str(m, "key"))); err != nil {
				ss.refuse(t, "license", err.Error())
			} else {
				log.Printf("licence: activated by %s", ss.uid)
				h.licenseChanged()
			}
			ss.send(s.lic.statusMsg())
		}()
		return true
	case "license-release":
		_ = s.lic.release()
		h.licenseChanged()
	}
	ss.send(s.lic.statusMsg())
	return true
}
