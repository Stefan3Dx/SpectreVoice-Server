package main

import (
	"bufio"
	"crypto/x509"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWebMoveE2E: a moderator moves people from the web client (web/e2e/move.js): the mic on voice channels and
// the # on text ones, "Move to…" in a person's card with the voice channels, "Move to my channel".
//
//	SV_WEB_E2E=1 go test -run TestWebMoveE2E -v .      (SV_WEB_SHOTS=<dir> keeps screenshots)
func TestWebMoveE2E(t *testing.T) {
	if os.Getenv("SV_WEB_E2E") != "1" {
		t.Skip("set SV_WEB_E2E=1 (needs node, playwright and its Chromium)")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not found")
	}
	ownerKey := newKey(t)
	spki, _ := x509.MarshalPKIXPublicKey(&ownerKey.PublicKey)
	s, hs := newTestServer(t, func(c *config) { c.owners = uidOf(spki) })
	own, _ := dialV(t, hs, ownerKey, "Own", "")
	bob, _ := dialV(t, hs, newKey(t), "Bob", "")
	lobby, afk := chanID(t, own.wel, "Lobby"), chanID(t, own.wel, "AFK")
	own.send(map[string]any{"t": "ch-save", "name": "news", "kind": "text"})
	own.wait(func(m map[string]any) bool { return m["t"] == "channel" && m["c"].(map[string]any)["name"] == "news" })
	rs, _ := s.store.rolesOf(spaceID)
	var modID int64
	for _, r := range rs {
		if r.Name == "Moderator" {
			modID = r.ID
		}
	}

	cmd := exec.Command("node", "web/e2e/move.js", hs.URL)
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
	waitLine := func(prefix string) string {
		t.Helper()
		deadline := time.After(90 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					_ = cmd.Wait()
					t.Fatalf("e2e ended before %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-deadline:
				_ = cmd.Process.Kill()
				t.Fatalf("no %q from the page", prefix)
			}
		}
	}
	uid := strings.TrimPrefix(waitLine("UID "), "UID ")
	own.send(map[string]any{"t": "member-roles", "uid": uid, "roles": []any{modID}})
	own.wait(isDone("member-roles"))
	fmt.Fprintln(in, "GO")
	waitLine("STEP icons")
	waitLine("STEP picked")
	if mv := bob.wait(isT("moved")); int64(mv["ch"].(float64)) != afk || mv["by"] != "Mod" {
		t.Fatal(mv)
	}
	fmt.Fprintln(in, "GO")
	waitLine("STEP here")
	if mv := bob.wait(isT("moved")); int64(mv["ch"].(float64)) != lobby {
		t.Fatal(mv)
	}
	waitLine("OK")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
