package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWebE2ESound drives the web client's soundboard and music bot in headless Chromium (web/e2e/sound.js):
//
//	SV_WEB_E2E=1 go test -run TestWebE2ESound -v .      (SV_WEB_SHOTS=<dir> keeps screenshots)
//
// Ana (an owner) uploads a sound from the page and plays it; Bob's page (WebAudio) and an app's connection here (as
// the PC's) both get it; the app plays one and Bob's page plays that too. Then Bob drives the music bot from its
// dialog: a song (a generated tone, no yt-dlp), a second in the queue, pause, resume, skip, stop. Needs node,
// playwright with its Chromium (see TestWebE2E) and ffmpeg (the upload's conversion, the bot's songs).
func TestWebE2ESound(t *testing.T) {
	if os.Getenv("SV_WEB_E2E") != "1" {
		t.Skip("set SV_WEB_E2E=1 (needs node, playwright and its Chromium, ffmpeg)")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not found")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal("ffmpeg not found")
	}
	s, hs := newTestServer(t, func(c *config) { c.music = true; c.ytdlp = ff; c.ffmpeg = ff })
	if !s.music.enabled() {
		t.Fatal("music off")
	}
	s.music.resolveFn = func(q string) (song, error) {
		return song{Title: "Tone " + q, Page: "https://example.com/" + q, Secs: 30, URL: "lavfi"}, nil
	}
	ffmpegInput = func(string) []string { return []string{"-f", "lavfi", "-i", "sine=frequency=440:duration=30"} }
	defer func() { ffmpegInput = defaultFFmpegInput }()
	oldCool := sbCooldown
	sbCooldown = 500 * time.Millisecond
	defer func() { sbCooldown = oldCool }()

	cmd := exec.Command("node", "web/e2e/sound.js", hs.URL)
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
		defer close(lines)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	var pc *tclient
	ok := false
	deadline := time.After(4 * time.Minute)
loop:
	for {
		select {
		case l, more := <-lines:
			if !more {
				break loop
			}
			t.Log("e2e:", l)
			switch {
			case strings.HasPrefix(l, "UID Ana "):
				if err := s.store.setRole(strings.TrimPrefix(l, "UID Ana "), roleOwner); err != nil {
					t.Fatalf("owner: %v", err)
				}
				// The app's side (as the PC is): in the same channel as the pages
				pc, _ = dialV(t, hs, newKey(t), "Pc", "")
				_, _ = io.WriteString(in, "GO\n")
			case strings.HasPrefix(l, "PLAYED "):
				// Ana's sound reached the app too, the same sound by its hash
				hash := strings.TrimPrefix(l, "PLAYED ")
				m := pc.wait(func(m map[string]any) bool { return m["t"] == "sb-play" })
				if m["hash"] != hash || m["byName"] != "Ana" {
					t.Fatalf("the app got %v, the page played %s", m, hash)
				}
				time.Sleep(time.Second) // the channel's gap between sounds
				pc.send(map[string]any{"t": "sb", "op": "play", "id": m["id"]})
				pc.wait(func(m map[string]any) bool { return m["t"] == "sb-play" && m["byName"] == "Pc" })
				_, _ = io.WriteString(in, "PCPLAYED\n")
			case l == "OK":
				ok = true
			}
		case <-deadline:
			_ = cmd.Process.Kill()
			t.Fatal("the page took too long")
		}
	}
	if err := cmd.Wait(); err != nil || !ok {
		t.Fatalf("e2e: %v (ok %v)", err, ok)
	}
	s.music.stopIn(1)
}
