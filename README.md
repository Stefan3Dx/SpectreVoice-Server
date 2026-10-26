# Spectre Voice Server

Your own server for [Spectre Voice](https://spectrevoice.com): voice channels, chat with history, screen streams,
communities with roles, invite links, a music bot, channel files, games and push notifications. One program, no
database to set up, runs on a small LXC or VPS.

## Install

On Debian 12/13 or Ubuntu 22.04+ (amd64 or arm64), as root:

```sh
curl -fsSL https://raw.githubusercontent.com/Stefan3Dx/SpectreVoice-Server/main/install.sh | sudo bash
```

It asks the server's name and, for a public server, the domain: then it sets up HTTPS (Caddy), a TURN relay
(coturn) and the music bot (ffmpeg, yt-dlp). At the end it prints the address for the apps and a **claim code**:
in the app, *Server › Claim*, type it and the server is yours.

Without questions: `sudo bash install.sh --yes` (LAN) or
`sudo bash install.sh --domain voice.example.com --email you@example.com --yes` (public).
Update: `sudo bash install.sh --update`.

### Ports

| | |
|---|---|
| TCP 443 (and 80 for the certificate) | the apps (HTTPS); on a LAN only TCP 7880 |
| UDP 50000–50200 | voice and streams |
| TCP/UDP 3478, UDP 49152–49352 | TURN (only with a domain) |

At home behind a router: forward these to the server and set `SV_PUBLIC_IP` to your public IP or DDNS name.

## Privacy

Everything stays on this server: accounts are keys made on the devices (no e-mail, no phone number), chat history,
files and settings are here and nowhere else. What leaves it, and only when you turn it on or use it:

- **Push notifications** (`SV_PUSH_CREDENTIALS`): through Google; by default only "there is a message" and which
  server, never who or what (`SV_PUSH_CONTENT=false`). On the phone they're off until the person turns them on.
- **Music bot**: `!play some words` searches YouTube from this server (the words go there); `!play <link>` plays
  only that link (public addresses only).
- **Pro licence**: once a day the licence key and this server's key fingerprint go to the licence service. Nothing
  about the people on it.
- **Crash reports** from the apps go to the developer only if the person says yes, with addresses and ids taken out.

## Settings

`/etc/spectrevoice/server.env` (see [deploy/server.env.example](deploy/server.env.example)), then
`systemctl restart spectrevoice`.

## Free and Pro

Every server is Free: voice up to 96 kbit/s stereo, streams up to 1080p30, 2 communities per person, files up to
25 MB. A Pro licence (bought once, typed in the app: *Server › Licence*) lifts these: studio voice up to 256 kbit/s,
streams up to 4K60, 50 communities per person, files up to 500 MB, cosmetics. The licence is bound to this
server's key; moving to another server, the app asks for it again and it moves there.

## Build

```sh
go test ./... && CGO_ENABLED=0 go build -o spectrevoice-server .
```

## License

[PolyForm Shield 1.0.0](LICENSE.md): free to use, change and run, including for your own community or business;
not to make a product that competes with Spectre Voice.
