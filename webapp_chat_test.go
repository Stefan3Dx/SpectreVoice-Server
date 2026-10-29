package main

import (
	"bufio"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/gorilla/websocket"
)

// javaHash is Kotlin's (Java's) String.hashCode: the apps key a message's reactions by it (Presence.kt
// MessageReactions.key: sender + "|" + hash.toUInt().toString(36)).
func javaHash(s string) int32 {
	var h int32
	for _, u := range utf16.Encode([]rune(s)) {
		h = 31*h + int32(u)
	}
	return h
}

func reactKey(name, text string) string {
	return name + "|" + strconv.FormatUint(uint64(uint32(javaHash(text))), 36)
}

func TestReactKeyAsTheApps(t *testing.T) {
	// As on the JVM: "hello".hashCode() == 99162322; an emoji counts as its two UTF-16 halves
	if javaHash("hello") != 99162322 || javaHash("💀") != 0xd83d*31+0xdc80 {
		t.Fatal(javaHash("hello"), javaHash("💀"))
	}
	if k := reactKey("Ana", "hello"); k != "Ana|1n1e4y" {
		t.Fatal(k)
	}
	// A negative hash is taken unsigned (toUInt): "polygenelubricants".hashCode() is Integer.MIN_VALUE
	if h := javaHash("polygenelubricants"); h != -2147483648 || reactKey("B", "polygenelubricants") != "B|zik0zk" {
		t.Fatal(h, reactKey("B", "polygenelubricants"))
	}
}

// TestWebChatE2E: the web client's chat as the PC has it (web/e2e/chat.js), between two browser users and a Go
// client in the PC's place: reactions over the bus with the apps' key, replies, edit, delete, @mentions, typing,
// the emoji picker.
//
//	SV_WEB_E2E=1 go test -run TestWebChatE2E -v .      (SV_WEB_SHOTS=<dir> keeps screenshots)
func TestWebChatE2E(t *testing.T) {
	if os.Getenv("SV_WEB_E2E") != "1" {
		t.Skip("set SV_WEB_E2E=1 (needs node, playwright and its Chromium)")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not found")
	}
	s, hs := newTestServer(t, nil)
	t.Cleanup(s.bus.conns.Wait)
	pc, _ := dialV(t, hs, newKey(t), "Pc", "")
	// The PC's bus: it sees the web's reactions and sends its own
	bws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+"/bus?token="+url.QueryEscape(pc.tok)+"&space="+mainSpace, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bws.Close()
	busIn := make(chan map[string]any, 256)
	go func() {
		defer close(busIn)
		for {
			var m map[string]any
			if bws.ReadJSON(&m) != nil {
				return
			}
			busIn <- m
		}
	}()
	var busMu sync.Mutex
	busWait := func(what string, match func(map[string]any) bool) map[string]any {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case m, ok := <-busIn:
				if !ok {
					t.Fatalf("bus closed waiting for %s", what)
				}
				if match(m) {
					return m
				}
			case <-deadline:
				t.Fatalf("no %s on the bus", what)
			}
		}
	}

	cmd := exec.Command("node", "web/e2e/chat.js", hs.URL)
	cmd.Stderr = &testWriter{t}
	in, _ := cmd.StdinPipe()
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
	waitLine := func(want string) {
		t.Helper()
		deadline := time.After(90 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					_ = cmd.Wait()
					t.Fatalf("e2e ended before %q", want)
				}
				if l == want {
					return
				}
			case <-deadline:
				_ = cmd.Process.Kill()
				t.Fatalf("no %q from the page", want)
			}
		}
	}
	msgWith := func(match func(m map[string]any) bool) map[string]any {
		return pc.wait(func(m map[string]any) bool {
			if m["t"] != "msg" {
				return false
			}
			mm, _ := m["m"].(map[string]any)
			return mm != nil && match(mm)
		})["m"].(map[string]any)
	}

	waitLine("STEP react")
	// Bob's 👍 reached the "PC" with the key the apps use for Ana's line
	r := busWait("Bob's reaction", func(m map[string]any) bool { return m["t"] == "mreact" })
	if r["k"] != reactKey("Ana", "hello bob") || r["e"] != "👍" || r["on"] != true || r["name"] != "Bob" || r["relay"] != true {
		t.Fatalf("reaction: %v (want key %q)", r, reactKey("Ana", "hello bob"))
	}
	hello := msgWith(func(m map[string]any) bool { return m["text"] == "hello bob" })

	waitLine("STEP reply")
	// The reply as the apps write it: the quote on the first line
	want := "↪ Ana: hello bob\nhi ana"
	msgWith(func(m map[string]any) bool { return m["text"] == want && m["name"] == "Bob" })
	busMu.Lock()
	_ = bws.WriteJSON(map[string]any{"t": "mreact", "k": reactKey("Bob", want), "e": "❤️", "on": true, "name": "Pc"})
	busMu.Unlock()
	_, _ = io.WriteString(in, "GO\n")

	waitLine("STEP edit")
	ed := msgWith(func(m map[string]any) bool { return m["text"] == "hello bob, again" })
	if ed["id"] != hello["id"] || ed["edit"] == nil {
		t.Fatalf("edit: %v (was %v)", ed, hello)
	}
	waitLine("STEP delete")
	del := msgWith(func(m map[string]any) bool { return m["del"] == true })
	if del["id"] != hello["id"] {
		t.Fatalf("delete: %v", del)
	}
	waitLine("STEP mention")
	msgWith(func(m map[string]any) bool { return m["text"] == "@Ana look" })
	busWait("typing", func(m map[string]any) bool {
		k, _ := m["k"].(string)
		return m["t"] == "typing" && m["name"] == "Bob" && strings.HasPrefix(k, "c")
	})
	waitLine("OK")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
