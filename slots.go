package main

// Everything is free on every server. The one thing donations change is how many may be connected at once: a
// server has 30 places (or fewer, SV_MAX_USERS), and a donation made for it adds places for 30 days (+20 to no
// limit, by the donation's size). Donations add up while they last.
//
// The places are under the donation service's control (SV_LICENSE_URL, run by Spectre Voice):
//   - every server has its own key (data/server.key) and its machine's hashed parts (hwid.go);
//   - a donation is redeemed by the server itself: it sends its key, its machine and the donation, signed with its
//     key; the service checks the donation and signs a token for that key and that machine (data/slots/);
//   - this public build ("Free") keeps 30 places whatever the tokens say. With an active donation it fetches the
//     official "Plus" build from the service (signed by it, checked here) and runs that instead (upgrade.go);
//   - the Plus build applies the tokens, checks in with the service (new donations, revoked ones, its clock) and
//     stops counting them when it can't for a week.
//
// The licence (LICENSE.md, Elastic License 2.0) forbids changing, disabling or getting around this.
//
//	<- {"t":"slots"}                                       (staff) -> {"t":"slots", limits, donations:[…], edition}
//	<- {"t":"slots-redeem","product":…,"purchase":…}     (staff) a Google Play donation made in the app
//	<- {"t":"slots-redeem","code":"SVD-…"}               (staff) a code from a donation on the website

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

// The donation service's public key: only what it signs counts.
const vendorKey = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEPvZ/XYRaeTZGQc9FvvU/yosAKgcqe1/cYJVSDrUncVf9+hPl32D5MW243ErADXL14t3uLaubaHljbDxzix/c1w=="

const (
	baseUsers     = 30 // places every server has
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
	FileMB         int64  `json:"fileMB"`         // one file in the chat
	ChannelFilesMB int64  `json:"channelFilesMB"` // all the channel files
	RecordMin      int    `json:"recordMin"`      // 0: no limit
	Soundboard     int    `json:"soundboard"`
	Cosmetics      bool   `json:"cosmetics"`
	MusicQueue     int    `json:"musicQueue"`
}

var allLimits = limits{Tier: "free", MaxUsers: baseUsers, AudioMax: 6, VideoMax: 8, FileMB: 500, ChannelFilesMB: 20480,
	RecordMin: 0, Soundboard: 120, Cosmetics: true, MusicQueue: 500}

// slotClaims: what the donation service signs. Tier "gift" is a donation's places; "time" is the service's clock
// (given at each check-in).
type slotClaims struct {
	V     int      `json:"v"`
	Lic   string   `json:"lic"`  // the donation's id
	Tier  string   `json:"tier"` // "gift" or "time"
	Srv   string   `json:"srv"`  // the server's key fingerprint
	Hw    []string `json:"hw"`   // the machine's hashed parts
	Iat   int64    `json:"iat"`
	Exp   int64    `json:"exp"`
	Slots *int     `json:"slots,omitempty"` // places added; 0 = no limit
}

func (c *slotClaims) places() int {
	if c.Slots == nil {
		return 0
	}
	return *c.Slots
}

// donation: one active donation, as the apps show it.
type donation struct {
	Slots int   `json:"slots"` // 0: no limit
	Until int64 `json:"until"`
}

// edition: what this build does with the donations' tokens (edition_free.go here; the Plus build has its own).
type edition interface {
	name() string
	start(l *licensing)                         // once the kept tokens are loaded
	background(l *licensing)                    // its own work, once the server is up
	apply(l *licensing, lim *limits, now int64) // the active donations' places (l.mu held)
	accepted(l *licensing)                      // a donation was just redeemed
	note(l *licensing) (kind, text string)      // for the admins, when something is under way
}

type licensing struct {
	s      *server
	key    *ecdsa.PrivateKey
	spki   []byte
	fp     string
	hw     []string
	dir    string // data/slots
	data   string
	vendor *ecdsa.PublicKey
	url    string
	http   *http.Client
	ed     edition

	mu     sync.Mutex
	tokens map[string]*slotClaims // file name → claims
	last   error
	state  any // the edition's own (Plus: its clock and check-ins)

	upMu      sync.Mutex // one upgrade at a time
	upgrading bool
	upLast    error
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
	l := &licensing{s: s, key: k, spki: spki, fp: fingerprint(spki), hw: hardwareID(), dir: filepath.Join(s.cfg.data, "slots"), data: s.cfg.data,
		vendor: vendor, url: strings.TrimRight(s.cfg.licenseURL, "/"), http: &http.Client{Timeout: 60 * time.Second},
		tokens: map[string]*slotClaims{}, ed: newEdition()}
	if err := os.MkdirAll(l.dir, 0o750); err != nil {
		return nil, err
	}
	if len(l.hw) == 0 {
		// A machine that shows none of its parts (some containers): an id of its own, kept with the data
		f := filepath.Join(s.cfg.data, "machine.id")
		b, err := os.ReadFile(f)
		if err != nil || len(b) < 16 {
			r := make([]byte, 16)
			_, _ = rand.Read(r)
			b = []byte(hex.EncodeToString(r))
			_ = writeFileAtomic(f, b, 0o600)
		}
		l.hw = []string{hwPart("data", string(b))}
		log.Printf("places: this machine shows none of its parts; using an id kept in %s", f)
	}
	// Older versions' files: their tokens weren't tied to the machine
	for _, f := range []string{"gift.tok", "license.tok"} {
		_ = os.Remove(filepath.Join(s.cfg.data, f))
	}
	entries, _ := os.ReadDir(l.dir)
	now := time.Now().UnixMilli()
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(l.dir, e.Name())); err == nil {
			if c, err := l.parse(strings.TrimSpace(string(b))); err == nil && l.ours(c) == nil && c.Tier == "gift" && c.Exp > now {
				l.tokens[e.Name()] = c
				continue
			}
		}
		_ = os.Remove(filepath.Join(l.dir, e.Name())) // ended, or not for this server or machine
	}
	l.ed.start(l)
	log.Printf("places: %s (server %s, %s edition, %d machine parts)", placesText(l.limits().MaxUsers), l.fp, l.ed.name(), len(l.hw))
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
	if json.Unmarshal(raw, &c) != nil || c.V != 2 || (c.Tier != "gift" && c.Tier != "time") {
		return nil, errors.New("unknown token")
	}
	return &c, nil
}

// ours: is the token for this server, on this machine?
func (l *licensing) ours(c *slotClaims) error {
	if c.Srv != l.fp {
		return errors.New("that donation is for another server")
	}
	if !hwMatch(c.Hw, l.hw) {
		return errors.New("that donation is for another machine")
	}
	return nil
}

// add keeps a donation's token for this server.
func (l *licensing) add(tok string) error {
	c, err := l.parse(tok)
	if err != nil {
		return err
	}
	if err := l.ours(c); err != nil {
		return err
	}
	if c.Tier != "gift" {
		return errors.New("not a donation")
	}
	if c.Exp <= time.Now().UnixMilli() {
		return errors.New("that donation's places have ended")
	}
	h := sha256.Sum256([]byte(c.Lic))
	name := hex.EncodeToString(h[:12]) + ".tok" // one file per donation: a renewed token replaces it
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, had := l.tokens[name]; !had && len(l.tokens) >= maxSlotTokens {
		return errors.New("too many donations at once")
	}
	if err := writeFileAtomic(filepath.Join(l.dir, name), []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	l.tokens[name] = c
	return nil
}

// keepClock keeps the service's signed time, given for the request with this nonce (an old answer replayed
// doesn't count). The Plus build won't let the machine's clock run backwards past it.
func (l *licensing) keepClock(tok, nonce string) *slotClaims {
	c, err := l.parse(tok)
	if err != nil || c.Tier != "time" || l.ours(c) != nil || c.Lic != "clock:"+nonce {
		return nil
	}
	_ = writeFileAtomic(filepath.Join(l.data, "clock.tok"), []byte(tok+"\n"), 0o600)
	return c
}

// keptClock: the last signed time kept, if any.
func (l *licensing) keptClock() *slotClaims {
	b, err := os.ReadFile(filepath.Join(l.data, "clock.tok"))
	if err != nil {
		return nil
	}
	c, err := l.parse(strings.TrimSpace(string(b)))
	if err != nil || c.Tier != "time" || l.ours(c) != nil {
		return nil
	}
	return c
}

// drop forgets a donation (revoked by the service).
func (l *licensing) dropLocked(lic string) {
	for name, c := range l.tokens {
		if c.Lic == lic {
			delete(l.tokens, name)
			_ = os.Remove(filepath.Join(l.dir, name))
		}
	}
}

func (l *licensing) limits() limits {
	if l == nil {
		return allLimits
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limitsLocked()
}

// limitsLocked: 30 places, plus what the edition gives for the active donations; ended ones go.
func (l *licensing) limitsLocked() limits {
	lim := allLimits
	now := time.Now().UnixMilli()
	for name, c := range l.tokens {
		if c.Exp <= now {
			delete(l.tokens, name)
			_ = os.Remove(filepath.Join(l.dir, name))
		}
	}
	l.ed.apply(l, &lim, now)
	return lim
}

// active: the donations whose places last, soonest end first (l.mu held).
func (l *licensing) activeLocked(now int64) []*slotClaims {
	out := []*slotClaims{}
	for _, c := range l.tokens {
		if c.Exp > now {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Exp < out[j].Exp })
	return out
}

func (l *licensing) donations() []donation {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []donation{}
	for _, c := range l.activeLocked(time.Now().UnixMilli()) {
		out = append(out, donation{Slots: c.places(), Until: c.Exp})
	}
	return out
}

// welcome: what the app is told about the server's places.
func (l *licensing) welcome() map[string]any {
	return map[string]any{"limits": l.limits()}
}

func (l *licensing) statusMsg() map[string]any {
	lim := l.limits()
	out := map[string]any{"t": "slots", "server": l.fp, "limits": lim, "donations": l.donations(), "base": baseUsers, "edition": l.ed.name()}
	if kind, n := l.ed.note(l); n != "" {
		out["note"], out["noteKind"] = n, kind
	}
	l.mu.Lock()
	if l.last != nil {
		out["error"] = l.last.Error()
	}
	l.mu.Unlock()
	return out
}

// signed: a request to the donation service, signed with the server's key: who it is (key and machine), what it
// runs, and the request's own fields.
func (l *licensing) signed(kind string, fields map[string]any) []byte {
	p := map[string]any{"kind": kind, "server": l.fp, "pub": base64.StdEncoding.EncodeToString(l.spki), "hw": l.hw,
		"ts": time.Now().UnixMilli(), "version": version, "edition": l.ed.name(), "nonce": newNonce()}
	for k, v := range fields {
		p[k] = v
	}
	pj, _ := json.Marshal(p)
	pb := base64.RawURLEncoding.EncodeToString(pj)
	b, _ := json.Marshal(map[string]string{"p": pb, "sig": base64.RawURLEncoding.EncodeToString(signRaw(l.key, []byte(pb)))})
	return b
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// call posts a signed request (with a fresh nonce, given back) and decodes the JSON answer.
func (l *licensing) call(path, kind string, fields map[string]any, out any) (nonce string, err error) {
	if l.url == "" {
		return "", errors.New("no donation service set (SV_LICENSE_URL)")
	}
	nonce = newNonce()
	f := map[string]any{"nonce": nonce}
	for k, v := range fields {
		f[k] = v
	}
	resp, err := l.http.Post(l.url+path, "application/json", bytes.NewReader(l.signed(kind, f)))
	if err != nil {
		return "", fmt.Errorf("donation service: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return "", errors.New(e.Error)
	}
	return nonce, json.Unmarshal(b, out)
}

var codeRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// partnerRetry: the first wait before a hosting plan's code that couldn't be redeemed is tried again (doubling up
// to 30 minutes; a var for the tests).
var partnerRetry = time.Minute

// redeem asks the donation service for a donation's places: a Google Play purchase, or a code from the website.
func (l *licensing) redeem(product, purchase, code string) error {
	f := map[string]any{}
	switch {
	case code != "":
		if !codeRe.MatchString(code) {
			return errors.New("that isn't a donation code")
		}
		f["code"] = code
	case product != "":
		f["product"], f["purchase"] = product, purchase
	default:
		return errors.New("nothing to redeem")
	}
	var r struct{ Token, Clock string }
	nonce, err := l.call("/v1/gift", "gift", f, &r)
	if err != nil {
		return err
	}
	l.keepClock(r.Clock, nonce)
	if r.Token == "" {
		return nil // a hosting plan within the free 30 places: nothing to add
	}
	if err := l.add(r.Token); err != nil {
		return err
	}
	l.ed.accepted(l)
	return nil
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

// slotsWatch tells everyone when the places change (a donation ends, one comes in at a check-in).
func (l *licensing) slotsWatch() {
	go l.ed.background(l)
	if code := strings.ToUpper(strings.TrimSpace(l.s.cfg.partnerCode)); code != "" {
		// A hosting plan's places: redeemed at every start (the host may have changed the plan), and again until it
		// works: a node starting many servers at once, or before its network is up, mustn't leave one on 30 places
		// (and on the public build) until its next restart
		go func() {
			wait := partnerRetry
			for {
				err := l.redeem("", "", code)
				l.mu.Lock()
				l.last = err
				l.mu.Unlock()
				if err == nil {
					log.Printf("places: the hosting plan gives %s", placesText(l.limits().MaxUsers))
					l.s.hub.licenseChanged()
					return
				}
				log.Printf("places: the hosting plan's code: %v (again in %s)", err, wait)
				time.Sleep(wait)
				wait = min(2*wait, 30*time.Minute)
			}
		}()
	}
	last := l.limits().MaxUsers
	for range time.Tick(time.Minute) {
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
