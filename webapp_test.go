package main

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestWebAppServed(t *testing.T) {
	_, hs := newTestServer(t, nil)
	get := func(path string) (*http.Response, string) {
		t.Helper()
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := c.Get(hs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(b)
	}
	res, body := get("/app/")
	if res.StatusCode != 200 || !strings.Contains(body, `<script type="module" src="app.js">`) {
		t.Fatalf("index: %d %q", res.StatusCode, body)
	}
	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP lacks %q: %q", want, csp)
		}
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatal(ct)
	}
	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal(res.Header)
	}
	res, body = get("/app/app.js")
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(body, "spectrevoice-auth-v2:") {
		t.Fatalf("app.js: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res, _ = get("/app/app.css")
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/css") {
		t.Fatal(res.StatusCode)
	}
	// /app goes to /app/ with its query (the invite page links /app/?invite=…)
	res, _ = get("/app?invite=abc")
	if res.StatusCode != 301 || res.Header.Get("Location") != "/app/?invite=abc" {
		t.Fatal(res.StatusCode, res.Header.Get("Location"))
	}
	for _, p := range []string{"/app/nope.js", "/app/e2e/run.js", "/app/web/app.js"} {
		if res, _ = get(p); res.StatusCode != 404 {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
	}
}

func TestInvitePageOffersBrowser(t *testing.T) {
	s, hs := newTestServer(t, nil)
	in := &invite{Comm: 1, By: "x", Created: time.Now().UnixMilli()}
	if err := s.store.addInvite(in); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(hs.URL + "/i/" + in.Code)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), `href="/app/?invite=`+in.Code+`"`) {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
}

// TestWebE2E drives the page in headless Chromium: SV_WEB_E2E=1 go test -run TestWebE2E -v .
// Needs node and playwright with its Chromium (npm i -g playwright && npx playwright install chromium; NODE_PATH to
// the global node_modules when playwright isn't installed next to web/e2e).
func TestWebE2E(t *testing.T) {
	if os.Getenv("SV_WEB_E2E") != "1" {
		t.Skip("set SV_WEB_E2E=1 (needs node, playwright and its Chromium)")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not found")
	}
	_, hs := newTestServer(t, nil)
	g, _ := dialV(t, hs, newKey(t), "Go", "")
	lobby := chanID(t, g.wel, "Lobby")
	text := "hello from go " + itoa(int(time.Now().UnixNano()%1000000))

	cmd := exec.Command("node", "web/e2e/run.js", hs.URL, text)
	cmd.Stderr = &testWriter{t}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			t.Log("e2e:", sc.Text())
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitLine := func(prefix string) {
		t.Helper()
		deadline := time.After(60 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("e2e ended before %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return
				}
			case <-deadline:
				_ = cmd.Process.Kill()
				t.Fatalf("no %q from the page", prefix)
			}
		}
	}
	waitLine("CONNECTED")
	// The page's hello: a web client, in the Lobby
	web := g.wait(func(m map[string]any) bool { return m["t"] == "member" && m["m"].(map[string]any)["name"] == "Web" })["m"].(map[string]any)
	if c, _ := web["client"].(string); !strings.HasPrefix(c, "web/") {
		t.Fatalf("client: %v", web)
	}
	if int64(web["ch"].(float64)) != lobby {
		t.Fatalf("web client not in the Lobby: %v", web)
	}
	webSid := int(web["sid"].(float64))
	waitLine("RTC connected")
	g.send(map[string]any{"t": "say", "to": "c:" + itoa(int(lobby)), "text": text})
	waitLine("SEEN")
	// What the page says reaches the Go client
	if m := g.wait(func(m map[string]any) bool {
		return m["t"] == "msg" && m["m"].(map[string]any)["text"] == "hi from web"
	}); m["m"].(map[string]any)["name"] != "Web" {
		t.Fatal(m)
	}
	// Push-to-talk: the fake microphone's sound, with the audio-level extension, shows the page talking
	g.wait(func(m map[string]any) bool {
		return m["t"] == "talk" && int(m["sid"].(float64)) == webSid && m["on"] == true
	})
	waitLine("OK")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Log("e2e stderr:", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
