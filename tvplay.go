package main

// Playing a channel on the server's TV (tv.go): ffmpeg reads the channel (through the TV's proxy, tvnet.go) and
// sends RTP (H.264 + Opus) to two UDP ports on 127.0.0.1; the packets go from there into the room's tracks, as a
// streamer's would. Viewers watch the room "tv:main" exactly like a screen share.
//
// ffmpeg is held on a short leash: a picture at most SV_TV_HEIGHT high (16:9 width), 30 frames a second,
// SV_TV_KBPS; read and connect timeouts and reconnects on its input; killed when nothing came for a while, when the
// channel changes, when nobody watches, and with the server (its own process group, killed with the server).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	tvMaxTries   = 8  // ffmpeg failing this many times in a row: the TV gives up and says why
	tvFPSMax     = 30 // x264 in software: 30 frames keep a small server's CPU moderate
	tvKbpsMax    = 12000
	tvHeightMax  = 1080
	tvRoomPrefix = "tv:"
)

// Vars so tests can make them short: no packet this long after ffmpeg starts, or while it plays, and it's killed.
var (
	tvFirstPacket = 40 * time.Second
	tvStall       = 25 * time.Second
)

// What the tracks announce: Constrained Baseline is the H.264 every WebRTC viewer takes.
var (
	tvVideoCap = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}
	tvAudioCap = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: opusFmtp}
)

type tvPlayer struct {
	tm           *tvManager
	rm           *room
	video, audio *webrtc.TrackLocalStaticRTP
	vconn, aconn *net.UDPConn
	ctx          context.Context
	cancel       context.CancelFunc
	runs         sync.WaitGroup // the channel being played (and the one before, until its ffmpeg is gone)

	mu        sync.Mutex
	closed    bool
	runCancel context.CancelFunc
	runDone   chan struct{}
	runCtx    context.Context
	idx       int    // the channel playing
	err       string // why it doesn't play now ("" when fine)
	watched   time.Time
	noVideo   bool // the channel has only sound (a radio)

	live    atomic.Bool
	lastPkt atomic.Int64 // the last packet from ffmpeg (unix ns)
}

// newTVPlayer opens the TV's room (s.rooms, within the server's stream limit) and its two local ports.
func newTVPlayer(tm *tvManager) (*tvPlayer, error) {
	s := tm.s
	video, err := webrtc.NewTrackLocalStaticRTP(tvVideoCap, "video", "spectre-tv")
	if err != nil {
		return nil, err
	}
	audio, err := webrtc.NewTrackLocalStaticRTP(tvAudioCap, "audio", "spectre-tv")
	if err != nil {
		return nil, err
	}
	rm := &room{name: tvRoom, subs: map[*subscriber]bool{}, local: true, tracks: []*webrtc.TrackLocalStaticRTP{video, audio},
		keyAt: map[webrtc.SSRC]time.Time{}, stop: make(chan struct{})}
	s.mu.Lock()
	old := s.rooms[rm.name]
	if old == nil && len(s.rooms) >= s.cfg.maxRooms {
		s.mu.Unlock()
		return nil, errors.New("the server has as many streams as it may")
	}
	s.rooms[rm.name] = rm
	s.mu.Unlock()
	if old != nil {
		go s.endRoom(old, nil)
	}
	vconn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		s.endRoom(rm, nil)
		return nil, err
	}
	aconn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		_ = vconn.Close()
		s.endRoom(rm, nil)
		return nil, err
	}
	// Big socket buffers: a key frame comes as a burst of a few hundred packets
	_ = vconn.SetReadBuffer(4 << 20)
	_ = aconn.SetReadBuffer(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	p := &tvPlayer{tm: tm, rm: rm, video: video, audio: audio, vconn: vconn, aconn: aconn, ctx: ctx, cancel: cancel, watched: time.Now(), idx: -1}
	go p.read(vconn, video, &rtpRewriter{clock: 90000}, true)
	go p.read(aconn, audio, &rtpRewriter{clock: 48000}, false)
	go p.idleWatch()
	log.Printf("[%s] TV on", rm.name)
	return p, nil
}

// play switches to channel ch: the ffmpeg playing now is stopped before the next one starts, the room and its
// viewers stay.
func (p *tvPlayer) play(ch tvChannel, idx int) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if p.runCancel != nil {
		p.runCancel()
	}
	prev := p.runDone
	ctx, cancel := context.WithCancel(p.ctx)
	done := make(chan struct{})
	p.runCancel, p.runDone, p.runCtx = cancel, done, ctx
	p.idx, p.err, p.noVideo = idx, "", false
	p.watched = time.Now() // whoever switched gets the full idle time to start watching
	p.mu.Unlock()
	p.live.Store(false)
	p.rm.localLive.Store(false)
	p.tm.changed()
	p.runs.Add(1)
	go func() {
		defer p.runs.Done()
		defer close(done)
		if prev != nil {
			<-prev
		}
		p.run(ctx, ch)
	}()
}

// status: the channel playing, whether its picture comes, and why not.
func (p *tvPlayer) status() (idx int, live bool, err string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idx, p.live.Load(), p.err
}

// close turns the TV off: ffmpeg killed, the room ended (its viewers told), the ports closed. Never called with
// tm.mu held (ending the room tells the hub).
func (p *tvPlayer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	p.live.Store(false)
	p.rm.localLive.Store(false)
	p.tm.s.endRoom(p.rm, nil)
	_ = p.vconn.Close()
	_ = p.aconn.Close()
	log.Printf("[%s] TV off", p.rm.name)
}

// wait returns once no ffmpeg of this TV runs any more.
func (p *tvPlayer) wait() { p.runs.Wait() }

func (p *tvPlayer) setErr(ctx context.Context, why string) {
	p.mu.Lock()
	if ctx.Err() != nil || p.closed {
		p.mu.Unlock()
		return
	}
	p.err = why
	p.mu.Unlock()
	p.tm.changed()
}

func (p *tvPlayer) run(ctx context.Context, ch tvChannel) {
	var noAudio, noVideo bool
	tries := 0
	for {
		start := time.Now()
		why, missing := p.ffmpeg(ctx, ch, noAudio, noVideo)
		if ctx.Err() != nil {
			return
		}
		// A channel without sound (or a radio without picture): that output is left out, at once
		if missing == "audio" && !noAudio {
			noAudio = true
			continue
		}
		if missing == "video" && !noVideo {
			noVideo = true
			p.mu.Lock()
			p.noVideo = true
			p.mu.Unlock()
			continue
		}
		// It played a good while: a new failure starts counting again
		if p.lastPkt.Load() > start.UnixNano() && time.Since(start) > time.Minute {
			tries = 0
		}
		tries++
		if why == "" {
			why = "the channel stopped"
		}
		p.live.Store(false)
		p.rm.localLive.Store(false)
		if tries >= tvMaxTries {
			p.setErr(ctx, fmt.Sprintf("%s (gave up after %d tries)", why, tries))
			log.Printf("[%s] ffmpeg gave up after %d tries: %s", p.rm.name, tries, why)
			return
		}
		wait := min(time.Duration(1<<min(tries, 5))*time.Second, 30*time.Second)
		p.setErr(ctx, why)
		log.Printf("[%s] ffmpeg ended (%s); again in %v", p.rm.name, why, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// ffmpeg runs one ffmpeg for ch until it ends, ctx does, or nothing comes from it for too long: why it ended
// (from its messages), and "audio" or "video" when the channel has no such stream.
func (p *tvPlayer) ffmpeg(parent context.Context, ch tvChannel, noAudio, noVideo bool) (why, missing string) {
	path, err := p.tm.ffmpegPath()
	if err != nil {
		return err.Error(), ""
	}
	proxy, err := p.tm.proxyURL()
	if err != nil {
		return "the TV's proxy didn't start: " + err.Error(), ""
	}
	mode, vaProf := p.tm.resolveMode()
	cfg := p.tm.s.cfg
	o := tvOpts{
		mode: mode, url: ch.URL, agent: ch.Agent, ref: ch.Ref,
		vport: p.vconn.LocalAddr().(*net.UDPAddr).Port, aport: p.aconn.LocalAddr().(*net.UDPAddr).Port,
		noAudio: noAudio, noVideo: noVideo, vaDev: p.tm.vaDev, vaProf: vaProf, kbps: cfg.tvKbps, height: cfg.tvHeight,
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, tvArgs(o)...)
	// Its own small environment: the proxy, nothing of the server's (no secrets, no other proxy)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "http_proxy=" + proxy, "no_proxy="}
	ownGroup(cmd)
	cmd.Cancel = func() error { return signalGroup(cmd, false) } // ffmpeg ends cleanly on SIGTERM
	cmd.WaitDelay = 3 * time.Second                              // else it's killed
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err.Error(), ""
	}
	if err := cmd.Start(); err != nil {
		return "ffmpeg didn't start: " + redactURLs(err.Error()), ""
	}
	defer func() { _ = signalGroup(cmd, true) }() // anything it started, gone too
	// The watchdog: nothing for a while (a dead source that keeps the connection open) and it's killed
	started := time.Now()
	var stalled atomic.Value
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				last := time.Unix(0, p.lastPkt.Load())
				switch {
				case last.Before(started) && now.Sub(started) > tvFirstPacket:
					stalled.Store("nothing came from the channel")
				case !last.Before(started) && now.Sub(last) > tvStall:
					stalled.Store("the channel stopped sending")
				default:
					continue
				}
				cancel()
				return
			}
		}
	}()
	var last string
	lim := &logLimit{every: 30 * time.Second, n: 12}
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 16<<10), 64<<10)
	for sc.Scan() {
		line := redactURLs(strings.TrimSpace(sc.Text()))
		if line == "" {
			continue
		}
		if strings.Contains(line, "does not contain any stream") || strings.Contains(line, "matches no streams") {
			switch {
			case strings.Contains(line, "#1") || strings.Contains(line, "0:a"):
				missing = "audio"
			case strings.Contains(line, "#0") || strings.Contains(line, "0:v"):
				missing = "video"
			}
		}
		if looksLikeError(line) {
			last = line
		}
		if lim.ok() {
			log.Printf("tv: [%s] %s", p.rm.name, line)
		}
	}
	err = cmd.Wait()
	if parent.Err() == nil && p.tm.proxyBlocked() >= started.UnixNano() {
		return errTVBlocked.Error(), missing
	}
	if s, ok := stalled.Load().(string); ok && parent.Err() == nil {
		return s, missing
	}
	if last == "" && err != nil && parent.Err() == nil {
		last = "ffmpeg: " + err.Error()
	}
	return trimWhy(last), missing
}

// read takes ffmpeg's RTP packets from one port into a track.
func (p *tvPlayer) read(conn *net.UDPConn, track *webrtc.TrackLocalStaticRTP, rw *rtpRewriter, video bool) {
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		// Only ffmpeg, on this machine
		if !from.IP.IsLoopback() {
			continue
		}
		var pkt rtp.Packet
		if pkt.Unmarshal(buf[:n]) != nil {
			continue
		}
		rw.rewrite(&pkt, time.Now())
		_ = track.WriteRTP(&pkt)
		p.rm.bytes.Add(int64(n))
		statStreamIn.Add(int64(n))
		p.lastPkt.Store(time.Now().UnixNano())
		if !p.live.Load() {
			p.mu.Lock()
			radio := p.noVideo
			p.mu.Unlock()
			if video || radio {
				p.wentLive()
			}
		}
	}
}

func (p *tvPlayer) wentLive() {
	p.mu.Lock()
	ctx := p.runCtx
	if ctx == nil || p.closed || ctx.Err() != nil || p.live.Swap(true) {
		p.mu.Unlock()
		return
	}
	p.err = ""
	p.mu.Unlock()
	p.rm.localLive.Store(true)
	p.tm.changed()
	log.Printf("[%s] live", p.rm.name)
}

// idleWatch turns the TV off once nobody has watched for a while (an IPTV stream costs the provider's connection
// and the server's CPU, even for nobody); the channel chosen stays, the next viewer starts it again.
func (p *tvPlayer) idleWatch() {
	t := time.NewTicker(p.tm.idleTick)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
		}
		p.rm.mu.Lock()
		n := len(p.rm.subs)
		p.rm.mu.Unlock()
		p.mu.Lock()
		if n > 0 {
			p.watched = time.Now()
		}
		idle := time.Since(p.watched) > p.tm.idle
		p.mu.Unlock()
		if idle {
			log.Printf("[%s] nobody watched for %v", p.rm.name, p.tm.idle)
			p.tm.playerIdle(p)
			return
		}
	}
}

// ====== RTP continuity ======

// rtpRewriter keeps sequence numbers and timestamps going on across ffmpeg restarts and channel switches (each new
// ffmpeg starts at random ones): the viewers' players see one stream that went on, not a jump they'd take as loss.
type rtpRewriter struct {
	clock   uint32
	started bool
	src     uint32 // the SSRC of the ffmpeg sending now
	seqOff  uint16
	tsOff   uint32
	lastSeq uint16
	lastTS  uint32
	lastAt  time.Time
}

func (r *rtpRewriter) rewrite(p *rtp.Packet, now time.Time) {
	if !r.started || p.SSRC != r.src {
		seq, ts := p.SequenceNumber, p.Timestamp
		if r.started {
			// Going on from the last packet, as much later as the time that passed
			seq = r.lastSeq + 1
			gap := now.Sub(r.lastAt)
			if gap < 0 {
				gap = 0
			}
			ts = r.lastTS + uint32(gap.Seconds()*float64(r.clock)) + 1
		}
		r.seqOff = seq - p.SequenceNumber
		r.tsOff = ts - p.Timestamp
		r.src = p.SSRC
		r.started = true
	}
	p.SequenceNumber += r.seqOff
	p.Timestamp += r.tsOff
	r.lastSeq, r.lastTS, r.lastAt = p.SequenceNumber, p.Timestamp, now
}

// ====== ffmpeg ======

type tvOpts struct {
	mode             string // "x264", "vaapi"
	url, agent, ref  string
	vport, aport     int
	noAudio, noVideo bool
	vaDev, vaProf    string
	kbps, height     int
}

// What ffmpeg may open and read: http(s) only (through the proxy), and the formats IPTV channels come in. No
// file, no raw tcp/udp, no DASH or concat lists that could name other protocols.
const (
	tvProtocols = "http,https,tls,tcp,crypto,hls,httpproxy"
	tvFormats   = "hls,mpegts,mov,mp4,m4a,flv,live_flv,matroska,webm,aac,mp3,ac3,eac3,h264,hevc,mpeg,mpegvideo,ogg,wav"
)

// tvArgs is ffmpeg's command line: the channel in, H.264 to vport and Opus to aport as RTP (small packets, for
// WebRTC), at the pace it plays (-re: a live HLS channel starts a few segments back and would otherwise come as a
// burst of half a minute).
func tvArgs(o tvOpts) []string {
	if o.kbps <= 0 {
		o.kbps = 3000
	}
	o.kbps = min(o.kbps, tvKbpsMax)
	if o.height <= 0 {
		o.height = 720
	}
	o.height = min(max(o.height, 240), tvHeightMax) &^ 1
	width := (o.height * 16 / 9) &^ 1
	kb := strconv.Itoa(o.kbps) + "k"
	a := []string{"-hide_banner", "-nostdin", "-loglevel", "warning"}
	if o.mode == "vaapi" && !o.noVideo {
		a = append(a, "-vaapi_device", o.vaDev)
	}
	agent := o.agent
	if agent == "" {
		agent = tvUserAgent
	}
	a = append(a, "-protocol_whitelist", tvProtocols, "-format_whitelist", tvFormats,
		"-re", "-fflags", "+genpts+discardcorrupt",
		"-user_agent", agent, "-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_on_network_error", "1",
		"-reconnect_delay_max", "5", "-rw_timeout", "15000000")
	if o.ref != "" {
		a = append(a, "-referer", o.ref)
	}
	a = append(a, "-i", o.url)
	keys := []string{"-g", "60", "-force_key_frames", "expr:gte(t,n_forced*2)"} // a key frame every 2 s: a viewer joining waits at most that
	rate := []string{"-b:v", kb, "-maxrate", kb, "-bufsize", kb}
	fit := fmt.Sprintf("w='min(iw,%d)':h='min(ih,%d)':force_original_aspect_ratio=decrease", width, o.height)
	if !o.noVideo {
		a = append(a, "-map", "0:v:0?", "-an", "-sn", "-dn")
		switch o.mode {
		case "vaapi":
			a = append(a, "-vf", "yadif=deint=interlaced,format=nv12,hwupload,scale_vaapi="+fit,
				"-fpsmax", strconv.Itoa(tvFPSMax), "-c:v", "h264_vaapi", "-profile:v", o.vaProf, "-bf", "0")
			a = append(a, keys...)
			a = append(a, rate...)
		default:
			a = append(a, "-vf", "yadif=deint=interlaced,scale="+fit+":force_divisible_by=2,format=yuv420p",
				"-fpsmax", strconv.Itoa(tvFPSMax),
				"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-profile:v", "baseline", "-bf", "0")
			a = append(a, keys...)
			a = append(a, rate...)
		}
		a = append(a, "-f", "rtp", "-payload_type", "102", fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", o.vport))
	}
	if !o.noAudio {
		a = append(a, "-map", "0:a:0?", "-vn", "-sn", "-dn", "-c:a", "libopus", "-ar", "48000", "-ac", "2", "-b:a", "128k",
			"-f", "rtp", "-payload_type", "111", fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", o.aport))
	}
	return a
}

var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s'"]+`)
var credsInText = regexp.MustCompile(`[^\s/'"@]+:[^\s/'"@]+@`)

// redactURLs takes addresses (and user:password@ parts) out of ffmpeg's messages: they hold the subscription's
// credentials.
func redactURLs(s string) string {
	s = urlInText.ReplaceAllString(s, "<address>")
	return credsInText.ReplaceAllString(s, "<credentials>@")
}

func looksLikeError(line string) bool {
	l := strings.ToLower(line)
	for _, w := range []string{"error", "failed", "invalid", "forbidden", "not found", "refused", "timed out", "unauthorized", "no such", "could not", "unable", "403", "404", "401"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

func trimWhy(s string) string {
	// "[http @ 0x55d…] " says nothing to a person
	if i := strings.Index(s, "] "); strings.HasPrefix(s, "[") && i > 0 {
		s = s[i+2:]
	}
	return clip(s, 200)
}

// logLimit lets n lines through every period (ffmpeg can complain every frame).
type logLimit struct {
	every   time.Duration
	n       int
	since   time.Time
	used    int
	dropped int
}

func (l *logLimit) ok() bool {
	now := time.Now()
	if now.Sub(l.since) > l.every {
		if l.dropped > 0 {
			log.Printf("tv: (%d more ffmpeg lines not shown)", l.dropped)
		}
		l.since, l.used, l.dropped = now, 0, 0
	}
	if l.used < l.n {
		l.used++
		return true
	}
	l.dropped++
	return false
}

// ====== Which encoder ======

func (tm *tvManager) ffmpegPath() (string, error) {
	name := tm.s.cfg.ffmpeg
	if name == "" {
		name = "ffmpeg"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", errors.New("ffmpeg isn't installed on the server (apt install ffmpeg, or SV_FFMPEG)")
	}
	return path, nil
}

// resolveMode works out how the TV encodes, once: SV_TV_VIDEO auto takes the graphics card (VAAPI) when a short
// test encode on it works, else x264.
func (tm *tvManager) resolveMode() (mode, vaProf string) {
	tm.modeOnce.Do(func() {
		want := strings.ToLower(strings.TrimSpace(tm.s.cfg.tvVideo))
		if want == "" {
			want = "auto"
		}
		dev := tm.s.cfg.tvVaapi
		if dev == "" {
			dev = "/dev/dri/renderD128"
		}
		switch want {
		case "x264":
			tm.mode = "x264"
		case "vaapi", "auto":
			path, err := tm.ffmpegPath()
			prof := ""
			if _, serr := os.Stat(dev); err == nil && serr == nil {
				prof = probeVaapi(path, dev)
			}
			switch {
			case prof != "":
				tm.mode, tm.vaProf = "vaapi", prof
			case want == "vaapi":
				tm.mode, tm.vaProf = "vaapi", "main"
				log.Printf("tv: !!! SV_TV_VIDEO=vaapi, but a test encode on %s didn't work; trying anyway", dev)
			default:
				tm.mode = "x264"
			}
		default:
			log.Printf("tv: unknown SV_TV_VIDEO %q, using x264", want)
			tm.mode = "x264"
		}
		tm.vaDev = dev
		switch tm.mode {
		case "vaapi":
			log.Printf("tv: video encoded on the graphics card (VAAPI %s, H.264 %s)", dev, tm.vaProf)
		default:
			log.Printf("tv: video encoded by x264 in software (up to %dp%d)", min(max(tm.s.cfg.tvHeight, 240), tvHeightMax), tvFPSMax)
		}
	})
	return tm.mode, tm.vaProf
}

// probeVaapi encodes a second of test picture on the card: the H.264 profile that worked, or "".
func probeVaapi(ffmpeg, dev string) string {
	for _, prof := range []string{"constrained_baseline", "main"} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-vaapi_device", dev,
			"-f", "lavfi", "-i", "testsrc=size=640x360:rate=30", "-t", "1",
			"-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi", "-profile:v", prof, "-bf", "0", "-f", "null", "-")
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			return prof
		}
		log.Printf("tv: VAAPI test (%s): %v %s", prof, err, strings.TrimSpace(string(out)))
	}
	return ""
}
