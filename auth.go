package main

// Identity: every device keeps its own P-256 key pair; there are no accounts. The public key (X.509
// SubjectPublicKeyInfo, DER, base64) is the identity, its uid a short hash of it.
//
//	-> {"t":"challenge","n":"<base64 of 32 random bytes>"}
//	<- {"t":"hello","pub":"<base64 SPKI>","sig":"<base64 ASN.1 ECDSA, SHA-256 over authPrefix+nonce>",...}
//
// A proven session gets a token (HMAC, signed with the server's secret) that the side connections
// (/bus, /streams, /files, /update) carry instead of proving the key again.

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const authPrefix = "spectrevoice-auth-v1:"

// v2 binds the signature to the server the app dialed (its host name): a server that relays our challenge to an app
// gets a signature for its own name, which no other server accepts. v1 (older apps) stays accepted for now.
const authPrefixV2 = "spectrevoice-auth-v2:"

// hostOnly: a Host header or address without its port and brackets, lower case.
func hostOnly(h string) string {
	h = strings.TrimSpace(h)
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

// signedV2: what a v2 app signs for this server.
func signedV2(host string, nonce []byte) []byte {
	return append([]byte(authPrefixV2+hostOnly(host)+"\n"), nonce...)
}

const tokenLife = 24 * time.Hour

// uidOf gives the uid of a public key (base64 SPKI): 20 characters, URL safe.
func uidOf(spki []byte) string {
	h := sha256.Sum256(spki)
	return base64.RawURLEncoding.EncodeToString(h[:15])
}

// verifyIdentity checks sig over nonce by pub and gives the key's uid.
func verifyIdentity(pub string, nonce, sig []byte) (string, error) {
	return verifyAuthSig(pub, append([]byte(authPrefix), nonce...), sig)
}

// verifyIdentityV2: the signature over this server's host name and the nonce.
func verifyIdentityV2(pub, host string, nonce, sig []byte) (string, error) {
	return verifyAuthSig(pub, signedV2(host, nonce), sig)
}

func verifyAuthSig(pub string, data, sig []byte) (string, error) {
	if len(pub) == 0 || len(pub) > 512 || len(sig) == 0 || len(sig) > 128 {
		return "", errors.New("pub or sig missing")
	}
	der, err := base64.StdEncoding.DecodeString(pub)
	if err != nil {
		return "", errors.New("pub isn't base64")
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return "", errors.New("pub doesn't parse")
	}
	ek, ok := k.(*ecdsa.PublicKey)
	if !ok || ek.Curve.Params().Name != "P-256" {
		return "", errors.New("pub isn't a P-256 key")
	}
	h := sha256.Sum256(data)
	if !ecdsa.VerifyASN1(ek, h[:], sig) {
		return "", errors.New("bad signature")
	}
	return uidOf(der), nil
}

// secret is the server's own key for tokens, made once and kept in the data folder.
func loadSecret(dir string) ([]byte, error) {
	f := filepath.Join(dir, "secret.key")
	if b, err := os.ReadFile(f); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(f, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *server) makeToken(uid string) string {
	exp := strconv.FormatInt(time.Now().Add(tokenLife).Unix(), 10)
	body := uid + "." + exp
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

// checkToken gives the uid a token was made for, if it's genuine and current.
func (s *server) checkToken(tok string) (string, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || len(tok) > 200 {
		return "", false
	}
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	// Banned from the server since: the token's day is over (files, streams, backups, reports)
	if s.mods != nil && s.mods.banned(parts[0], 0) {
		return "", false
	}
	return parts[0], true
}

// tokenUID is the uid behind a request's token (?token= or Authorization: Bearer).
func (s *server) tokenUID(r *http.Request) (string, bool) {
	t := r.URL.Query().Get("token")
	if t == "" {
		t = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return s.checkToken(t)
}

func (s *server) authorized(r *http.Request) bool {
	_, ok := s.tokenUID(r)
	return ok
}

// isAdmin: the request's token belongs to an owner or admin.
func (s *server) isAdmin(r *http.Request) bool {
	uid, ok := s.tokenUID(r)
	return ok && s.store.role(uid).atLeast(roleAdmin)
}
