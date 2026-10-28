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
- **Donations for places**: when you redeem one, this server's key fingerprint goes to the donation service.
  Nothing about the people on it.
- **Crash reports** from the apps go to the developer only if the person says yes, with addresses and ids taken out.

## Settings

`/etc/spectrevoice/server.env` (see [deploy/server.env.example](deploy/server.env.example)), then
`systemctl restart spectrevoice`.

## Free, with donations

Everything is free on every server: voice up to 256 kbit/s stereo, streams up to 4K60, the admin panel, every
theme. A server has **30 places** (people connected at once). A donation made for it adds places for 30 days:
from +20 to no limit, by the donation's size, and donations add up. In the app: *Server › Donate*, or a donation
code from the website (*Server › Donation code*). `SV_MAX_USERS` sets your own lower cap.

## Bots

Anyone can write a bot for a server: a program with its own key that a community manager registers (`bot-add`),
limited to that community and to the permissions it was given, shown with the robot icon, outside the places
count. The protocol and a complete example (Go, ~150 lines) are in [docs/BOTS.md](docs/BOTS.md).

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
