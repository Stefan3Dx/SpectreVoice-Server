# Spectre Voice Server

Your own server for [Spectre Voice](https://spectrevoice.com): voice channels, chat with history, screen streams,
roles, invite links, a music bot, channel files, games and push notifications. One program, no database to set up,
runs on a small LXC or VPS.

One server is one space, as on TeamSpeak: people join with the server's **address**, a **display name** and, if you
set one, the **server password**. Their identity is a key made on their device (no account). Everything on the
server is shared by everyone on it: channels, roles and permissions, the server chat, bots, invites, the music bot,
moderation and the places.

## Install

On Debian 12/13 or Ubuntu 22.04+ (amd64 or arm64), as root:

```sh
curl -fsSL https://raw.githubusercontent.com/Stefan3Dx/SpectreVoice-Server/main/install.sh | sudo bash
```

It asks the server's name, a server password (empty: anyone with the address gets in) and, for a public server, the
domain: then it sets up HTTPS (Caddy), a TURN relay (coturn) and the music bot (ffmpeg, yt-dlp). At the end it prints
the address for the apps, the password and a **claim code**: in the app, *Server › Claim*, type it and the server
is yours.

Name and password can be changed later in the app (*Server settings*, by the server's admins): that wins over
`SV_NAME` / `SV_PASSWORD` and is kept in the server's database (the password as a bcrypt hash). The server's owners
and admins never need the password. An invite link can carry the right to skip it (made by admins: *Invite › Let
them in without the password*); who came in that way comes back without it, until banned.

Without questions: `sudo bash install.sh --yes` (LAN) or
`sudo bash install.sh --domain voice.example.com --email you@example.com --yes` (public); `--password PASS` sets the
server password.
Update: `sudo bash install.sh --update`. Restore a backup: `sudo bash install.sh --restore <file>` (see Backups).

### Ports

| | |
|---|---|
| TCP 443 (and 80 for the certificate) | the apps (HTTPS); on a LAN only TCP 7880 |
| UDP 50000–50200 | voice and streams ("P2P only" streams go app to app: the server passes on only their signalling) |
| TCP/UDP 3478, UDP 49152–49352 | TURN (only with a domain) |

At home behind a router: forward these to the server and set `SV_PUBLIC_IP` to your public IP or DDNS name.

## Web client

The server also serves a small client at **`/app/`** (`https://voice.example.com/app/`): voice and chat in the browser,
nothing to install. An invite page (`/i/<code>`) offers *Open in the browser*, which lands on `/app/?invite=<code>`
and uses the invite in its sign-in (its channel, and the password skipped when the invite says so); `?ch=<id>`
joins a channel. A server with a password asks for it. The identity is a P-256 key made in the browser
(WebCrypto, kept in IndexedDB, never leaves it) and signs the same v2 challenge as the apps, for the page's own host;
the nickname is remembered. Channels with who's where and who talks, join (locked channels ask the password), listen
and talk through WebRTC (push-to-talk with Space or the big button, open mic, mute, deafen, a volume per person),
channel and server chat with history, RO/EN. Needs HTTPS (or `localhost`) for the microphone and WebCrypto.
Not in the web client, by design: streams, games, files, private messages, moderation — the apps have them.

Files: `web/index.html`, `web/app.js`, `web/app.css`, embedded in the binary (`webapp.go`); no frameworks, no build
step, no external scripts or fonts, a strict `Content-Security-Policy` (everything from this server only).

Tests: `go test -run 'TestWebApp|TestInvitePage' .` checks the page and its headers. The end-to-end test drives the
page in headless Chromium through Playwright and is off unless asked for:

```sh
npm i -g playwright && npx playwright install chromium     # once
SV_WEB_E2E=1 NODE_PATH=$(npm root -g) go test -run TestWebE2E -v .
```

It connects as "Web", checks the welcome (channel list), the voice connection (offer/answer, `connected`), a message
from a Go client reaching the page and one from the page reaching it, and the talking indicator from the fake
microphone (`web/e2e/run.js`).

## TV (IPTV)

The server can play one IPTV channel for everyone, watched in the apps like a screen share, with sound. Someone with
the **TV** permission (admins have it) gives the playlist: an M3U address (`http://…/get.php?…&type=m3u_plus`) or an
uploaded `.m3u` file, and decides whether everyone may change the channel. Everyone sees the channel list (names,
groups and logos, with search and a group filter) and who put the channel on, and when.

The server plays the channel itself with **ffmpeg** (installed with the music bot; without it the apps say so):
H.264 at most `SV_TV_HEIGHT` (720) lines, 30 frames a second and `SV_TV_KBPS` (3000) kbit/s, Opus sound, published as
the stream room `tv:main`. It starts when someone puts a channel on or comes to watch and stops when nobody has
watched for `SV_TV_IDLE` (90) seconds; the channel chosen stays. ffmpeg has read timeouts, reconnects, a watchdog
(nothing for 25 s: restarted, 8 failures in a row: it gives up and says why), its own process group and dies with
the server. `SV_TV_VIDEO=auto` uses the graphics card (VAAPI, `SV_TV_VAAPI`) when a test encode works, else x264.
The TV counts as one of the server's streams (`SV_MAX_STREAMS`) and its viewers as a stream's (`SV_MAX_VIEWERS`).

The playlist's address and the channels' addresses hold the subscription's user and password: they never leave the
server, are never logged, and the apps only ever get names, groups and logos (the logos fetched by the server). An
uploaded file is kept as `data/tv/playlist.m3u`, readable by the server only. Playlists up to 10 MB and 20,000
channels; channel addresses must be http or https.

Every address the TV opens (the playlist, the logos, the channel and anything it leads to: redirects, HLS
segments) must be **public**: ffmpeg goes through a small proxy of the server on 127.0.0.1 that refuses loopback,
the LAN, link-local and other reserved ranges, checked on the address actually connected to. To play sources on
your own LAN (a tuner, a home IPTV box), set `SV_TV_LAN=true`. `SV_TV=false` turns the TV off.

Those watching the TV can send reactions (emoji that fly over the picture) and short lines shown over it, to
everyone watching; the apps show who's watching. Limits: 5 reactions a second a person (30 in all), a line every
1.5 s, 200 characters; chat mutes apply.

## Soundboard

Short sounds for everyone on the server. Someone with **soundboard_manage** (admins have it) uploads an OGG/Opus,
MP3 or WAV file with a name and an emoji; the server turns it into mono Opus with **ffmpeg** (loudness evened out,
at most 10 s: longer ones are cut) and keeps it as `data/sounds/<sha256>.opus`. Anyone with **soundboard**
(@everyone has it) plays one into their voice channel: the server tells everyone in the channel, and each app plays
its own copy (fetched once, kept by its hash) unless its person muted soundboard sounds. One sound a person every
3 s, 12 a minute, one a channel every 0.7 s; uploads 8 MB at most, 10 a person in 10 minutes. `SV_SOUNDS_MAX` (100)
sounds at most; `SV_SOUNDBOARD=false` turns it off.

## Privacy

Everything stays on this server: accounts are keys made on the devices (no e-mail, no phone number), chat history,
files and settings are here and nowhere else. What leaves it, and only when you turn it on or use it:

- **Push notifications** (`SV_PUSH_CREDENTIALS`): through Google; by default only "there is a message" and which
  server, never who or what (`SV_PUSH_CONTENT=false`). On the phone they're off until the person turns them on.
- **Music bot**: `!play some words` searches YouTube from this server (the words go there); `!play <link>` plays
  only that link (public addresses only).
- **TV**: the server downloads the playlist and plays the channel from the IPTV provider (the provider sees this
  server's address, never the viewers').
- **Donations for places**: when you redeem one, this server's key fingerprint goes to the donation service.
  Nothing about the people on it.
- **Crash reports** from the apps go to the developer only if the person says yes, with addresses and ids taken out.

## Settings

`/etc/spectrevoice/server.env` (see [deploy/server.env.example](deploy/server.env.example)), then
`systemctl restart spectrevoice`.

## Backups

The server backs itself up every day at `SV_BACKUP_HOUR` (3 AM, server time; `-1` turns it off) into
`SV_BACKUP_DIR` (`/var/lib/spectrevoice/backups`) and keeps the newest `SV_BACKUP_KEEP` (14). Each backup is one
`spectrevoice-backup-YYYYMMDD-HHMMSS.tar.gz`: a consistent copy of the database (SQLite `VACUUM INTO`, taken while
the server runs), every file of the data folder (shared files, channel files, Lei and profiles, the TV playlist, the
server's keys) and `server.env`. It holds the server's secrets: the file is `0600` in a `0700` folder, readable only
by the `spectrevoice` user; copy it somewhere safe the same way. The update cache and the downloaded Plus program are
left out. The admin panel shows the last backup (*Tools › Backup*). A folder outside `/var/lib/spectrevoice` needs
`ReadWritePaths=` for the service (install.sh adds it).

```sh
sudo -u spectrevoice spectrevoice-server backup [folder]       # one now (the server may be running)
sudo bash install.sh --restore /var/lib/spectrevoice/backups/spectrevoice-backup-20261029-030000.tar.gz
```

`--restore` stops the server, checks the archive (its database's checksum and `PRAGMA integrity_check`), moves what
the data folder held to `before-restore-<time>/` inside it, puts the backup in its place (and `server.env`, the old
one kept as `server.env.before-restore`), then starts the server. `spectrevoice-server restore <file>` does the same
for the data folder alone and refuses while the server runs.

## Temporary channels and events

Anyone with *Temporary channels* (on @everyone) makes a voice channel for themselves (a name, optionally a password
and a limit), lands in it and runs it; it's deleted once empty for `SV_TEMP_EMPTY` seconds (60). One per person
(`SV_TEMP_PER_USER`), `SV_TEMP_MAX` (20) on the server; they're never saved, so a restart has none.

Those with *Events* (moderators and admins) put events on the server's calendar: everyone sees them, says
interested or going, and is told `SV_EVENT_LEAD` (15) minutes before and at the start: in the app, or by push where
the person turned push on. Anyone can mute these notices.

## Lei from the owner

The server's owner, and whoever the owner gives *Give Lei* (an administrator doesn't have it by itself), adds or
takes Lei on anyone's purse from the app, with a reason, at most `SV_LEI_MAX` (100000) at a time. The person is told;
every grant is kept (who, to whom, how much, why) and shown in the admin panel.

## Free, with donations

Everything is free on every server: voice up to 256 kbit/s stereo, streams up to 4K60, the admin panel, every
theme. A server has **30 places** (people connected at once). A donation made for it adds places for 30 days:
from +20 to no limit, by the donation's size, and donations add up. In the app: *Server › Donate*, or a donation
code from the website (*Server › Donation code*). `SV_MAX_USERS` sets your own lower cap.

## Bots

Anyone can write a bot for a server: a program with its own key that a server manager registers (`bot-add`),
limited to the permissions it was given, shown with the robot icon, outside the places count, never asked for the
server password. The protocol and a complete example (Go, ~150 lines) are in [docs/BOTS.md](docs/BOTS.md).

## Build

```sh
go test ./... && CGO_ENABLED=0 go build -o spectrevoice-server .
```

## License

[Elastic License 2.0](LICENSE.md), with an added permission: free to use and run, including to host your own
community or business for others; not to offer Spectre Voice servers as a hosting service, and not to change,
disable or get around the places limit and the donation checks (the licence key functionality).

## Places and donations

Every server has 30 places (people connected at once). A donation made for a server, from the app, adds places for
30 days: the server sends the donation together with its own key and its machine's hashed parts to the donation
service, which signs a token for that server on that machine. The places themselves come with the official Plus
build, which the server then downloads (signed by the service, checked before it runs) and keeps in
`/var/lib/spectrevoice/bin`. The Plus build checks in every 6 hours; a week without the service pauses the extra
places until it is reached again. A server without donations never contacts the donation service.
