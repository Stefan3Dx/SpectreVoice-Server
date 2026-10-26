package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A fake Google: the token endpoint and purchases.products.get, answering from purchases (token -> JSON).
func fakeGoogle(t *testing.T, purchases map[string]string) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || strings.Count(r.Form.Get("assertion"), ".") != 2 {
				http.Error(w, "bad grant", 400)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"AT","expires_in":3600}`))
		case strings.HasPrefix(r.URL.Path, "/androidpublisher/v3/applications/app.test/purchases/products/"):
			if r.Header.Get("Authorization") != "Bearer AT" {
				http.Error(w, "no auth", 401)
				return
			}
			calls.Add(1)
			tok := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := purchases[tok]
			if !ok {
				http.Error(w, `{"error":"not found"}`, 404)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testPlayVerifier(t *testing.T, google *httptest.Server, dir string) *playVerifier {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	cred, _ := json.Marshal(playAccount{Type: "service_account", ClientEmail: "sa@test", TokenURI: google.URL + "/token",
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	f := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(f, cred, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := newPlayVerifier(f, "app.test", dir, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	p.api = google.URL
	return p
}

func TestBusPlay(t *testing.T) {
	me, other := newUID(t), newUID(t)
	acc := playAccountID(me)
	google, calls := fakeGoogle(t, map[string]string{
		"tok-good-0001":   `{"purchaseState":0,"orderId":"GPA.1","obfuscatedExternalAccountId":"` + acc + `","purchaseTimeMillis":"1700000000000"}`,
		"tok-pending-001": `{"purchaseState":2,"orderId":"GPA.2","obfuscatedExternalAccountId":"` + acc + `"}`,
		"tok-other-00001": `{"purchaseState":0,"orderId":"GPA.3","obfuscatedExternalAccountId":"` + playAccountID(other) + `"}`,
		"tok-tester-0001": `{"purchaseState":0,"purchaseType":0,"orderId":"GPA.4","obfuscatedExternalAccountId":"` + acc + `"}`,
	})
	bt := newBusTest(t, nil, nil)
	dir := bt.s.bus.dir

	// Without the service account: refused
	c := bt.dial("play", me)
	c.write(map[string]any{"t": "play", "product": "donate_5", "token": "tok-good-0001"})
	if m := c.expect("play"); m["ok"] != false || m["why"] != "off" {
		t.Fatalf("off: %v", m)
	}

	bt.s.bus.mu.Lock()
	bt.s.bus.playV = testPlayVerifier(t, google, dir)
	bt.s.bus.mu.Unlock()

	u := bt.dial("play", other)
	c.write(map[string]any{"t": "play", "product": "donate_5", "token": "bad"})
	if m := c.expect("play"); m["why"] != "bad" {
		t.Fatalf("bad token: %v", m)
	}
	for tok, why := range map[string]string{"tok-pending-001": "pending", "tok-other-00001": "account", "tok-missing-0001": "google"} {
		c.write(map[string]any{"t": "play", "product": "donate_5", "token": tok})
		if m := c.expect("play"); m["ok"] != false || m["why"] != why {
			t.Fatalf("%s: %v", tok, m)
		}
	}
	c.write(map[string]any{"t": "play", "product": "donate_99", "token": "tok-good-0001"})
	if m := c.expect("play"); m["why"] != "product" {
		t.Fatalf("product: %v", m)
	}
	// A tester's purchase: fine to consume, nothing written
	c.write(map[string]any{"t": "play", "product": "donate_5", "token": "tok-tester-0001"})
	if m := c.expect("play"); m["ok"] != true {
		t.Fatalf("tester: %v", m)
	}

	// The real one: everyone sees the donation, in RON, then the answer
	c.write(map[string]any{"t": "play", "product": "donate_5", "token": "tok-good-0001", "name": "Ana"})
	put := c.expect("put")
	v := put["v"].(map[string]any)
	g := v["d"].([]any)[0].(map[string]any)
	if put["kind"] != "donors" || put["id"] != me || v["n"] != "Ana" || g["a"] != 25.0 || g["t"] != 1700000000000.0 || g["m"] != "Google Play 5 €" {
		t.Fatalf("donation: %v", put)
	}
	if m := c.expect("play"); m["ok"] != true || m["token"] != "tok-good-0001" {
		t.Fatalf("answer: %v", m)
	}
	if m := u.expect("put"); m["id"] != me {
		t.Fatalf("others see it: %v", m)
	}
	// Handed in again (a lost answer): ok, not counted twice
	c.write(map[string]any{"t": "play", "product": "donate_5", "token": "tok-good-0001", "name": "Ana"})
	if m := c.expect("play"); m["ok"] != true {
		t.Fatalf("again: %v", m)
	}
	// ... nor after a restart (the orders file)
	p2 := testPlayVerifier(t, google, dir)
	if p2.reserve("GPA.1") {
		t.Fatal("order forgotten after restart")
	}
	if calls.Load() == 0 {
		t.Fatal("google never asked")
	}
	// Nobody may write donors themselves
	c.write(map[string]any{"t": "put", "kind": "donors", "id": me, "v": map[string]any{"n": "x", "d": []any{}}})
	if m := c.expect("error"); m["why"] != "admin" {
		t.Fatalf("donors put: %v", m)
	}
}

func TestPrivacyPage(t *testing.T) {
	_, hs := newTestServer(t, func(c *config) { c.contact = "a<b>@x.ro" })
	r, err := http.Get(hs.URL + "/privacy")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b := make([]byte, 1<<16)
	n, _ := io.ReadFull(r.Body, b)
	body := string(b[:n])
	if r.StatusCode != 200 || !strings.Contains(body, "a&lt;b&gt;@x.ro") || strings.Contains(body, "CONTACT_EMAIL") {
		t.Fatalf("privacy: %d", r.StatusCode)
	}
}
