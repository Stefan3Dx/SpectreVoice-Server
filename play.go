package main

// Donations from the Google Play app: the phone buys a consumable ("donate_5", …) through Google Play Billing and
// hands the purchase token to the server, which asks Google whether it is real before it writes the donation
// into donors/<uid> (the same record the admin's app keeps for the PC donations). The app consumes the purchase
// only once the server said yes, so a lost answer is sent again on the next start and found already counted.
//
//	<- {"t":"play","product":"donate_5","token":"<purchaseToken>","name":"<name>"}
//	-> {"t":"play","ok":true,"token":…,"product":…}                      written (or already written before)
//	-> {"t":"play","ok":false,"token":…,"why":"off|auth|product|bad|pending|account|google|full|gone","message":…}
//
// The app sets obfuscatedAccountId = hex(sha256(uid)) when it buys, so a token can only be counted for the
// identity that paid. Only signed-in apps (auth.go) may hand in a token.

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// What each product gives, in euros. Must match the products made in Play Console and Billing.kt in the app.
var playProducts = map[string]float64{"donate_2": 2, "donate_5": 5, "donate_10": 10, "donate_20": 20}

var playTokenRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{10,1000}$`)

type playVerifier struct {
	pkg       string
	eurToRon  float64 // donors are kept in RON (Donations.CURRENCY)
	allowTest bool    // count license testers' purchases (purchaseType 0)
	api       string  // https://androidpublisher.googleapis.com (tests point it elsewhere)
	http      *http.Client

	email, tokenURI string
	key             *rsa.PrivateKey

	mu      sync.Mutex
	access  string
	expires time.Time

	seenMu   sync.Mutex
	seenFile string          // orders already counted, kept across restarts ("" in memory only)
	seen     map[string]bool // orderId (or the token when Google gives none)
	inflight map[string]bool // being written right now (the same token handed in twice at once counts once)
}

type playAccount struct {
	Type        string `json:"type"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// newPlayVerifier reads the service account's key file (downloaded from Google Cloud, never committed).
func newPlayVerifier(credFile, pkg, stateDir string, eurToRon float64, allowTest bool) (*playVerifier, error) {
	raw, err := os.ReadFile(credFile)
	if err != nil {
		return nil, err
	}
	var a playAccount
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("service account file: %w", err)
	}
	block, _ := pem.Decode([]byte(a.PrivateKey))
	if block == nil || a.ClientEmail == "" {
		return nil, errors.New("service account file has no private_key or client_email")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("service account key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("service account key isn't RSA")
	}
	if a.TokenURI == "" {
		a.TokenURI = "https://oauth2.googleapis.com/token"
	}
	p := &playVerifier{pkg: pkg, eurToRon: eurToRon, allowTest: allowTest, api: "https://androidpublisher.googleapis.com",
		http: &http.Client{Timeout: 20 * time.Second}, email: a.ClientEmail, tokenURI: a.TokenURI, key: rk, seen: map[string]bool{}, inflight: map[string]bool{}}
	if stateDir != "" {
		p.seenFile = filepath.Join(stateDir, "play-orders.json")
		if data, err := os.ReadFile(p.seenFile); err == nil {
			_ = json.Unmarshal(data, &p.seen)
		}
	}
	return p, nil
}

// accessToken signs a JWT with the service account's key and trades it for an OAuth token (kept until it ends).
func (p *playVerifier) accessToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.access != "" && time.Now().Before(p.expires) {
		return p.access, nil
	}
	enc := base64.RawURLEncoding
	now := time.Now().Unix()
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": p.email, "scope": "https://www.googleapis.com/auth/androidpublisher",
		"aud": p.tokenURI, "iat": now, "exp": now + 3600})
	unsigned := enc.EncodeToString(head) + "." + enc.EncodeToString(claims)
	h := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {unsigned + "." + enc.EncodeToString(sig)}}
	resp, err := p.http.PostForm(p.tokenURI, form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &t) != nil || t.AccessToken == "" {
		return "", fmt.Errorf("google token: %s", resp.Status)
	}
	p.access = t.AccessToken
	p.expires = time.Now().Add(time.Duration(max(t.ExpiresIn-120, 60)) * time.Second)
	return p.access, nil
}

// The parts of purchases.products.get the server looks at.
type playPurchase struct {
	PurchaseState               *int   `json:"purchaseState"` // 0 bought, 1 cancelled, 2 pending
	PurchaseType                *int   `json:"purchaseType"`  // 0 test, 1 promo, 2 rewarded; absent: a real purchase
	OrderID                     string `json:"orderId"`
	ObfuscatedExternalAccountID string `json:"obfuscatedExternalAccountId"`
	PurchaseTimeMillis          string `json:"purchaseTimeMillis"`
}

func (p *playVerifier) lookup(product, token string) (*playPurchase, error) {
	access, err := p.accessToken()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/androidpublisher/v3/applications/%s/purchases/products/%s/tokens/%s",
		strings.TrimRight(p.api, "/"), url.PathEscape(p.pkg), url.PathEscape(product), url.PathEscape(token))
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google: %s %s", resp.Status, bytes.TrimSpace(body[:min(len(body), 200)]))
	}
	var pp playPurchase
	if err := json.Unmarshal(body, &pp); err != nil || pp.PurchaseState == nil {
		return nil, errors.New("google: unexpected answer")
	}
	return &pp, nil
}

// reserve says whether order is still to be counted, and holds it until done says how it went.
func (p *playVerifier) reserve(order string) bool {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	if p.seen[order] || p.inflight[order] {
		return false
	}
	p.inflight[order] = true
	return true
}

// done releases a reserved order; counted records it for good (and keeps it on disk).
func (p *playVerifier) done(order string, counted bool) {
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	delete(p.inflight, order)
	if !counted {
		return
	}
	p.seen[order] = true
	if p.seenFile != "" {
		data, _ := json.Marshal(p.seen)
		if err := writeFileAtomic(p.seenFile, data, 0o600); err != nil {
			log.Printf("play: orders not saved: %v", err)
		}
	}
}

func playAccountID(uid string) string {
	h := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(h[:])
}

// play checks a purchase the app made and writes the donation (see the top of the file). Runs on its own
// goroutine: Google takes a moment to answer.
func (b *bus) play(sp *busSpace, c *busClient, m map[string]any) {
	product, _ := m["product"].(string)
	token, _ := m["token"].(string)
	name, _ := m["name"].(string)
	answer := func(ok bool, why, msg string) {
		r := map[string]any{"t": "play", "ok": ok, "token": token, "product": product}
		if !ok {
			r["why"], r["message"] = why, msg
		}
		c.send(r)
	}
	p := b.playV
	eur, known := playProducts[product]
	switch {
	case p == nil:
		answer(false, "off", "this server doesn't take Google Play donations")
		return
	case !c.authed:
		answer(false, "auth", "sign in first")
		return
	case !known:
		answer(false, "product", "unknown product")
		return
	case !playTokenRe.MatchString(token) || !safeID.MatchString(c.uid):
		answer(false, "bad", "bad token")
		return
	}
	pp, err := p.lookup(product, token)
	if err != nil {
		log.Printf("play: %s %s: %v", c.uid, product, err)
		answer(false, "google", "Google didn't confirm the purchase, try again later")
		return
	}
	switch {
	case *pp.PurchaseState == 2:
		answer(false, "pending", "payment still pending")
		return
	case *pp.PurchaseState != 0:
		answer(false, "bad", "purchase cancelled")
		return
	case pp.ObfuscatedExternalAccountID != playAccountID(c.uid):
		answer(false, "account", "bought by another identity")
		return
	}
	order := pp.OrderID
	if order == "" {
		order = "token:" + token
	}
	test := pp.PurchaseType != nil && *pp.PurchaseType == 0
	if test && !p.allowTest {
		answer(true, "", "") // a license tester's: the app may consume it, nothing is written
		return
	}
	if !p.reserve(order) {
		answer(true, "", "") // counted before (or right now): the app may consume it
		return
	}
	when := time.Now().UnixMilli()
	var ms int64
	if _, err := fmt.Sscan(pp.PurchaseTimeMillis, &ms); err == nil && ms > 0 {
		when = ms
	}
	note := fmt.Sprintf("Google Play %g €", eur)
	if why := b.addDonation(sp, c.uid, name, eur*p.eurToRon, when, note); why != "" {
		p.done(order, false)
		answer(false, why, "not written, try again later")
		return
	}
	p.done(order, true)
	log.Printf("play: %s gave %g € (%s)", c.uid, eur, order)
	answer(true, "", "")
}

// addDonation appends a gift to donors/<uid> as the admin's app would, and tells everyone ("" when written).
func (b *bus) addDonation(sp *busSpace, uid, name string, amount float64, when int64, note string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spaces[sp.name] != sp {
		return "gone" // everyone left (and the space was saved) while Google answered: the app hands it in again
	}
	donors := sp.state["donors"]
	if donors == nil {
		donors = map[string]json.RawMessage{}
		sp.state["donors"] = donors
	}
	rec := decodeRecord(donors[uid])
	gifts, _ := rec["d"].([]any)
	gifts = append(gifts, map[string]any{"a": amount, "t": when, "m": note})
	if len(gifts) > 300 {
		gifts = gifts[len(gifts)-300:]
	}
	rec["d"] = gifts
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > 40 {
		name = string(r[:40])
	}
	if name != "" {
		rec["n"] = name
	} else if _, has := rec["n"]; !has {
		rec["n"] = "?"
	}
	rec["_by"] = "play"
	raw, _ := json.Marshal(rec)
	grow := len(raw) - len(donors[uid])
	if len(raw) > busMaxValue || sp.size+grow > busMaxSpace || (donors[uid] == nil && len(donors) >= busKinds["donors"]) {
		return "full"
	}
	sp.set("donors", uid, raw)
	sp.dirty = true
	sp.broadcast(map[string]any{"t": "put", "kind": "donors", "id": uid, "v": json.RawMessage(raw), "from": "play"}, nil)
	return ""
}

//go:embed privacy.html
var privacyPage string

// handlePrivacy serves the Play app's privacy policy (Play Console needs a public address for it): /privacy
func (s *server) handlePrivacy(w http.ResponseWriter, r *http.Request) {
	contact := strings.TrimSpace(s.cfg.contact)
	if contact == "" {
		contact = "—"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	page := strings.NewReplacer("CONTACT_EMAIL", html.EscapeString(contact), "SERVER_NAME", html.EscapeString(s.cfg.name),
		"SV_FILE_DAYS", fmt.Sprint(s.cfg.fileDays)).Replace(privacyPage)
	_, _ = io.WriteString(w, page)
}
