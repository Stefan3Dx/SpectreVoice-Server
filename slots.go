package main

// Everything is free on every server. The one thing donations change is how many may be connected at once: a
// server has 30 places (or fewer, SV_MAX_USERS), and each donation made for it adds places for 30 days
// (+20 to no limit, by the donation's size). Donations add up while they last.
//
// A donation is checked by the donation service (SV_LICENSE_URL), which signs a "slots" token naming this server's
// own key (data/server.key) and how many places it gives until when. The tokens are kept in data/slots/.
//
//	<- {"t":"slots"}                                       (staff) -> {"t":"slots", limits, donations:[…]}
//	<- {"t":"slots-redeem","product":…,"purchase":…}     (staff) a Google Play donation made in the app
//	<- {"t":"slots-redeem","code":"SVD-…"}               (staff) a code from a donation on the website
//
// Since the server's source is open, its owner could take the limit out; the places are a thank-you that keeps
// the lights on, not a lock.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
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
	"sort"
	"strings"
	"sync"
	"time"
)

// The donation service's public key: only tokens it signs give places.
const vendorKey = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEPvZ/XYRaeTZGQc9FvvU/yosAKgcqe1/cYJVSDrUncVf9+hPl32D5MW243ErADXL14t3uLaubaHljbDxzix/c1w=="

const (
	baseUsers     = 30 // places every server has
	oldGiftUsers  = 50 // a gift token from before sizes (no "slots" in it)
	maxSlotTokens = 200
)

// What a server allows. Everything is the same for everyone; only MaxUsers grows with donations.
type limits struct {
	Tier           string `json:"tier"`      // "free", or "supporter" while a donation is active
	Supporter      bool   `json:"supporter"` // a donation is active
	MaxUsers       int    `json:"maxUsers"`  // connected at once (0: no limit)
	BonusUsers     int    `json:"bonusUsers"`
	BonusUntil     int64  `json:"bonusUntil,omitempty"` // when the first active donation ends
	AudioMax       int    `json:"audioMax"`
	VideoMax       int    `json:"videoMax"`
	OwnCommunities int    `json:"ownCommunities"` // per person
	FileMB         int64  `json:"fileMB"`         // one file in the chat
	ChannelFilesMB int64  `json:"channelFilesMB"` // all of a community's channel files
	RecordMin      int    `json:"recordMin"`      // 0: no limit
	Soundboard     int    `json:"soundboard"`
	Cosmetics      bool   `json:"cosmetics"`
	MusicQueue     int    `json:"musicQueue"`
}

var allLimits = limits{Tier: "free", MaxUsers: baseUsers, AudioMax: 6, VideoMax: 8, OwnCommunities: 50, FileMB: 500, ChannelFilesMB: 20480,
	RecordMin: 0, Soundboard: 120, Cosmetics: true, MusicQueue: 500}

// slotClaims: what the donation service signs.
type slotClaims struct {
	V     int    `json:"v"`
	Lic   string `json:"lic"`  // the donation's id
	Tier  string `json:"tier"` // "gift"
	Srv   string `json:"srv"`  // the server's key fingerprint
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
	Slots *int   `json:"slots,omitempty"` // places added; 0 = no limit; absent (old gifts) = 50
}

func (c *slotClaims) places() int {
	if c.Slots == nil {
		return oldGiftUsers
	}
	return *c.Slots
}

// donation: one active donation, as the apps show it.
type donation struct {
	Slots int   `json:"slots"` // 0: no limit
	Until int64 `json:"until"`
}

type licensing struct {
	s      *server
	key    *ecdsa.PrivateKey
	spki   []byte
	fp     string
	dir    string // data/slots
	vendor *ecdsa.PublicKey
	url    string
	http   *http.Client

	mu     sync.Mutex
	tokens map[string]*slotClaims // file name → claims
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
	l := &licensing{s: s, key: k, spki: spki, fp: fingerprint(spki), dir: filepath.Join(s.cfg.data, "slots"), vendor: vendor,
		url: strings.TrimRight(s.cfg.licenseURL, "/"), http: &http.Client{Timeout: 20 * time.Second}, tokens: map[string]*slotClaims{}}
	if err := os.MkdirAll(l.dir, 0o750); err != nil {
		return nil, err
	}
	// The gift kept by older versions comes along; the Pro licence of older versions is no longer needed
	if b, err := os.ReadFile(filepath.Join(s.cfg.data, "gift.tok")); err == nil {
		if l.add(strings.TrimSpace(string(b))) == nil {
			_ = os.Remove(filepath.Join(s.cfg.data, "gift.tok"))
		}
	}
	_ = os.Remove(filepath.Join(s.cfg.data, "license.tok"))
	entries, _ := os.ReadDir(l.dir)
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(l.dir, e.Name())); err == nil {
			if c, err := l.parse(strings.TrimSpace(string(b))); err == nil && c.Srv == l.fp && c.Exp > time.Now().UnixMilli() {
				l.tokens[e.Name()] = c
				continue
			}
		}
		_ = os.Remove(filepath.Join(l.dir, e.Name())) // ended, or not for this server
	}
	lim := l.limits()
	log.Printf("places: %s (server %s)", placesText(lim.MaxUsers), l.fp)
	return l, nil
}

func placesText(n int) string {
	if n == 0 {
		return "no limit"
	}
	return fmt.Sprintf("%d at once", n)
}

// signRaw signs data (SHA-256) as r||s, 64 bytes.
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

// parse gives a token's claims when the donation service signed it.
func (l *licensing) parse(tok string) (*slotClaims, error) {
	body, sigPart, ok := strings.Cut(tok, ".")
	if !ok {
		return nil, errors.New("not a donation token")
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(body)
	sig, err2 := base64.RawURLEncoding.DecodeString(sigPart)
	if err1 != nil || err2 != nil || !verifyRaw(l.vendor, []byte(body), sig) {
		return nil, errors.New("the donation token isn't genuine")
	}
	var c slotClaims
	if json.Unmarshal(raw, &c) != nil || c.V != 1 || c.Tier != "gift" {
		return nil, errors.New("unknown token")
	}
	return &c, nil
}

// add keeps a token for this server.
func (l *licensing) add(tok string) error {
	c, err := l.parse(tok)
	if err != nil {
		return err
	}
	if c.Srv != l.fp {
		return errors.New("that donation is for another server")
	}
	if c.Exp <= time.Now().UnixMilli() {
		return errors.New("that donation's places have ended")
	}
	h := sha256.Sum256([]byte(tok))
	name := hex.EncodeToString(h[:12]) + ".tok"
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.tokens) >= maxSlotTokens {
		return errors.New("too many donations at once")
	}
	if err := writeFileAtomic(filepath.Join(l.dir, name), []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	l.tokens[name] = c
	return nil
}

func (l *licensing) limits() limits {
	if l == nil {
		return allLimits
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limitsLocked()
}

// limitsLocked: 30 places, plus every active donation's; ended ones go.
func (l *licensing) limitsLocked() limits {
	lim := allLimits
	now := time.Now().UnixMilli()
	for name, c := range l.tokens {
		if c.Exp <= now {
			delete(l.tokens, name)
			_ = os.Remove(filepath.Join(l.dir, name))
			continue
		}
		lim.Supporter, lim.Tier = true, "supporter"
		if lim.BonusUntil == 0 || c.Exp < lim.BonusUntil {
			lim.BonusUntil = c.Exp
		}
		if lim.MaxUsers == 0 || c.places() == 0 {
			lim.MaxUsers, lim.BonusUsers = 0, 0
			continue
		}
		lim.MaxUsers += c.places()
		lim.BonusUsers += c.places()
	}
	return lim
}

func (l *licensing) donations() []donation {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []donation{}
	now := time.Now().UnixMilli()
	for _, c := range l.tokens {
		if c.Exp > now {
			out = append(out, donation{Slots: c.places(), Until: c.Exp})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until < out[j].Until })
	return out
}

// welcome: what the app is told about the server's places.
func (l *licensing) welcome() map[string]any {
	return map[string]any{"limits": l.limits()}
}

func (l *licensing) statusMsg() map[string]any {
	lim := l.limits()
	out := map[string]any{"t": "slots", "server": l.fp, "limits": lim, "donations": l.donations(), "base": baseUsers}
	l.mu.Lock()
	if l.last != nil {
		out["error"] = l.last.Error()
	}
	l.mu.Unlock()
	return out
}

var codeRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// redeem asks the donation service for a donation's places: a Google Play purchase, or a code from the website.
func (l *licensing) redeem(product, purchase, code string) error {
	if l.url == "" {
		return errors.New("no donation service set (SV_LICENSE_URL)")
	}
	body := map[string]any{"server": l.fp, "pub": base64.StdEncoding.EncodeToString(l.spki)}
	switch {
	case code != "":
		if !codeRe.MatchString(code) {
			return errors.New("that isn't a donation code")
		}
		body["code"] = code
	case product != "":
		body["product"], body["purchase"] = product, purchase
	default:
		return errors.New("nothing to redeem")
	}
	b, _ := json.Marshal(body)
	resp, err := l.http.Post(l.url+"/v1/gift", "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("donation service: %w", err)
	}
	defer resp.Body.Close()
	var r struct{ Token, Error string }
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r)
	if resp.StatusCode != 200 || r.Token == "" {
		if r.Error == "" {
			r.Error = resp.Status
		}
		return errors.New(r.Error)
	}
	return l.add(r.Token)
}

func (s *server) limits() limits { return s.lic.limits() }

// licenseChanged tells everyone the server's places (the apps show them).
func (h *hub) licenseChanged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.s.lic.welcome()
	w["t"] = "tier"
	h.broadcast(w, nil)
}

// slotsWatch tells everyone when a donation's places end (checked hourly).
func (l *licensing) slotsWatch() {
	last := l.limits().MaxUsers
	for range time.Tick(time.Hour) {
		if n := l.limits().MaxUsers; n != last {
			last = n
			l.s.hub.licenseChanged()
		}
	}
}

func (s *server) licenseOp(ss *session, t string, m map[string]any) bool {
	switch t {
	case "slots", "slots-redeem":
	default:
		return false
	}
	h := s.hub
	h.mu.Lock()
	staff := h.staff(ss.uid)
	h.mu.Unlock()
	if !staff {
		ss.refuse(t, "role", "only the server's admins")
		return true
	}
	if t == "slots-redeem" {
		go func() {
			err := s.lic.redeem(clip(str(m, "product"), 100), clip(str(m, "purchase"), 2000), strings.ToUpper(strings.TrimSpace(clip(str(m, "code"), 64))))
			s.lic.mu.Lock()
			s.lic.last = err
			s.lic.mu.Unlock()
			if err != nil {
				ss.refuse(t, "slots", err.Error())
			} else {
				log.Printf("places: a donation redeemed by %s, now %s", ss.uid, placesText(s.limits().MaxUsers))
				h.licenseChanged()
			}
			ss.send(s.lic.statusMsg())
		}()
		return true
	}
	ss.send(s.lic.statusMsg())
	return true
}
