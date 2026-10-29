package main

import (
	"bufio"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWebE2EMedia drives the web client's streams, TV, files and settings in headless Chromium (web/e2e/media.js):
//
//	SV_WEB_E2E=1 go test -run TestWebE2EMedia -v .      (SV_WEB_SHOTS=<dir> keeps screenshots of each view)
//
// Needs node, playwright with its Chromium (see TestWebE2E) and ffmpeg (the TV's test channel).
func TestWebE2EMedia(t *testing.T) {
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
	// The TV's channel and its playlist are on 127.0.0.1 here
	tvLoopbackOK = true
	defer func() { tvLoopbackOK = false }()
	oldGap := tvPlayGap
	tvPlayGap = 0
	defer func() { tvPlayGap = oldGap }()
	media := t.TempDir()
	gen := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "120", "-c:v", "libx264", "-preset", "ultrafast", "-g", "30", "-c:a", "aac", "-f", "hls", "-hls_time", "2", "-hls_playlist_type", "vod", filepath.Join(media, "index.m3u8"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("making the test channel: %v %s", err, out)
	}
	provider := httptest.NewServer(http.FileServer(http.Dir(media)))
	defer provider.Close()
	var logo bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for i := range img.Pix {
		img.Pix[i] = 0xc0
	}
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	_ = png.Encode(&logo, img)
	lists := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/logo.png" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(logo.Bytes())
			return
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXTINF:-1 tvg-logo=\"http://%s/logo.png\" group-title=\"Test\",Test Channel\n%s/index.m3u8\n#EXTINF:-1 group-title=\"News\",Broken\nhttp://127.0.0.1:9/none.m3u8\n", r.Host, provider.URL)
	}))
	defer lists.Close()

	s, hs := newTestServer(t, func(c *config) {
		c.tv = true
		c.ffmpeg = ff
		c.tvVideo = "x264"
		c.tvKbps = 1200
		c.tvHeight = 360
		c.files = true
		c.fileMB = 10
	})
	t.Cleanup(s.tv.stopAll)
	// The second server takes no stream through its SFU (a share goes peer to peer) and has a password
	_, hs2 := newTestServer(t, func(c *config) { c.maxRooms = 0; c.joinPass = "pw" })

	cmd := exec.Command("node", "web/e2e/media.js", hs.URL, hs2.URL, lists.URL+"/list.m3u")
	cmd.Stderr = &testWriter{t}
	in, _ := cmd.StdinPipe()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	ok := false
	go func() {
		defer close(done)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			l := sc.Text()
			t.Log("e2e:", l)
			if uid, found := strings.CutPrefix(l, "UID Ana "); found {
				// Ana runs the server: the TV's playlist is hers to set
				if err := s.store.setRole(uid, roleOwner); err != nil {
					t.Errorf("owner: %v", err)
				}
				_, _ = io.WriteString(in, "GO\n")
			}
			if l == "OK" {
				ok = true
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("the page took too long")
	}
	if err := cmd.Wait(); err != nil || !ok {
		t.Fatalf("e2e: %v (ok %v)", err, ok)
	}
}
