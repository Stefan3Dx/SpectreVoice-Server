package main

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWebGamesRules runs the web client's game rules (web/games/*.js) against the cases taken from the apps' Kotlin
// rules and tests (web/games/test/run.mjs), and replays a recording of the apps' own compiled rules through them
// (web/games/test/kotlin.mjs): the same states on the bus, the same Lei, the same moves refused. Skipped without node.
func TestWebGamesRules(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found")
	}
	for _, script := range []string{"web/games/test/run.mjs", "web/games/test/kotlin.mjs"} {
		out, err := exec.Command("node", script).CombinedOutput()
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "OK ") {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		t.Log(script, strings.TrimSpace(string(out)))
	}
}

// TestWebE2EGames: two browsers play a tic-tac-toe and a chess opening at one table, and a slot machine pays through
// the server's purse (web/e2e/games.js): SV_WEB_E2E=1 go test -run TestWebE2EGames -v . (SV_WEB_SHOTS=<dir> keeps
// screenshots). Needs node, playwright and its Chromium (see TestWebE2E).
func TestWebE2EGames(t *testing.T) {
	if os.Getenv("SV_WEB_E2E") != "1" {
		t.Skip("set SV_WEB_E2E=1 (needs node, playwright and its Chromium)")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not found")
	}
	_, hs := newTestServer(t, nil)
	args := []string{"web/e2e/games.js", hs.URL}
	if dir := os.Getenv("SV_WEB_SHOTS"); dir != "" {
		args = append(args, dir)
	}
	cmd := exec.Command("node", args...)
	cmd.Stderr = &testWriter{t}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ok := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			t.Log("e2e:", sc.Text())
			if sc.Text() == "OK" {
				ok = true
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("the pages took too long")
	}
	if err := cmd.Wait(); err != nil || !ok {
		t.Fatalf("e2e: %v (ok %v)", err, ok)
	}
}
