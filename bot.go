package main

// The music bot: one per community, in a voice channel like a person. It plays what people ask for in the chat
// (yt-dlp finds it, ffmpeg makes Opus of it) and the server sends that as the bot's voice, so every app hears it
// with nothing to install.
//
//	!play <link or words>   queue a song (the bot comes into your channel first)
//	!skip                   next song (those allowed at once; others by a vote of half the channel)
//	!pause !resume !stop    (those allowed, or whoever asked for the song playing)
//	!queue  !np  !vol 1-100  !join
//	<- {"t":"music","cmd":"play","q":…}   the same from the apps' buttons
//
// Needs yt-dlp and ffmpeg (install.sh installs them). Nothing is kept on disk: ffmpeg streams into the bot.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	botName     = "🎵 Music"
	botFrame    = 20 * time.Millisecond
	botIdle     = 3 * time.Minute // with nothing queued, the bot leaves after this
	maxSongTime = 3 * time.Hour
)

type song struct {
	Title string `json:"title"`
	URL   string `json:"-"`
	Page  string `json:"page,omitempty"`
	By    string `json:"by"`
	ByUID string `json:"-"`
	Secs  int    `json:"secs,omitempty"`
}

type musicBot struct {
	mb    *musicBots
	comm  int64
	ss    *session
	track *webrtc.TrackLocalStaticSample

	mu      sync.Mutex
	queue   []song
	now     *song
	paused  bool
	volume  int // 1..100
	votes   map[string]bool
	cancel  context.CancelFunc // the song playing
	wake    chan struct{}
	stopped bool
}

type musicBots struct {
	s    *server
	mu   sync.Mutex
	bots map[int64]*musicBot
	ok   bool
	// resolveFn replaces yt-dlp in tests
	resolveFn func(q string) (song, error)
}

func newMusicBots(s *server) *musicBots {
	mb := &musicBots{s: s, bots: map[int64]*musicBot{}}
	if s.cfg.music {
		_, e1 := exec.LookPath(s.cfg.ytdlp)
		_, e2 := exec.LookPath(s.cfg.ffmpeg)
		mb.ok = e1 == nil && e2 == nil
		if !mb.ok {
			log.Printf("music bot off: yt-dlp or ffmpeg not found")
		}
	}
	return mb
}

func (mb *musicBots) enabled() bool { return mb != nil && mb.ok }

// joined: someone came into a channel; nothing to do for now (the bot follows !join only).
func (mb *musicBots) joined(*session, int64) {}

// botOf gives the community's bot, made in channel ch when there's none.
func (mb *musicBots) botOf(comm, ch int64) (*musicBot, error) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if b := mb.bots[comm]; b != nil {
		return b, nil
	}
	h := mb.s.hub
	b := &musicBot{mb: mb, comm: comm, volume: 60, votes: map[string]bool{}, wake: make(chan struct{}, 1)}
	ss := &session{uid: fmt.Sprintf("bot-music-%d", comm), name: botName, client: "bot/" + version, bot: true, comm: comm, ch: ch,
		out: make(chan []byte, sessQueue), done: make(chan struct{}), since: time.Now().UnixMilli()}
	go func() { // a bot reads nothing it's sent
		for {
			select {
			case <-ss.out:
			case <-ss.done:
				return
			}
		}
	}()
	h.mu.Lock()
	h.next++
	ss.sid = h.next
	h.mu.Unlock()
	// Its stream id names its session, like everyone's voice
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		SDPFmtpLine: opusFmtp}, "a"+itoa(ss.sid), "s"+itoa(ss.sid))
	if err != nil {
		close(ss.done)
		return nil, err
	}
	b.ss, b.track = ss, track
	h.mu.Lock()
	h.sessions[ss.sid] = ss
	h.toComm(comm, map[string]any{"t": "member", "m": ss.member()})
	h.mu.Unlock()
	mb.s.voice.addBot(ss, track)
	mb.bots[comm] = b
	go b.run()
	log.Printf("music: bot %d in community %d", ss.sid, comm)
	return b, nil
}

func (mb *musicBots) remove(b *musicBot) {
	mb.mu.Lock()
	if mb.bots[b.comm] == b {
		delete(mb.bots, b.comm)
	}
	mb.mu.Unlock()
	mb.s.hub.leave(b.ss)
}

// say posts the bot's line in its channel's chat (not kept: it's only a note).
func (b *musicBot) say(text string) {
	h := b.mb.s.hub
	h.mu.Lock()
	ch := b.ss.ch
	msg := &chatMsg{ID: -time.Now().UnixNano(), To: chanConv(ch), From: b.ss.uid, Name: botName, Text: text, At: time.Now().UnixMilli()}
	h.deliver(map[string]any{"t": "msg", "m": msg}, ch, "", "")
	h.mu.Unlock()
}

func (b *musicBot) status() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{"t": "music", "sid": b.ss.sid, "ch": b.ss.ch, "now": b.now, "queue": b.queue, "paused": b.paused, "volume": b.volume}
}

func (b *musicBot) tell() {
	st := b.status()
	h := b.mb.s.hub
	h.mu.Lock()
	h.toComm(b.comm, st)
	h.mu.Unlock()
}

func (b *musicBot) poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// run plays the queue, one song at a time, and leaves when idle.
func (b *musicBot) run() {
	idle := time.NewTimer(botIdle)
	defer idle.Stop()
	for {
		b.mu.Lock()
		if b.stopped {
			b.mu.Unlock()
			b.mb.remove(b)
			return
		}
		var next *song
		if len(b.queue) > 0 && !b.paused {
			s := b.queue[0]
			b.queue = b.queue[1:]
			next = &s
		}
		b.now = next
		b.votes = map[string]bool{}
		b.mu.Unlock()
		if next == nil {
			b.tell()
			select {
			case <-b.wake:
				idle.Reset(botIdle)
				continue
			case <-idle.C:
				b.say("Plec, nu mai e nimic în coadă.")
				b.mu.Lock()
				b.stopped = true
				b.mu.Unlock()
				continue
			}
		}
		b.tell()
		b.say("▶ " + next.Title + " · cerut de " + next.By)
		if err := b.play(next); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("music: %s: %v", next.Title, err)
			b.say("Nu pot reda „" + next.Title + "”.")
		}
		idle.Reset(botIdle)
	}
}

// play streams one song: ffmpeg makes 20 ms Opus packets (in Ogg), sent at their own pace.
func (b *musicBot) play(sg *song) error {
	ctx, cancel := context.WithTimeout(context.Background(), maxSongTime)
	b.mu.Lock()
	b.cancel = cancel
	vol := b.volume
	b.mu.Unlock()
	defer cancel()
	s := b.mb.s
	h := s.hub
	h.mu.Lock()
	kbps := 96
	if c := h.chans[b.ss.ch]; c != nil {
		kbps = audioLevels[max(s.channelAudio(c), 4)-1].Kbps
	}
	h.mu.Unlock()
	args := append([]string{"-hide_banner", "-loglevel", "error"}, ffmpegInput(sg.URL)...)
	args = append(args, "-vn", "-af", fmt.Sprintf("volume=%.2f,aresample=48000", float64(vol)/100),
		"-c:a", "libopus", "-b:a", strconv.Itoa(kbps)+"k", "-ac", "2", "-application", "audio", "-frame_duration", "20",
		"-f", "ogg", "-page_duration", "20000", "pipe:1")
	cmd := exec.CommandContext(ctx, s.cfg.ffmpeg, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	r := newOggPackets(bufio.NewReaderSize(out, 64<<10))
	next := time.Now()
	for {
		pkt, err := r.next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for {
			b.mu.Lock()
			p := b.paused
			b.mu.Unlock()
			if !p || ctx.Err() != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
			next = time.Now()
		}
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		}
		next = next.Add(botFrame)
		if err := b.track.WriteSample(media.Sample{Data: pkt, Duration: botFrame}); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		s.voice.botHot(b.ss.sid)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// ffmpegInput: how ffmpeg opens a song (tests give it a generated tone).
var ffmpegInput = defaultFFmpegInput

func defaultFFmpegInput(u string) []string {
	return []string{"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "4", "-i", u}
}

// oggPackets reads Opus packets out of an Ogg stream (the two header packets skipped). One buffer, reused.
type oggPackets struct {
	r       *bufio.Reader
	hdr     [27]byte
	seg     [255]byte
	pending []byte // a packet that goes on into the next page
	page    []byte
	buf     []byte // reused for every page
	lens    []int
	at      int
	skipped int
}

func newOggPackets(r *bufio.Reader) *oggPackets { return &oggPackets{r: r} }

func (o *oggPackets) next() ([]byte, error) {
	for {
		if o.at < len(o.lens) {
			off := 0
			for i := 0; i < o.at; i++ {
				off += o.lens[i]
			}
			p := o.page[off : off+o.lens[o.at]]
			o.at++
			if o.skipped < 2 {
				o.skipped++
				continue
			}
			return p, nil
		}
		if err := o.readPage(); err != nil {
			return nil, err
		}
	}
}

func (o *oggPackets) readPage() error {
	if _, err := io.ReadFull(o.r, o.hdr[:]); err != nil {
		return err
	}
	if string(o.hdr[:4]) != "OggS" {
		return errors.New("not an Ogg stream")
	}
	n := int(o.hdr[26])
	if _, err := io.ReadFull(o.r, o.seg[:n]); err != nil {
		return err
	}
	total := 0
	for _, l := range o.seg[:n] {
		total += int(l)
	}
	need := len(o.pending) + total
	if cap(o.buf) < need {
		o.buf = make([]byte, need, need*2)
	}
	body := o.buf[:need]
	copy(body, o.pending)
	if _, err := io.ReadFull(o.r, body[len(o.pending):]); err != nil {
		return err
	}
	o.lens = o.lens[:0]
	cur := len(o.pending)
	o.pending = nil
	for i, l := range o.seg[:n] {
		cur += int(l)
		if l < 255 {
			o.lens = append(o.lens, cur)
			cur = 0
		} else if i == n-1 {
			// The last packet goes on in the next page
			start := len(body) - cur
			o.pending = append([]byte(nil), body[start:]...)
			body = body[:start]
		}
	}
	o.page, o.at = body, 0
	return nil
}

// resolve asks yt-dlp for a song's title, page and stream address.
func (mb *musicBots) resolve(q string) (song, error) {
	if mb.resolveFn != nil {
		return mb.resolveFn(q)
	}
	target := q
	if !strings.HasPrefix(q, "http://") && !strings.HasPrefix(q, "https://") {
		target = "ytsearch1:" + q
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, mb.s.cfg.ytdlp, "--no-playlist", "--no-warnings", "-f", "bestaudio/best",
		"--print", "%(title)s\n%(webpage_url)s\n%(duration)s\n%(url)s", "--", target).Output()
	if err != nil {
		return song{}, errors.New("not found")
	}
	l := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(l) < 4 {
		return song{}, errors.New("not found")
	}
	secs, _ := strconv.ParseFloat(l[2], 64)
	return song{Title: clip(l[0], 200), Page: l[1], Secs: int(secs), URL: l[3]}, nil
}

// command runs a "!…" line said in a channel's chat; true when it was one of the bot's.
func (mb *musicBots) command(ss *session, ch int64, text string) bool {
	if !mb.enabled() || !strings.HasPrefix(text, "!") {
		return false
	}
	cmd, arg, _ := strings.Cut(strings.TrimSpace(text[1:]), " ")
	return mb.run(ss, ch, strings.ToLower(cmd), strings.TrimSpace(arg))
}

func (mb *musicBots) op(ss *session, t string, m map[string]any) bool {
	if t != "music" {
		return false
	}
	if !mb.enabled() {
		ss.refuse(t, "off", "this server has no music bot")
		return true
	}
	h := mb.s.hub
	h.mu.Lock()
	ch := ss.ch
	h.mu.Unlock()
	if !mb.run(ss, ch, str(m, "cmd"), str(m, "q")) {
		mb.mu.Lock()
		b := mb.bots[ss.comm]
		mb.mu.Unlock()
		if b != nil {
			ss.send(b.status())
		} else {
			ss.send(map[string]any{"t": "music"})
		}
	}
	return true
}

func (mb *musicBots) run(ss *session, ch int64, cmd, arg string) bool {
	h := mb.s.hub
	mb.mu.Lock()
	b := mb.bots[ss.comm]
	mb.mu.Unlock()
	h.mu.Lock()
	boss := h.may(ss, pMusic)
	listeners := 0
	if b != nil {
		for _, o := range h.sessions {
			if o.ch == b.ss.ch && !o.bot {
				listeners++
			}
		}
	}
	h.mu.Unlock()
	switch cmd {
	case "play", "p":
		if arg == "" {
			return true
		}
		lim := mb.s.limits().MusicQueue
		if b != nil {
			b.mu.Lock()
			full := len(b.queue) >= lim
			b.mu.Unlock()
			if full {
				ss.refuse("music", "full", "the queue is full")
				return true
			}
		}
		go func() {
			sg, err := mb.resolve(arg)
			if err != nil {
				ss.refuse("music", "gone", "nothing found for "+clip(arg, 60))
				return
			}
			sg.By, sg.ByUID = ss.name, ss.uid
			bot, err := mb.botOf(ss.comm, ch)
			if err != nil {
				ss.refuse("music", "server", err.Error())
				return
			}
			bot.mu.Lock()
			bot.queue = append(bot.queue, sg)
			n := len(bot.queue)
			playing := bot.now != nil
			bot.mu.Unlock()
			if playing {
				bot.say(fmt.Sprintf("➕ %s (în coadă: %d)", sg.Title, n))
			}
			bot.poke()
			bot.tell()
		}()
	case "join":
		if b == nil {
			return true
		}
		h.mu.Lock()
		old := b.ss.ch
		if c := h.chans[ch]; c != nil && c.Comm == b.comm {
			b.ss.ch = ch
			h.memberChanged(b.ss)
		}
		h.mu.Unlock()
		mb.s.voice.sync(old, ch)
	case "skip", "s", "next":
		if b == nil {
			return true
		}
		b.mu.Lock()
		mine := b.now != nil && b.now.ByUID == ss.uid
		b.votes[ss.uid] = true
		need := (listeners + 1) / 2
		got := len(b.votes)
		c := b.cancel
		b.mu.Unlock()
		if boss || mine || got >= need {
			if c != nil {
				c()
			}
		} else {
			b.say(fmt.Sprintf("⏭ %s vrea mai departe (%d/%d)", ss.name, got, need))
		}
	case "pause", "resume", "stop":
		if b == nil {
			return true
		}
		b.mu.Lock()
		mine := b.now != nil && b.now.ByUID == ss.uid
		if !boss && !mine && listeners > 1 {
			b.mu.Unlock()
			ss.refuse("music", "role", "not allowed")
			return true
		}
		switch cmd {
		case "pause":
			b.paused = true
		case "resume":
			b.paused = false
		case "stop":
			b.queue, b.stopped, b.paused = nil, true, false
			if b.cancel != nil {
				b.cancel()
			}
		}
		b.mu.Unlock()
		b.poke()
		b.tell()
	case "vol", "volume":
		if b == nil {
			return true
		}
		v, err := strconv.Atoi(arg)
		if err != nil || v < 1 || v > 100 {
			return true
		}
		b.mu.Lock()
		b.volume = v
		b.mu.Unlock()
		b.say(fmt.Sprintf("🔊 %d%% de la următoarea melodie", v))
		b.tell()
	case "queue", "q", "np":
		if b != nil {
			ss.send(b.status())
		}
		return false
	default:
		return false
	}
	return true
}
