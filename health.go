package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"
)

var started = time.Now()

type health struct {
	Version string `json:"version"`
	Uptime  int64  `json:"uptime"` // seconds
	Rooms   int    `json:"rooms"`
	Viewers int    `json:"viewers"`
	Bus     int    `json:"bus"`
	MemMB   int    `json:"mem_mb"`
}

func (s *server) health() health {
	h := health{Version: version, Uptime: int64(time.Since(started).Seconds())}
	s.mu.Lock()
	rooms := make([]*room, 0, len(s.rooms))
	for _, rm := range s.rooms {
		rooms = append(rooms, rm)
	}
	s.mu.Unlock()
	h.Rooms = len(rooms)
	for _, rm := range rooms {
		rm.mu.Lock()
		h.Viewers += len(rm.subs)
		rm.mu.Unlock()
	}
	if s.bus != nil {
		s.bus.mu.Lock()
		for _, sp := range s.bus.spaces {
			h.Bus += len(sp.clients)
		}
		s.bus.mu.Unlock()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	h.MemMB = int(m.Sys >> 20)
	return h
}

// GET /health?token=… : what the server is doing (for the owner's app).
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.health())
}

// watchdog tells systemd (Type=notify, WatchdogSec=…) it is ready, then keeps saying it is alive while its own
// HTTP answers; if that stops (a deadlock), systemd restarts it. Without systemd's socket it does nothing.
func (s *server) watchdog(listen string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	notify := func(msg string) {
		c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte(msg))
	}
	every := 10 * time.Second
	if us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64); err == nil && us > 0 {
		every = time.Duration(us) * time.Microsecond / 3
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	client := &http.Client{Timeout: 5 * time.Second}
	ready := false
	for {
		ok := false
		if res, err := client.Get(url); err == nil {
			ok = res.StatusCode == 200
			res.Body.Close()
		}
		// The state lock too: a stuck lock means a stuck server
		done := make(chan struct{})
		go func() { s.health(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			ok = false
			log.Printf("watchdog: the server's locks are stuck")
		}
		if ok {
			if !ready {
				notify("READY=1")
				ready = true
			}
			notify("WATCHDOG=1")
		}
		time.Sleep(every)
	}
}
