package main

import (
	"os/exec"
	"strings"
	"testing"
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
