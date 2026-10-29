// Spectre Voice Server: voice in channels (WebRTC, Opus), the channel tree, channel and private chat with
// history, roles and moderation, streams, and what the apps share (games, Lei, profiles, donations) on one server.
//
// Every setting is a flag, with an SV_* environment variable as its default (systemd's EnvironmentFile).
//
//	spectrevoice-server                     run
//	spectrevoice-server role <uid> <role>   name an owner, admin or mod ("" to take it back), then exit
//	spectrevoice-server backup [dir]        a backup now (backup.go), into dir or SV_BACKUP_DIR
//	spectrevoice-server restore <file>      put a backup back (the server must be stopped)
//	spectrevoice-server version
package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

var version = "dev"

// mainSpace is the bus space of the server's one space (space.go).
const mainSpace = "main"

type config struct {
	listen      string
	data        string
	name        string
	motd        string
	joinPass    string
	maxUsers    int
	owners      string
	publicIP    string
	udpMin      uint
	udpMax      uint
	stun        string
	turn        string
	turnUser    string
	turnPass    string
	turnSecret  string // coturn's static-auth-secret: per-person credentials that end (publicICE)
	maxRooms    int
	maxSubs     int
	updates     bool
	ghApk       string
	files       bool
	fileMB      int64
	fileDays    int
	fileQuota   int64
	cfQuota     int64 // MB all channel files may take
	cfUserMB    int64 // MB one person's channel files may take on the server
	fileSpaceMB int64
	maxSpaces   int
	playCreds   string
	playPkg     string
	playRate    float64
	playTest    bool
	contact     string
	pushCreds   string // Firebase service account (JSON) for push notifications ("": none)
	inviteBase  string // what invite links start with (https://sfu.example.com/i/)
	licenseURL  string // the donation service (places for donations)
	partnerCode string // a hosting partner's plan code (SVP-…): its places, redeemed at start
	udpPort     int    // one UDP port for all the voice and streams (hosting panels), instead of the range
	vendorKey   string // its public key (tests use their own)
	audioLevel  int    // the voice's level where a channel doesn't set one (quality.go)
	music       bool   // the music bot (needs yt-dlp and ffmpeg)
	pushContent bool   // push notifications carry who and what (through Google); off: only that there's something
	ytdlp       string
	ffmpeg      string
	tv          bool   // the server's TV (tv.go): an IPTV channel everyone watches, played by ffmpeg
	tvLAN       bool   // the TV may open LAN addresses (else only public ones)
	tvKbps      int    // the TV's video bit rate
	tvHeight    int    // the TV's picture height at most (16:9 width)
	tvVideo     string // "auto" (VAAPI when it works, else x264), "vaapi", "x264"
	tvVaapi     string // the VAAPI device
	tvIdle      int    // seconds the TV plays with nobody watching
	noSounds    bool   // SV_SOUNDBOARD=false: no server soundboard (soundboard.go)
	soundsMax   int    // the server's sounds at most (SV_SOUNDS_MAX)
	tempEmpty   int    // seconds a temporary channel stays empty before it goes (tempch.go)
	tempPerUser int    // temporary channels one person has at once
	tempMax     int    // temporary channels on the server at once
	eventLead   int    // minutes before an event starts that people are told (events.go)
	backupDir   string // where the daily backups go (backup.go); "" : <data>/backups
	backupKeep  int    // daily backups kept
	backupHour  int    // the hour (server's local time) of the daily backup; -1: none
	leiMax      int    // the most Lei one grant may add or take (lei.go)
}

type server struct {
	udpMux  *ice.MultiUDPMuxDefault // with SV_UDP_PORT
	cfg     config
	apiMu   sync.RWMutex
	api     *webrtc.API // through webrtcAPI (rebuilt when the DDNS name moves)
	pubIP   string      // the address SV_PUBLIC_IP resolved to
	ice     []webrtc.ICEServer
	secret  []byte
	store   *store
	hub     *hub
	voice   *voice
	mods    *mods
	mu      sync.Mutex
	rooms   map[string]*room
	p2p     *p2pState // P2P streams: signalling only (p2p.go)
	link    *linkHub  // device linking: relay only (link.go)
	updates []*updater
	files   *fileStore
	bus     *bus
	push    *pusher
	lic     *licensing
	music   *musicBots
	chfiles *channelFiles
	metrics *metrics
	admin   *adminAuth
	logs    *logRing
	conf    serverConf  // name and password, as the admins set them (settings.go)
	tv      *tvManager  // the server's TV (tv.go)
	sb      *soundboard // the server's sounds (soundboard.go)
	// testSpaces lets tests open bus spaces besides mainSpace (nil in production)
	testSpaces func(name string) bool
}

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(env(name, "")); err == nil {
		return n
	}
	return def
}

func envBool(name string, def bool) bool {
	if b, err := strconv.ParseBool(env(name, "")); err == nil {
		return b
	}
	return def
}

func main() {
	var cfg config
	flag.StringVar(&cfg.listen, "listen", env("SV_LISTEN", ":7880"), "HTTP address (behind Caddy for HTTPS) [SV_LISTEN]")
	flag.StringVar(&cfg.data, "data", env("SV_DATA", "/var/lib/spectrevoice"), "folder for everything the server keeps [SV_DATA]")
	flag.StringVar(&cfg.name, "name", env("SV_NAME", "Spectre Voice"), "the server's name in the apps, until an admin renames it there [SV_NAME]")
	flag.StringVar(&cfg.motd, "motd", env("SV_MOTD", ""), "message shown on joining [SV_MOTD]")
	flag.StringVar(&cfg.joinPass, "password", env("SV_PASSWORD", ""), "server password ('' for an open server), until an admin changes it in the apps [SV_PASSWORD]")
	flag.IntVar(&cfg.maxUsers, "max-users", envInt("SV_MAX_USERS", 0), "your own cap on people connected at once (0: only the places: 30, plus donations) [SV_MAX_USERS]")
	flag.StringVar(&cfg.owners, "owners", env("SV_OWNERS", ""), "uids made owners at start, comma separated [SV_OWNERS]")
	flag.StringVar(&cfg.publicIP, "public-ip", env("SV_PUBLIC_IP", ""), "address announced for voice: the LAN IP, or the public IP / DDNS name when ports are forwarded (the LAN keeps working) [SV_PUBLIC_IP]")
	udpMin := flag.Int("udp-min", envInt("SV_UDP_MIN", 50000), "lowest UDP port for voice and streams [SV_UDP_MIN]")
	udpMax := flag.Int("udp-max", envInt("SV_UDP_MAX", 50200), "highest UDP port [SV_UDP_MAX]")
	flag.IntVar(&cfg.udpPort, "udp-port", envInt("SV_UDP_PORT", 0), "one UDP port for all voice and streams, instead of the range (hosting panels: the same number as the HTTP port) [SV_UDP_PORT]")
	flag.StringVar(&cfg.partnerCode, "partner-code", env("SV_PARTNER_CODE", ""), "a hosting partner's plan code (SVP-…), set by the host [SV_PARTNER_CODE]")
	flag.StringVar(&cfg.stun, "stun", env("SV_STUN", ""), "STUN server for the server itself, e.g. stun:stun.l.google.com:19302 [SV_STUN]")
	flag.StringVar(&cfg.turn, "turn", env("SV_TURN", ""), "TURN server handed to the apps, e.g. turn:sfu.example.com:3478 [SV_TURN]")
	flag.StringVar(&cfg.turnUser, "turn-user", env("SV_TURN_USER", ""), "TURN user [SV_TURN_USER]")
	flag.StringVar(&cfg.turnPass, "turn-pass", env("SV_TURN_PASS", ""), "TURN password [SV_TURN_PASS]")
	flag.StringVar(&cfg.turnSecret, "turn-secret", env("SV_TURN_SECRET", ""), "TURN shared secret (coturn use-auth-secret): credentials per person that end, instead of the user and password [SV_TURN_SECRET]")
	flag.IntVar(&cfg.maxRooms, "max-streams", envInt("SV_MAX_STREAMS", 16), "most streams at once [SV_MAX_STREAMS]")
	flag.IntVar(&cfg.maxSubs, "max-viewers", envInt("SV_MAX_VIEWERS", 50), "most viewers per stream [SV_MAX_VIEWERS]")
	flag.BoolVar(&cfg.updates, "updates", envBool("SV_UPDATES", false), "serve the apps' updates (Android direct APK, Windows MSI) from GitHub releases [SV_UPDATES]")
	flag.StringVar(&cfg.ghApk, "updates-repo", env("SV_UPDATES_REPO", "Stefan3Dx/SpectreVoice-Releases"), "public GitHub repo of the apps' releases (a private one needs SV_GITHUB_TOKEN) [SV_UPDATES_REPO]")
	flag.BoolVar(&cfg.files, "files", envBool("SV_FILES", true), "files shared in the chat [SV_FILES]")
	flag.Int64Var(&cfg.fileMB, "file-max-mb", int64(envInt("SV_FILE_MAX_MB", 100)), "largest shared file, MB [SV_FILE_MAX_MB]")
	flag.IntVar(&cfg.fileDays, "file-days", envInt("SV_FILE_DAYS", 30), "days a shared file is kept [SV_FILE_DAYS]")
	flag.Int64Var(&cfg.cfQuota, "cf-quota-mb", int64(envInt("SV_CF_QUOTA_MB", 10240)), "MB all channel files may take [SV_CF_QUOTA_MB]")
	flag.Int64Var(&cfg.cfUserMB, "cf-user-mb", int64(envInt("SV_CF_USER_MB", 2048)), "MB one person's channel files may take [SV_CF_USER_MB]")
	flag.Int64Var(&cfg.fileQuota, "file-quota-mb", int64(envInt("SV_FILE_QUOTA_MB", 5120)), "MB all shared files may take [SV_FILE_QUOTA_MB]")
	flag.Int64Var(&cfg.fileSpaceMB, "file-space-mb", int64(envInt("SV_FILE_SPACE_MB", 1024)), "MB the chat files of one space may take (0: no limit but the total) [SV_FILE_SPACE_MB]")
	flag.IntVar(&cfg.maxSpaces, "max-spaces", envInt("SV_MAX_SPACES", 16), "bus spaces kept open at once [SV_MAX_SPACES]")
	flag.StringVar(&cfg.playCreds, "play-credentials", env("SV_PLAY_CREDENTIALS", ""), "Google Play service account key file (JSON) for donations [SV_PLAY_CREDENTIALS]")
	flag.StringVar(&cfg.playPkg, "play-package", env("SV_PLAY_PACKAGE", "io.github.stefan3dx.spectrevoice"), "the Play app's package [SV_PLAY_PACKAGE]")
	flag.Float64Var(&cfg.playRate, "play-eur-ron", 5.0, "RON one euro donated through Google Play counts as")
	flag.BoolVar(&cfg.playTest, "play-allow-test", envBool("SV_PLAY_ALLOW_TEST", false), "count license testers' purchases [SV_PLAY_ALLOW_TEST]")
	flag.StringVar(&cfg.contact, "contact-email", env("SV_CONTACT_EMAIL", ""), "the owner's contact for data requests, shown at /privacy [SV_CONTACT_EMAIL]")
	flag.StringVar(&cfg.pushCreds, "push-credentials", env("SV_PUSH_CREDENTIALS", ""), "Firebase service account (JSON) for push notifications [SV_PUSH_CREDENTIALS]")
	flag.StringVar(&cfg.licenseURL, "license-url", env("SV_LICENSE_URL", "https://license.spectrevoice.com"), "the donation service: donations add places for a month [SV_LICENSE_URL]")
	flag.IntVar(&cfg.audioLevel, "audio-quality", envInt("SV_AUDIO_QUALITY", defaultAudioLevel), "voice quality where a channel sets none: 1 low … 6 max [SV_AUDIO_QUALITY]")
	flag.BoolVar(&cfg.pushContent, "push-content", envBool("SV_PUSH_CONTENT", false), "push notifications carry the sender and the text (they pass through Google) [SV_PUSH_CONTENT]")
	flag.BoolVar(&cfg.music, "music", envBool("SV_MUSIC", true), "the music bot (!play), when yt-dlp and ffmpeg are installed [SV_MUSIC]")
	flag.StringVar(&cfg.ytdlp, "yt-dlp", env("SV_YTDLP", "yt-dlp"), "yt-dlp program [SV_YTDLP]")
	flag.StringVar(&cfg.ffmpeg, "ffmpeg", env("SV_FFMPEG", "ffmpeg"), "ffmpeg program [SV_FFMPEG]")
	flag.BoolVar(&cfg.tv, "tv", envBool("SV_TV", true), "the server's TV: an IPTV channel (M3U playlist) everyone watches like a stream, played by ffmpeg [SV_TV]")
	flag.BoolVar(&cfg.tvLAN, "tv-lan", envBool("SV_TV_LAN", false), "the TV may open LAN addresses (10.x, 192.168.x, …) for its playlist and channels; off: public ones only [SV_TV_LAN]")
	flag.IntVar(&cfg.tvKbps, "tv-kbps", envInt("SV_TV_KBPS", 3000), "the TV's video bit rate, kbit/s (12000 at most) [SV_TV_KBPS]")
	flag.IntVar(&cfg.tvHeight, "tv-height", envInt("SV_TV_HEIGHT", 720), "the TV's picture height at most (240-1080, 16:9) [SV_TV_HEIGHT]")
	flag.StringVar(&cfg.tvVideo, "tv-video", env("SV_TV_VIDEO", "auto"), "how the TV's picture is encoded: auto (the graphics card through VAAPI when it works, else x264), vaapi, x264 [SV_TV_VIDEO]")
	flag.StringVar(&cfg.tvVaapi, "tv-vaapi", env("SV_TV_VAAPI", "/dev/dri/renderD128"), "the graphics card for SV_TV_VIDEO vaapi/auto [SV_TV_VAAPI]")
	flag.IntVar(&cfg.tvIdle, "tv-idle", envInt("SV_TV_IDLE", 90), "seconds the TV keeps playing with nobody watching [SV_TV_IDLE]")
	sounds := flag.Bool("soundboard", envBool("SV_SOUNDBOARD", true), "the server's soundboard: short sounds uploaded by admins, played into voice channels (needs ffmpeg to upload) [SV_SOUNDBOARD]")
	flag.IntVar(&cfg.soundsMax, "sounds-max", envInt("SV_SOUNDS_MAX", sbDefaultMax), "the server's sounds at most [SV_SOUNDS_MAX]")
	flag.IntVar(&cfg.tempEmpty, "temp-empty", envInt("SV_TEMP_EMPTY", 60), "seconds a temporary channel stays empty before it is deleted [SV_TEMP_EMPTY]")
	flag.IntVar(&cfg.tempPerUser, "temp-per-user", envInt("SV_TEMP_PER_USER", 1), "temporary channels one person may have at once [SV_TEMP_PER_USER]")
	flag.IntVar(&cfg.tempMax, "temp-max", envInt("SV_TEMP_MAX", 20), "temporary channels on the server at once [SV_TEMP_MAX]")
	flag.IntVar(&cfg.eventLead, "event-lead", envInt("SV_EVENT_LEAD", 15), "minutes before a scheduled event that people are told (0: only at the start) [SV_EVENT_LEAD]")
	flag.StringVar(&cfg.backupDir, "backup-dir", env("SV_BACKUP_DIR", ""), "folder for the daily backups (default: <data>/backups) [SV_BACKUP_DIR]")
	flag.IntVar(&cfg.backupKeep, "backup-keep", envInt("SV_BACKUP_KEEP", 14), "daily backups kept [SV_BACKUP_KEEP]")
	flag.IntVar(&cfg.backupHour, "backup-hour", envInt("SV_BACKUP_HOUR", 3), "hour of the day (0-23, server time) of the automatic backup; -1: none [SV_BACKUP_HOUR]")
	flag.IntVar(&cfg.leiMax, "lei-max", envInt("SV_LEI_MAX", 100000), "the most Lei one grant by the owner may add or take [SV_LEI_MAX]")
	tvLoop := flag.Bool("tv-test-loopback", false, "tests only: the TV may open 127.0.0.1 (never on a real server: the server's own ports are there)")
	flag.StringVar(&cfg.inviteBase, "invite-base", env("SV_INVITE_BASE", ""), "invite links' start, e.g. https://sfu.example.com/i/ (default: this server's address) [SV_INVITE_BASE]")
	proxies := flag.String("trusted-proxies", env("SV_TRUSTED_PROXIES", ""), "reverse proxies besides this machine's own whose X-Forwarded-For counts (a proxy elsewhere on the LAN), addresses or CIDRs, comma separated [SV_TRUSTED_PROXIES]")
	flag.Parse()
	trustedProxies = parseTrustedProxies(*proxies)
	cfg.udpMin, cfg.udpMax = uint(*udpMin), uint(*udpMax)
	tvLoopbackOK = *tvLoop
	cfg.noSounds = !*sounds

	switch flag.Arg(0) {
	case "version":
		fmt.Println(version)
		return
	case "role":
		if flag.NArg() != 3 || !validRole(flag.Arg(2)) {
			log.Fatal("usage: spectrevoice-server role <uid> <owner|admin|mod|''>")
		}
		st, err := openStore(filepath.Join(cfg.data, "spectre.db"))
		if err != nil {
			log.Fatal(err)
		}
		if err := st.setRole(flag.Arg(1), role(flag.Arg(2))); err != nil {
			log.Fatal(err)
		}
		st.close()
		fmt.Printf("%s: %q (applies when they next connect)\n", flag.Arg(1), flag.Arg(2))
		return
	case "backup":
		if flag.NArg() > 2 {
			log.Fatal("usage: spectrevoice-server backup [dir]")
		}
		if cfg.backupDir == "" {
			cfg.backupDir = filepath.Join(cfg.data, "backups")
		}
		if err := backupCLI(cfg, flag.Arg(1)); err != nil {
			log.Fatalf("backup: %v", err)
		}
		return
	case "restore":
		if flag.NArg() != 2 {
			log.Fatal("usage: spectrevoice-server restore <file> (with the server stopped)")
		}
		aside, err := restoreBackup(cfg.data, cfg.backupDir, flag.Arg(1))
		if err != nil {
			log.Fatalf("restore: %v", err)
		}
		fmt.Printf("restored %s into %s\nwhat was there before: %s (delete it once all is well)\n", flag.Arg(1), cfg.data, aside)
		return
	case "":
	default:
		log.Fatalf("unknown command %q", flag.Arg(0))
	}
	unlock, err := lockData(cfg.data)
	if err != nil {
		log.Fatal(err)
	}
	defer unlock()

	runPlusIfPresent(cfg)
	s, err := newServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	udp := fmt.Sprintf("UDP %d-%d", cfg.udpMin, cfg.udpMax)
	if cfg.udpPort > 0 {
		udp = fmt.Sprintf("UDP %d", cfg.udpPort)
	}
	log.Printf("Spectre Voice Server %s on %s (%s%s), data in %s", version, cfg.listen, udp,
		map[bool]string{true: ", announced as " + cfg.publicIP, false: ""}[cfg.publicIP != ""], cfg.data)
	go s.watchdog(cfg.listen)
	go s.eventLoop()
	go s.backupLoop()
	// Idle keep-alive connections close; uploads set their own longer deadline (files.go, chfiles.go)
	srv := &http.Server{Addr: cfg.listen, Handler: s.handler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func newServer(cfg config) (*server, error) {
	if err := os.MkdirAll(cfg.data, 0o750); err != nil {
		return nil, err
	}
	if cfg.tempEmpty <= 0 {
		cfg.tempEmpty = 60
	}
	if cfg.tempPerUser <= 0 {
		cfg.tempPerUser = 1
	}
	if cfg.tempMax <= 0 {
		cfg.tempMax = 20
	}
	if cfg.leiMax <= 0 {
		cfg.leiMax = 100000
	}
	if cfg.backupKeep <= 0 {
		cfg.backupKeep = 14
	}
	if cfg.backupDir == "" {
		cfg.backupDir = filepath.Join(cfg.data, "backups")
	}
	s := &server{cfg: cfg, rooms: map[string]*room{}, p2p: newP2P(), admin: newAdminAuth(), logs: newLogRing(500)}
	log.SetOutput(io.MultiWriter(os.Stderr, s.logs))
	var err error
	if s.secret, err = loadSecret(cfg.data); err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}
	if s.store, err = openStore(filepath.Join(cfg.data, "spectre.db")); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := s.store.migrateCommunities(cfg.name); err != nil {
		return nil, fmt.Errorf("communities: %w", err)
	}
	if err := s.loadSettings(); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	if err := s.store.migrateInvites(); err != nil {
		return nil, fmt.Errorf("invites: %w", err)
	}
	if err := s.store.migrateChannels(); err != nil {
		return nil, fmt.Errorf("channels: %w", err)
	}
	if err := s.store.migrateBots(); err != nil {
		return nil, fmt.Errorf("bots: %w", err)
	}
	s.store.dropTempLeftovers()
	s.store.migrateNewPerms()
	if err := s.store.migrateEvents(); err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	if err := s.store.migrateLei(); err != nil {
		return nil, fmt.Errorf("lei: %w", err)
	}
	// A server older than text channels gets one "general" per community, once
	if !s.store.hasColumnValue("channels", "kind", "text") {
		_, _ = s.store.db.Exec(`INSERT INTO channels(parent,name,pos,is_default,comm,kind) SELECT 0,'general',-10,0,id,'text' FROM communities`)
	}
	if cfg.vendorKey == "" {
		s.cfg.vendorKey = vendorKey
	}
	if s.lic, err = newLicensing(s); err != nil {
		return nil, err
	}
	for _, o := range strings.Split(cfg.owners, ",") {
		if o = strings.TrimSpace(o); o != "" {
			if err := s.store.setRole(o, roleOwner); err != nil {
				return nil, err
			}
			log.Printf("owner: %s", o)
		}
	}
	if err := s.setup(); err != nil {
		return nil, err
	}
	go s.watchPublicIP()
	s.mods = newMods(s)
	s.link = newLinkHub()
	// The hub before the voice: the voice's talk watcher reads s.hub from its own goroutine
	if s.hub, err = newHub(s); err != nil {
		return nil, err
	}
	s.voice = newVoice(s)
	s.tv = newTV(s)
	if s.sb, err = newSoundboard(s); err != nil {
		return nil, err
	}
	go s.lic.slotsWatch()
	s.metrics = newMetrics(s)
	if code := s.claimCode(); code != "" {
		log.Printf("nobody owns this server yet: in the app, Server › Claim, type %s", code)
	}
	s.music = newMusicBots(s)
	if s.chfiles, err = newChannelFiles(s, filepath.Join(cfg.data, "channel-files")); err != nil {
		return nil, err
	}
	if cfg.pushCreds != "" {
		if s.push, err = newPusher(cfg.pushCreds, s.store); err != nil {
			return nil, fmt.Errorf("push: %w", err)
		}
		log.Printf("push: notifications through Firebase project %s", s.push.project)
	}
	s.bus = newBus(filepath.Join(cfg.data, "bus"))
	s.bus.maxSpaces = cfg.maxSpaces
	s.bus.store = s.store
	s.bus.chfiles = s.chfiles
	s.bus.sb = s.sb
	if cfg.playCreds != "" {
		pv, err := newPlayVerifier(cfg.playCreds, cfg.playPkg, s.bus.dir, cfg.playRate, cfg.playTest)
		if err != nil {
			return nil, fmt.Errorf("play: %w", err)
		}
		s.bus.playV = pv
		log.Printf("play: donations to %s checked as %s", cfg.playPkg, pv.email)
	}
	if cfg.updates {
		dir := filepath.Join(cfg.data, "updates")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		token := strings.TrimSpace(os.Getenv("SV_GITHUB_TOKEN"))
		client := &http.Client{Timeout: 15 * time.Minute}
		s.updates = append(s.updates,
			&updater{dir: dir, index: "android.json", asset: ".apk", prefix: "android-v", repo: cfg.ghApk, token: token, api: "https://api.github.com", every: 10 * time.Minute, http: client},
			&updater{dir: dir, index: "windows.json", asset: ".msi", prefix: "windows-v", repo: cfg.ghApk, token: token, api: "https://api.github.com", every: 10 * time.Minute, http: client})
		for _, u := range s.updates {
			go u.loop()
		}
	}
	if cfg.files {
		dir := filepath.Join(cfg.data, "files")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		s.files = &fileStore{dir: dir, maxSize: cfg.fileMB << 20, keep: time.Duration(cfg.fileDays) * 24 * time.Hour, quota: cfg.fileQuota << 20, spaceQuota: cfg.fileSpaceMB << 20}
		s.files.recount()
		go s.files.sweep()
	}
	return s, nil
}

func (s *server) knownSpace(name string) bool {
	if s.testSpaces != nil && s.testSpaces(name) {
		return true
	}
	return commOfSpace(name) == spaceID
}

// mayUseSpace: uid is on this server and not banned (tests' extra spaces: anyone).
func (s *server) mayUseSpace(name, uid string) bool {
	if s.testSpaces != nil && s.testSpaces(name) {
		return true
	}
	c := commOfSpace(name)
	return c != 0 && (s.store.isMember(c, uid) || s.store.role(uid).atLeast(roleAdmin)) && (s.mods == nil || !s.mods.banned(uid, c))
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ws", s.hub.handle)
	mux.HandleFunc("/v1/link", s.handleLink)
	mux.HandleFunc("/streams", s.handleStreams)
	mux.HandleFunc("/streams/status", s.handleStreamStatus)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/update/", s.handleUpdate)
	mux.HandleFunc("/files", s.handleUpload)
	mux.HandleFunc("/f/", s.handleFile)
	mux.HandleFunc("/bus", s.handleBus)
	mux.HandleFunc("/backup/", s.handleBackup)
	mux.HandleFunc("/crash", s.handleCrash)
	mux.HandleFunc("/crash/", s.handleCrash)
	mux.HandleFunc("/privacy", s.handlePrivacy)
	mux.HandleFunc("/delete", s.handleDelete)
	mux.HandleFunc("/i/", s.handleInvitePage)
	mux.HandleFunc("/app", s.handleWebApp)
	mux.HandleFunc("/app/", s.handleWebApp)
	mux.HandleFunc("/download/", s.handleDownload)
	mux.HandleFunc("/api/invite/", s.handleInviteInfo)
	mux.HandleFunc("/admin", s.handleAdmin)
	mux.HandleFunc("/admin/", s.handleAdmin)
	mux.HandleFunc("/cf/upload", s.chfiles.handleUpload)
	mux.HandleFunc("/cf/", s.chfiles.handleDownload)
	mux.HandleFunc("/tv/channels", s.handleTVChannels)
	mux.HandleFunc("/tv/logo", s.handleTVLogo)
	mux.HandleFunc("/tv/playlist", s.handleTVPlaylist)
	mux.HandleFunc("/sb/upload", s.handleSoundUpload)
	mux.HandleFunc("/sb/f/", s.handleSoundFile)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "%s: Spectre Voice Server %s\n", s.serverName(), version)
	})
	return mux
}

func (s *server) setup() error {
	if s.cfg.udpPort > 0 {
		// Everything on one UDP port (a hosting panel gives one port per server)
		mux, err := ice.NewMultiUDPMuxFromPort(s.cfg.udpPort)
		if err != nil {
			return fmt.Errorf("udp port %d: %w", s.cfg.udpPort, err)
		}
		s.udpMux = mux
	}
	ip, private := "", false
	if s.cfg.publicIP != "" {
		var err error
		if ip, private, err = resolvePublic(s.cfg.publicIP); err != nil {
			return fmt.Errorf("public ip %q: %w", s.cfg.publicIP, err)
		}
	}
	api, err := s.buildAPI(ip, private)
	if err != nil {
		return err
	}
	s.apiMu.Lock()
	s.api, s.pubIP = api, ip
	s.apiMu.Unlock()
	if s.cfg.stun != "" {
		s.ice = append(s.ice, webrtc.ICEServer{URLs: []string{s.cfg.stun}})
	}
	return nil
}

// webrtcAPI: what new voice and stream connections are made with (rebuilt when a DDNS name moves).
func (s *server) webrtcAPI() *webrtc.API {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	return s.api
}

// publicIPEvery: how often a DDNS name in SV_PUBLIC_IP is looked up again (a var for the tests).
var publicIPEvery = 5 * time.Minute

// watchPublicIP follows a DDNS name: when it points somewhere new (a home connection's address changed), new
// connections announce the new address. Connections made before keep theirs; they break with the old address and
// the apps make new ones.
func (s *server) watchPublicIP() {
	if s.cfg.publicIP == "" || net.ParseIP(s.cfg.publicIP) != nil {
		return // an address doesn't move
	}
	for range time.Tick(publicIPEvery) {
		s.refreshPublicIP()
	}
}

// refreshPublicIP looks the DDNS name up once and rebuilds the API if it moved; true when it did.
func (s *server) refreshPublicIP() bool {
	ip, private, err := resolvePublic(s.cfg.publicIP)
	if err != nil {
		return false // a lookup that failed now (DNS down) keeps the address we have
	}
	s.apiMu.RLock()
	same := ip == s.pubIP
	s.apiMu.RUnlock()
	if same {
		return false
	}
	api, err := s.buildAPI(ip, private)
	if err != nil {
		log.Printf("voice: %s moved to %s, but: %v", s.cfg.publicIP, ip, err)
		return false
	}
	s.apiMu.Lock()
	old := s.pubIP
	s.api, s.pubIP = api, ip
	s.apiMu.Unlock()
	log.Printf("voice: %s moved from %s to %s: new connections announce %s", s.cfg.publicIP, old, ip, ip)
	return true
}

// buildAPI makes the WebRTC API (codecs, interceptors, ports) announcing ip ("": the machine's own addresses).
func (s *server) buildAPI(ip string, private bool) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := registerCodecs(m); err != nil {
		return nil, err
	}
	// The voice's level in every packet (who's talking)
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.AudioLevelURI}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	// A key frame now and then, so a stream viewer that missed one doesn't stay grey
	pli, err := intervalpli.NewReceiverInterceptor(intervalpli.GeneratorInterval(3 * time.Second))
	if err != nil {
		return nil, err
	}
	ir.Add(pli)
	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// Candidates an app may give us: no link-local, multicast or unspecified addresses (no STUN sent to odd
	// places; loopback stays, for an app on this same machine)
	se.SetRemoteIPFilter(func(ip net.IP) bool {
		return !(ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified())
	})
	if s.udpMux != nil {
		se.SetICEUDPMux(s.udpMux)
	} else if err := se.SetEphemeralUDPPortRange(uint16(s.cfg.udpMin), uint16(s.cfg.udpMax)); err != nil {
		return nil, err
	}
	if ip != "" {
		if private {
			// At home: the LAN address replaces the container's own
			se.SetNAT1To1IPs([]string{ip}, webrtc.ICECandidateTypeHost)
		} else if s.udpMux != nil {
			// One UDP port behind a router: the public address is added next to the LAN ones, on that same port.
			// (A mapped srflx candidate would be gathered on a random port outside the mux, which no router
			// forwards: the outside could never connect.)
			if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
				External:        []string{ip},
				AsCandidateType: webrtc.ICECandidateTypeHost,
				Mode:            webrtc.ICEAddressRewriteAppend,
			}); err != nil {
				return nil, err
			}
		} else {
			// Behind a router with a forwarded port range: the LAN keeps its own addresses, the outside gets the public one
			se.SetNAT1To1IPs([]string{ip}, webrtc.ICECandidateTypeSrflx)
		}
		log.Printf("voice announced as %s (%s)", ip, map[bool]string{true: "LAN", false: "public, plus the LAN addresses"}[private])
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

// turnCredLife: how long the TURN credentials a welcome gives work (with SV_TURN_SECRET); a reconnect gets new ones.
const turnCredLife = 48 * time.Hour

// publicICE is what the apps get to reach the server's voice through hard networks (a TURN relay, when set). With
// SV_TURN_SECRET (coturn's use-auth-secret) each person gets credentials of their own that end (the TURN REST
// scheme: "<expiry>:<uid>", HMAC-SHA1 of it with the shared secret), so a key once seen in a welcome isn't a
// relay for ever; else the fixed SV_TURN_USER / SV_TURN_PASS.
func (s *server) publicICE(uid string) []map[string]any {
	var out []map[string]any
	if s.cfg.turn == "" {
		return out
	}
	user, pass := s.cfg.turnUser, s.cfg.turnPass
	if s.cfg.turnSecret != "" {
		user = strconv.FormatInt(time.Now().Add(turnCredLife).Unix(), 10) + ":" + uid
		m := hmac.New(sha1.New, []byte(s.cfg.turnSecret))
		m.Write([]byte(user))
		pass = base64.StdEncoding.EncodeToString(m.Sum(nil))
	}
	return append(out, map[string]any{"urls": []string{s.cfg.turn}, "username": user, "credential": pass})
}

// lookupIP: DNS (tests give their own answers).
var lookupIP = net.LookupIP

// resolvePublic takes an IP or a name (a DDNS address) and says whether it's a private (LAN) address.
func resolvePublic(v string) (string, bool, error) {
	ip := net.ParseIP(v)
	if ip == nil {
		addrs, err := lookupIP(v)
		if err != nil {
			return "", false, err
		}
		for _, a := range addrs {
			if a.To4() != nil {
				ip = a
				break
			}
		}
		if ip == nil && len(addrs) > 0 {
			ip = addrs[0]
		}
		if ip == nil {
			return "", false, fmt.Errorf("no address for %s", v)
		}
	}
	return ip.String(), ip.IsPrivate() || ip.IsLoopback(), nil
}

// opusFmtp: stereo allowed both ways (the music levels and the bot), FEC always.
const opusFmtp = "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1"

// registerCodecs: Opus for the voice; H.264, VP8, VP9 and AV1 for the streams (the apps pick).
func registerCodecs(m *webrtc.MediaEngine) error {
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: opusFmtp},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}
	fb := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "transport-cc"}}
	video := []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", RTCPFeedback: fb}, PayloadType: 102},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", RTCPFeedback: fb}, PayloadType: 106},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f", RTCPFeedback: fb}, PayloadType: 108},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640032", RTCPFeedback: fb}, PayloadType: 112},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: fb}, PayloadType: 96},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0", RTCPFeedback: fb}, PayloadType: 98},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1, ClockRate: 90000, RTCPFeedback: fb}, PayloadType: 41},
	}
	for _, c := range video {
		if err := m.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}
	return nil
}
