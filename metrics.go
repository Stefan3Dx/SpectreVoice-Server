package main

// The server's numbers over time, for the Pro admin panel: a sample a minute (who's online, voice, streams,
// messages, joins, network, CPU, memory), kept 30 days in SQLite. Reading /proc is cheap; nothing runs per packet
// besides the atomic counters.

import (
	"bufio"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	metricsEvery = time.Minute
	metricsKeep  = 30 * 24 * time.Hour
)

// Counted where things happen (hub.go, voice.go).
var (
	statMessages atomic.Int64
	statJoins    atomic.Int64
	statVoiceIn  atomic.Int64 // bytes of voice the server got
)

type sample struct {
	T          int64   `json:"t"` // ms
	Online     int     `json:"online"`
	Voice      int     `json:"voice"`
	Streams    int     `json:"streams"`
	Viewers    int     `json:"viewers"`
	Messages   int64   `json:"messages"` // in this minute
	Joins      int64   `json:"joins"`
	VoiceKbps  float64 `json:"voiceKbps"`
	NetInKbps  float64 `json:"netInKbps"`
	NetOutKbps float64 `json:"netOutKbps"`
	CPU        float64 `json:"cpu"` // % of one core
	RSSMB      float64 `json:"rssMB"`
	Goroutines int     `json:"goroutines"`
}

type metrics struct {
	s  *server
	mu sync.Mutex
	// the last readings, for the deltas
	lastAt              time.Time
	lastCPU             float64
	lastRx, lastTx      int64
	lastVoice, lastMsgs int64
	lastJoins           int64
	now                 sample
}

func newMetrics(s *server) *metrics {
	_, _ = s.store.db.Exec(`CREATE TABLE IF NOT EXISTS metrics(t INTEGER PRIMARY KEY, data TEXT NOT NULL)`)
	m := &metrics{s: s, lastAt: time.Now()}
	m.lastCPU = procCPU()
	m.lastRx, m.lastTx = netBytes()
	go m.loop()
	return m
}

func (m *metrics) loop() {
	t := time.NewTicker(metricsEvery)
	defer t.Stop()
	n := 0
	for range t.C {
		sm := m.take()
		b, _ := json.Marshal(sm)
		_, _ = m.s.store.db.Exec(`INSERT OR REPLACE INTO metrics(t,data) VALUES(?,?)`, sm.T, string(b))
		if n++; n%60 == 0 {
			_, _ = m.s.store.db.Exec(`DELETE FROM metrics WHERE t < ?`, time.Now().Add(-metricsKeep).UnixMilli())
		}
	}
}

// take reads everything now and gives the minute's sample.
func (m *metrics) take() sample {
	now := time.Now()
	sm := sample{T: now.UnixMilli(), Goroutines: runtime.NumGoroutine()}
	h := m.s.hub
	h.mu.Lock()
	for _, ss := range h.sessions {
		if !ss.bot {
			sm.Online++
		}
	}
	sm.Streams = len(h.live)
	h.mu.Unlock()
	sm.Voice = m.s.voice.count()
	m.s.mu.Lock()
	rooms := make([]*room, 0, len(m.s.rooms))
	for _, r := range m.s.rooms {
		rooms = append(rooms, r)
	}
	m.s.mu.Unlock()
	for _, r := range rooms {
		r.mu.Lock()
		sm.Viewers += len(r.subs)
		r.mu.Unlock()
	}
	msgs, joins, vin := statMessages.Load(), statJoins.Load(), statVoiceIn.Load()
	cpu := procCPU()
	rx, tx := netBytes()
	m.mu.Lock()
	secs := now.Sub(m.lastAt).Seconds() // under m.mu: the admin panel may take one at the same time
	if secs <= 0 {
		secs = 1
	}
	sm.Messages, sm.Joins = msgs-m.lastMsgs, joins-m.lastJoins
	sm.VoiceKbps = float64(vin-m.lastVoice) * 8 / 1000 / secs
	if rx >= m.lastRx && tx >= m.lastTx {
		sm.NetInKbps = float64(rx-m.lastRx) * 8 / 1000 / secs
		sm.NetOutKbps = float64(tx-m.lastTx) * 8 / 1000 / secs
	}
	sm.CPU = (cpu - m.lastCPU) / secs * 100
	m.lastAt, m.lastCPU, m.lastRx, m.lastTx = now, cpu, rx, tx
	m.lastMsgs, m.lastJoins, m.lastVoice = msgs, joins, vin
	sm.RSSMB = rssMB()
	m.now = sm
	m.mu.Unlock()
	return sm
}

func (m *metrics) since(from time.Time) []sample {
	rows, err := m.s.store.db.Query(`SELECT data FROM metrics WHERE t >= ? ORDER BY t`, from.UnixMilli())
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []sample{}
	for rows.Next() {
		var d string
		var sm sample
		if rows.Scan(&d) == nil && json.Unmarshal([]byte(d), &sm) == nil {
			out = append(out, sm)
		}
	}
	return out
}

// procCPU: this process's CPU seconds so far (Linux; 0 elsewhere).
func procCPU() float64 {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		s = s[i+2:]
	}
	f := strings.Fields(s)
	if len(f) < 13 {
		return 0
	}
	u, _ := strconv.ParseFloat(f[11], 64)
	k, _ := strconv.ParseFloat(f[12], 64)
	return (u + k) / 100 // clock ticks (USER_HZ is 100 on Linux)
}

func rssMB() float64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return float64(ms.Sys) / (1 << 20)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); strings.HasPrefix(l, "VmRSS:") {
			kb, _ := strconv.ParseFloat(strings.Fields(l)[1], 64)
			return kb / 1024
		}
	}
	return 0
}

// netBytes: received and sent on all interfaces but loopback (the machine's, as the server sees it).
func netBytes() (int64, int64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var rx, tx int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		fl := strings.Fields(rest)
		if len(fl) < 9 {
			continue
		}
		r, _ := strconv.ParseInt(fl[0], 10, 64)
		t, _ := strconv.ParseInt(fl[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

// logRing keeps the server's last log lines for the panel.
type logRing struct {
	mu    sync.Mutex
	lines []string
	at    int
	full  bool
}

func newLogRing(n int) *logRing { return &logRing{lines: make([]string, n)} }

func (l *logRing) Write(p []byte) (int, error) {
	l.mu.Lock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		l.lines[l.at] = line
		l.at = (l.at + 1) % len(l.lines)
		if l.at == 0 {
			l.full = true
		}
	}
	l.mu.Unlock()
	return len(p), nil
}

func (l *logRing) last() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.full {
		return append([]string(nil), l.lines[:l.at]...)
	}
	return append(append([]string(nil), l.lines[l.at:]...), l.lines[:l.at]...)
}
