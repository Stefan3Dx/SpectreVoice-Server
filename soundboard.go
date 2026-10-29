package main

// The server's soundboard: short sounds kept on the server (data/sounds/<sha256>.opus), uploaded by those with
// "soundboard_manage" (admins by default) and played by those with "soundboard" (@everyone by default) into their
// voice channel. A play isn't mixed into anyone's voice: the server tells everyone in the channel which sound (by its
// hash), and each app plays its own copy, fetched once and kept by hash, unless its person muted soundboard sounds.
//
// An upload (OGG/Opus, MP3 or WAV, by its bytes) is turned by ffmpeg into mono 48 kHz Opus in Ogg, loudness evened
// out, at most 10 s (longer ones are cut); a server keeps at most SV_SOUNDS_MAX sounds (100).
//
//	<- {"t":"sb","op":"list"}                          -> {"t":"sb","items":[SOUND…],"max":N}
//	<- {"t":"sb","op":"play","id":ID}                  (soundboard, in a voice channel, heard there)
//	-> {"t":"sb-play","id","hash","name","emoji","ms","by":UID,"byName","sid","ch"}   to everyone in that channel
//	<- {"t":"sb","op":"edit","id":ID,"name":N,"emoji":E} (soundboard_manage)
//	<- {"t":"sb","op":"del","id":ID}                   (soundboard_manage)
//	-> {"t":"sb","items":[…],"max":N}                  to everyone, whenever the list changes (and in the welcome as "sb")
//	-> {"t":"error","op":"sb","why":"role"|"off"|"channel"|"cooldown"|"rate"|"busy"|"gone"|"bad","wait"?:ms,"message":…}
//
//	SOUND = {"id","hash","name","emoji","ms","size","by","byName","at"}
//
//	POST /sb/upload?token=T&name=N&emoji=E   (soundboard_manage) the body is the file -> {"id","hash","ms","cut"}
//	GET  /sb/f/<hash>.opus?token=T           the sound (Ogg Opus; never changes: cached for good by its hash)

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	sbMaxSeconds = 10
	sbInMax      = 8 << 20   // the file uploaded
	sbOutMax     = 512 << 10 // the Opus kept (10 s at 64 kbit/s is ~80 KB)
	sbNameMax    = 32
	sbDefaultMax = 100
)

// Vars so tests can change them.
var (
	sbCooldown    = 3 * time.Second        // one person: one sound, then this long before the next
	sbPerMinute   = 12                     // one person's sounds in a minute
	sbChannelGap  = 700 * time.Millisecond // one channel: sounds at most this often (several people at once)
	sbUploadsPer  = 10                     // one person's uploads in 10 minutes
	sbTranscodeTO = 30 * time.Second
)

const soundsSchema = `CREATE TABLE IF NOT EXISTS sounds(id INTEGER PRIMARY KEY, hash TEXT NOT NULL, name TEXT NOT NULL, emoji TEXT NOT NULL DEFAULT '', ms INTEGER NOT NULL, size INTEGER NOT NULL, by_uid TEXT NOT NULL DEFAULT '', by_name TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL)`

type sound struct {
	ID     int64  `json:"id"`
	Hash   string `json:"hash"`
	Name   string `json:"name"`
	Emoji  string `json:"emoji"`
	Ms     int    `json:"ms"`
	Size   int    `json:"size"`
	By     string `json:"by"`
	ByName string `json:"byName,omitempty"`
	At     int64  `json:"at"`
}

type sbUse struct {
	last   time.Time
	recent []time.Time // plays in the last minute
}

type soundboard struct {
	s       *server
	dir     string
	mu      sync.Mutex
	list    []sound
	uses    map[string]*sbUse      // per person
	chLast  map[int64]time.Time    // per channel
	uploads map[string][]time.Time // per person, the last 10 minutes
	sem     chan struct{}          // transcodes at once
	encode  func(ctx context.Context, data []byte, format string) ([]byte, float64, error)
}

var sbHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func newSoundboard(s *server) (*soundboard, error) {
	sb := &soundboard{s: s, dir: filepath.Join(s.cfg.data, "sounds"), uses: map[string]*sbUse{}, chLast: map[int64]time.Time{}, uploads: map[string][]time.Time{}, sem: make(chan struct{}, 2)}
	sb.encode = sb.ffmpegEncode
	if err := os.MkdirAll(sb.dir, 0o750); err != nil {
		return nil, err
	}
	st := s.store
	if _, err := st.db.Exec(soundsSchema); err != nil {
		return nil, err
	}
	// Roles from before the soundboard: everyone may play, those that could manage the server may upload
	if _, ok := st.setting("perm_sb"); !ok {
		_, _ = st.db.Exec(`UPDATE roles SET perms = perms | ? WHERE everyone=1`, int64(pSoundboard))
		_, _ = st.db.Exec(`UPDATE roles SET perms = perms | ? WHERE perms & ? != 0`, int64(pSoundboardManage|pSoundboard), int64(pManageCommunity))
		_ = st.setSetting("perm_sb", "1")
	}
	return sb, sb.load()
}

func (sb *soundboard) enabled() bool { return sb != nil && !sb.s.cfg.noSounds }

func (sb *soundboard) max() int {
	if n := sb.s.cfg.soundsMax; n > 0 {
		return n
	}
	return sbDefaultMax
}

func (sb *soundboard) load() error {
	rows, err := sb.s.store.db.Query(`SELECT s.id, s.hash, s.name, s.emoji, s.ms, s.size, s.by_uid, COALESCE(NULLIF(s.by_name,''), u.name, ''), s.created FROM sounds s LEFT JOIN users u ON u.uid = s.by_uid ORDER BY s.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var out []sound
	for rows.Next() {
		var x sound
		if err := rows.Scan(&x.ID, &x.Hash, &x.Name, &x.Emoji, &x.Ms, &x.Size, &x.By, &x.ByName, &x.At); err != nil {
			return err
		}
		if x.By == "" {
			x.ByName = ""
		}
		out = append(out, x)
	}
	sb.mu.Lock()
	sb.list = out
	sb.mu.Unlock()
	return rows.Err()
}

// items: the sounds as the apps see them.
func (sb *soundboard) items() []sound {
	if sb == nil {
		return []sound{}
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return append([]sound{}, sb.list...)
}

func (sb *soundboard) byID(id int64) (sound, bool) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	for _, x := range sb.list {
		if x.ID == id {
			return x, true
		}
	}
	return sound{}, false
}

func (sb *soundboard) hasHash(h string) bool {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	for _, x := range sb.list {
		if x.Hash == h {
			return true
		}
	}
	return false
}

// welcome: what the welcome carries as "sb" (nil: none on this server).
func (sb *soundboard) welcome() map[string]any {
	if !sb.enabled() {
		return nil
	}
	return map[string]any{"items": sb.items(), "max": sb.max()}
}

// changed tells everyone the list (after the change is in sb.list).
func (sb *soundboard) changed() {
	h := sb.s.hub
	if h == nil {
		return
	}
	data, _ := json.Marshal(map[string]any{"t": "sb", "items": sb.items(), "max": sb.max()})
	h.mu.Lock()
	for _, ss := range h.sessions {
		if ss.comm == spaceID && !ss.api {
			ss.queue(data)
		}
	}
	h.mu.Unlock()
}

// wiped: a person's uploads stay the server's, without their name (store.wipeUser clears by_uid).
func (sb *soundboard) wiped() {
	if sb == nil {
		return
	}
	_ = sb.load()
	sb.changed()
}

// ====== From the apps ======

var sbWhyText = map[string]string{
	"role": "you aren't allowed to", "off": "this server has no soundboard (SV_SOUNDBOARD=false)", "bad": "unknown soundboard operation",
	"channel": "join a voice channel first", "gone": "no such sound", "busy": "a sound is playing in this channel; a moment",
	"rate": "too many sounds; wait a minute", "mute": "you can't be heard in this channel",
}

func (h *hub) soundboardOp(ss *session, m map[string]any) {
	sb := h.s.sb
	op := str(m, "op")
	refuse := func(why string, extra map[string]any) {
		out := map[string]any{"t": "error", "op": "sb", "what": op, "why": why, "message": sbWhyText[why]}
		for k, v := range extra {
			out[k] = v
		}
		ss.send(out)
	}
	if !sb.enabled() {
		refuse("off", nil)
		return
	}
	switch op {
	case "list":
		ss.send(map[string]any{"t": "sb", "items": sb.items(), "max": sb.max()})
	case "play":
		snd, ok := sb.byID(i64(m, "id"))
		if !ok {
			refuse("gone", nil)
			return
		}
		h.mu.Lock()
		may := h.may(ss, pSoundboard)
		c := h.chans[ss.ch]
		heard := h.maySpeak(ss)
		h.mu.Unlock()
		switch {
		case !may:
			refuse("role", nil)
			return
		case c == nil || c.Kind != "":
			refuse("channel", nil)
			return
		case !heard:
			refuse("mute", nil)
			return
		}
		if why, wait := sb.take(ss.uid, ss.ch, time.Now()); why != "" {
			var extra map[string]any
			if wait > 0 {
				extra = map[string]any{"wait": wait.Milliseconds(), "message": fmt.Sprintf("wait %.1f s", wait.Seconds())}
			}
			refuse(why, extra)
			return
		}
		out, _ := json.Marshal(map[string]any{"t": "sb-play", "id": snd.ID, "hash": snd.Hash, "name": snd.Name, "emoji": snd.Emoji, "ms": snd.Ms,
			"by": ss.uid, "byName": ss.name, "sid": ss.sid, "ch": ss.ch})
		h.mu.Lock()
		ch := ss.ch
		for _, o := range h.sessions {
			if o.ch == ch && o.comm == ss.comm && !o.bot {
				o.queue(out)
			}
		}
		h.mu.Unlock()
	case "edit", "del":
		h.mu.Lock()
		may := !ss.api && h.may(ss, pSoundboardManage)
		h.mu.Unlock()
		if !may {
			refuse("role", nil)
			return
		}
		snd, ok := sb.byID(i64(m, "id"))
		if !ok {
			refuse("gone", nil)
			return
		}
		if op == "del" {
			sb.remove(snd)
		} else {
			name, emoji := sbName(str(m, "name"), snd.Name), sbEmoji(str(m, "emoji"), snd.Emoji)
			_, _ = h.s.store.db.Exec(`UPDATE sounds SET name=?, emoji=? WHERE id=?`, name, emoji, snd.ID)
			_ = sb.load()
		}
		sb.changed()
		ss.send(map[string]any{"t": "done", "op": "sb", "what": op, "id": snd.ID})
	default:
		refuse("bad", nil)
	}
}

// take: may uid play a sound in channel ch now ("" and it's counted), else why, and how long to wait.
func (sb *soundboard) take(uid string, ch int64, now time.Time) (string, time.Duration) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	u := sb.uses[uid]
	if u == nil {
		u = &sbUse{}
		sb.uses[uid] = u
	}
	if left := sbCooldown - now.Sub(u.last); !u.last.IsZero() && left > 0 {
		return "cooldown", left
	}
	keep := u.recent[:0]
	for _, t := range u.recent {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	u.recent = keep
	if len(u.recent) >= sbPerMinute {
		return "rate", time.Minute - now.Sub(u.recent[0])
	}
	if left := sbChannelGap - now.Sub(sb.chLast[ch]); left > 0 {
		return "busy", left
	}
	u.last = now
	u.recent = append(u.recent, now)
	sb.chLast[ch] = now
	// Old entries go now and then (people who left long ago)
	if len(sb.uses) > 4096 {
		for k, v := range sb.uses {
			if now.Sub(v.last) > 2*time.Minute {
				delete(sb.uses, k)
			}
		}
	}
	if len(sb.chLast) > 4096 {
		sb.chLast = map[int64]time.Time{ch: now}
	}
	return "", 0
}

func (sb *soundboard) remove(snd sound) {
	_, _ = sb.s.store.db.Exec(`DELETE FROM sounds WHERE id=?`, snd.ID)
	_ = sb.load()
	if !sb.hasHash(snd.Hash) {
		_ = os.Remove(sb.path(snd.Hash))
	}
	log.Printf("soundboard: sound %d removed", snd.ID)
}

func (sb *soundboard) path(hash string) string { return filepath.Join(sb.dir, hash+".opus") }

// sbName: a sound's name, cleaned (def when nothing is left).
func sbName(n, def string) string {
	n = strings.Join(strings.FieldsFunc(n, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if r := []rune(n); len(r) > sbNameMax {
		n = strings.TrimSpace(string(r[:sbNameMax]))
	}
	if n == "" {
		return def
	}
	return n
}

// sbEmoji: one emoji (or a short symbol): no letters or digits but the regional ones, at most 8 runes and 32 bytes.
func sbEmoji(e, def string) string {
	e = strings.TrimSpace(e)
	if e == "" || len(e) > 32 || utf8.RuneCountInString(e) > 8 || !utf8.ValidString(e) {
		return def
	}
	for _, r := range e {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r < 0x80 && r != '#' && r != '*' && !(r >= '0' && r <= '9') {
			return def
		}
	}
	return e
}

// ====== HTTP ======

// sbFormat: what the upload is, by its first bytes ("" when none of ours).
func sbFormat(b []byte) string {
	switch {
	case len(b) >= 4 && string(b[:4]) == "OggS":
		return "ogg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "wav"
	case len(b) >= 3 && string(b[:3]) == "ID3", len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return "mp3"
	}
	return ""
}

func sbJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) sbViewer(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, ok := s.tokenUID(r)
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return "", false
	}
	if !s.mayUseSpace(mainSpace, uid) {
		http.Error(w, "not on this server", http.StatusForbidden)
		return "", false
	}
	if !s.sb.enabled() {
		http.Error(w, "this server has no soundboard", http.StatusNotFound)
		return "", false
	}
	return uid, true
}

// GET /sb/f/<hash>.opus
func (s *server) handleSoundFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sbViewer(w, r); !ok {
		return
	}
	h := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sb/f/"), ".opus")
	if !sbHashRe.MatchString(h) || !s.sb.hasHash(h) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.sb.path(h))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "audio/ogg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+h+`"`)
	if r.Header.Get("If-None-Match") == `"`+h+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = io.Copy(w, f)
}

// POST /sb/upload
func (s *server) handleSoundUpload(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.sbViewer(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST the sound", http.StatusMethodNotAllowed)
		return
	}
	sb := s.sb
	s.hub.mu.Lock()
	may := s.hub.permsOf(spaceID, uid)&pSoundboardManage != 0
	s.hub.mu.Unlock()
	if !may || s.store.botOf(uid) != nil {
		sbJSON(w, http.StatusForbidden, map[string]string{"error": sbWhyText["role"], "why": "role"})
		return
	}
	if len(sb.items()) >= sb.max() {
		sbJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("the server has %d sounds, as many as it may", sb.max()), "why": "full"})
		return
	}
	if !sb.mayUpload(uid, time.Now()) {
		sbJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many uploads; wait a few minutes", "why": "rate"})
		return
	}
	if r.ContentLength > sbInMax {
		sbJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "the file is bigger than 8 MB", "why": "big"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, sbInMax+1))
	if err != nil {
		sbJSON(w, http.StatusBadRequest, map[string]string{"error": "the upload broke off", "why": "bad"})
		return
	}
	if len(data) > sbInMax {
		sbJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "the file is bigger than 8 MB", "why": "big"})
		return
	}
	format := sbFormat(data)
	if format == "" {
		sbJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send an OGG/Opus, MP3 or WAV file", "why": "format"})
		return
	}
	select {
	case sb.sem <- struct{}{}:
	case <-time.After(10 * time.Second):
		sbJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the server is busy; try again", "why": "busy"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sbTranscodeTO)
	out, inSec, err := sb.encode(ctx, data, format)
	cancel()
	<-sb.sem
	if err != nil {
		why := "bad"
		if errors.Is(err, errSbFFmpeg) {
			why = "ffmpeg"
		}
		sbJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "why": why})
		return
	}
	ms := oggOpusMillis(out)
	if ms < 50 || len(out) > sbOutMax {
		sbJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no sound in the file", "why": "bad"})
		return
	}
	sum := sha256.Sum256(out)
	hash := hex.EncodeToString(sum[:])
	if !sb.hasHash(hash) {
		if err := writeFileAtomic(sb.path(hash), out, 0o640); err != nil {
			sbJSON(w, http.StatusInternalServerError, map[string]string{"error": "the sound couldn't be kept", "why": "server"})
			return
		}
	}
	q := r.URL.Query()
	name := sbName(q.Get("name"), "Sound")
	emoji := sbEmoji(q.Get("emoji"), "🔊")
	byName := s.store.userName(uid)
	res, err := s.store.db.Exec(`INSERT INTO sounds(hash,name,emoji,ms,size,by_uid,by_name,created) VALUES(?,?,?,?,?,?,?,?)`, hash, name, emoji, ms, len(out), uid, byName, time.Now().UnixMilli())
	if err != nil {
		sbJSON(w, http.StatusInternalServerError, map[string]string{"error": "the sound couldn't be kept", "why": "server"})
		return
	}
	id, _ := res.LastInsertId()
	_ = sb.load()
	sb.changed()
	log.Printf("soundboard: sound %d (%d ms, %d bytes) from %s", id, ms, len(out), uid)
	sbJSON(w, http.StatusOK, map[string]any{"id": id, "hash": hash, "ms": ms, "cut": inSec > sbMaxSeconds+0.05})
}

func (sb *soundboard) mayUpload(uid string, now time.Time) bool {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	var keep []time.Time
	for _, t := range sb.uploads[uid] {
		if now.Sub(t) < 10*time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= sbUploadsPer {
		sb.uploads[uid] = keep
		return false
	}
	sb.uploads[uid] = append(keep, now)
	return true
}

// ====== ffmpeg ======

var errSbFFmpeg = errors.New("ffmpeg isn't installed on the server (apt install ffmpeg, or SV_FFMPEG)")

var sbDurationRe = regexp.MustCompile(`Duration: (\d+):(\d\d):(\d\d(?:\.\d+)?)`)

// ffmpegEncode: the upload as mono 48 kHz Opus in Ogg, ≤ 10 s; also how long the upload was (s; 0: unknown).
// ffmpeg reads only that one file, only as the format its bytes said, and writes only to its stdout.
func (sb *soundboard) ffmpegEncode(ctx context.Context, data []byte, format string) ([]byte, float64, error) {
	name := sb.s.cfg.ffmpeg
	if name == "" {
		name = "ffmpeg"
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return nil, 0, errSbFFmpeg
	}
	// From a private file (read whole, ffmpeg knows how long it is; from a pipe it often doesn't)
	tmp, err := os.CreateTemp(sb.dir, ".upload-*")
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, 0, err
	}
	if err := tmp.Close(); err != nil {
		return nil, 0, err
	}
	run := func(codec ...string) ([]byte, string, error) {
		args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "info", "-protocol_whitelist", "file", "-f", format, "-i", "file:" + tmp.Name(),
			"-t", strconv.Itoa(sbMaxSeconds), "-vn", "-sn", "-dn", "-map_metadata", "-1", "-map", "0:a:0",
			"-af", "loudnorm=I=-16:TP=-2:LRA=11", "-ac", "1", "-ar", "48000"}
		args = append(args, codec...)
		args = append(args, "-b:a", "64k", "-f", "ogg", "pipe:1")
		cmd := exec.CommandContext(ctx, bin, args...)
		var out, errb bytes.Buffer
		cmd.Stdout = &limitedBuffer{b: &out, max: sbOutMax + 1}
		cmd.Stderr = &limitedBuffer{b: &errb, max: 64 << 10}
		err := cmd.Run()
		return out.Bytes(), errb.String(), err
	}
	out, log1, err := run("-c:a", "libopus")
	if err != nil && strings.Contains(log1, "libopus") {
		out, log1, err = run("-c:a", "opus", "-strict", "-2")
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, errors.New("the sound took too long to convert")
		}
		return nil, 0, errors.New("the file couldn't be read as a sound")
	}
	var in float64
	if m := sbDurationRe.FindStringSubmatch(log1); m != nil {
		hh, _ := strconv.Atoi(m[1])
		mm, _ := strconv.Atoi(m[2])
		ss, _ := strconv.ParseFloat(m[3], 64)
		in = float64(hh*3600+mm*60) + ss
	}
	return out, in, nil
}

type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

// oggOpusMillis: an Ogg Opus file's length (the last page's granule position less the pre-skip, at 48 kHz); 0 when
// it isn't one.
func oggOpusMillis(b []byte) int {
	var pre uint16
	var last int64 = -1
	for pos := 0; pos+27 <= len(b); {
		if string(b[pos:pos+4]) != "OggS" {
			return 0
		}
		granule := int64(binary.LittleEndian.Uint64(b[pos+6 : pos+14]))
		segs := int(b[pos+26])
		if pos+27+segs > len(b) {
			return 0
		}
		size := 0
		for _, l := range b[pos+27 : pos+27+segs] {
			size += int(l)
		}
		body := pos + 27 + segs
		if body+size > len(b) {
			return 0
		}
		if size >= 19 && string(b[body:body+8]) == "OpusHead" {
			pre = binary.LittleEndian.Uint16(b[body+10 : body+12])
		}
		if granule >= 0 {
			last = granule
		}
		pos = body + size
	}
	if last <= int64(pre) {
		return 0
	}
	return int((last - int64(pre)) * 1000 / 48000)
}
