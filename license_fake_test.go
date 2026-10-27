package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The tests' machine: fixed parts, whatever runs them.
func init() {
	dir, _ := os.MkdirTemp("", "sv-hw-")
	_ = os.MkdirAll(filepath.Join(dir, "etc"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "etc/machine-id"), []byte("0123456789abcdef0123456789abcdef\n"), 0o644)
	hwRoot = dir
	firstMAC = func() string { return "52:54:00:12:34:56" }
}

// fakeLS: a donation service for tests, with its own vendor key. Codes: "SVD-GOOD" +150 places, "SVD-ALL-1" no limit.
type fakeLS struct {
	*httptest.Server
	vk      *ecdsa.PrivateKey
	vendor  string // public key, base64
	mu      sync.Mutex
	grants  map[string][]fakeGrant // server → donations
	revoked map[string]bool
	plus    []byte
	plusVer string
	clock   int64 // the service's time (0: now)
	checks  int
}

type fakeGrant struct {
	lic   string
	slots int
	exp   int64
	hw    []string
}

func (f *fakeLS) sign(c slotClaims) string {
	b, _ := json.Marshal(c)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(signRaw(f.vk, []byte(body)))
}

func (f *fakeLS) now() int64 {
	if f.clock != 0 {
		return f.clock
	}
	return time.Now().UnixMilli()
}

// verifySigned checks a server's signed request, as the real service does.
func verifySigned(r io.Reader) (map[string]any, error) {
	var env struct{ P, Sig string }
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return nil, err
	}
	pj, _ := base64.RawURLEncoding.DecodeString(env.P)
	sig, _ := base64.RawURLEncoding.DecodeString(env.Sig)
	var p map[string]any
	if json.Unmarshal(pj, &p) != nil {
		return nil, errors.New("bad payload")
	}
	der, _ := base64.StdEncoding.DecodeString(p["pub"].(string))
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	if fingerprint(der) != p["server"] || !verifyRaw(k.(*ecdsa.PublicKey), []byte(env.P), sig) {
		return nil, errors.New("bad signature")
	}
	return p, nil
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func newFakeLS(t *testing.T) *fakeLS {
	t.Helper()
	vk := newKey(t)
	pub, _ := x509.MarshalPKIXPublicKey(&vk.PublicKey)
	f := &fakeLS{vk: vk, vendor: base64.StdEncoding.EncodeToString(pub), grants: map[string][]fakeGrant{}, revoked: map[string]bool{}, plusVer: "9.9.9"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := verifySigned(r.Body)
		if err != nil || p["kind"] != r.URL.Path[len("/v1/"):] {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"bad request"}`)
			return
		}
		srv, hw := p["server"].(string), strs(p["hw"])
		f.mu.Lock()
		defer f.mu.Unlock()
		clock := f.sign(slotClaims{V: 2, Lic: "clock:" + p["nonce"].(string), Tier: "time", Srv: srv, Hw: hw, Iat: f.now(), Exp: f.now() + 8*24*3600*1000})
		active := func() []string {
			var out []string
			for _, g := range f.grants[srv] {
				if !f.revoked[g.lic] && g.exp > f.now() {
					sl := g.slots
					out = append(out, f.sign(slotClaims{V: 2, Lic: g.lic, Tier: "gift", Srv: srv, Hw: g.hw, Iat: f.now(), Exp: g.exp, Slots: &sl}))
				}
			}
			return out
		}
		switch r.URL.Path {
		case "/v1/gift":
			n, ok := map[string]int{"SVD-GOOD": 150, "SVD-ALL-1": 0}[p["code"].(string)]
			if !ok {
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"error":"unknown code"}`)
				return
			}
			g := fakeGrant{lic: "D-" + p["code"].(string), slots: n, exp: f.now() + 30*24*3600*1000, hw: hw}
			f.grants[srv] = append(f.grants[srv], g)
			sl := g.slots
			_ = json.NewEncoder(w).Encode(map[string]any{"token": f.sign(slotClaims{V: 2, Lic: g.lic, Tier: "gift", Srv: srv, Hw: hw, Iat: f.now(), Exp: g.exp, Slots: &sl}), "clock": clock})
		case "/v1/checkin":
			f.checks++
			var rev []string
			for k := range f.revoked {
				rev = append(rev, k)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tokens": active(), "revoked": rev, "clock": clock, "plus": map[string]string{"version": f.plusVer}})
		case "/v1/plus":
			if len(active()) == 0 {
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"error":"no active donation for this server"}`)
				return
			}
			h := sha256.Sum256(f.plus)
			sum := hex.EncodeToString(h[:])
			w.Header().Set("X-SV-Version", f.plusVer)
			w.Header().Set("X-SV-SHA256", sum)
			w.Header().Set("X-SV-Sig", base64.RawURLEncoding.EncodeToString(signRaw(f.vk, plusSigned(f.plusVer, p["arch"].(string), sum))))
			_, _ = w.Write(f.plus)
		default:
			w.WriteHeader(404)
		}
	}))
	f.plus = []byte("#!/bin/sh\necho plus\n")
	t.Cleanup(f.Close)
	return f
}

// fakeLicenseService: the fake's address and vendor key.
func fakeLicenseService(t *testing.T) (*httptest.Server, string) {
	f := newFakeLS(t)
	return f.Server, f.vendor
}
