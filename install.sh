#!/usr/bin/env bash
# Spectre Voice Server — install or update, plug and play (Debian 12/13, Ubuntu 22.04+; LXC, VM or VPS).
#
#   curl -fsSL https://raw.githubusercontent.com/Stefan3Dx/SpectreVoice-Server/main/install.sh | sudo bash
#   sudo bash install.sh                 asks a few questions (Enter keeps the default)
#   sudo bash install.sh --yes           no questions: LAN server, defaults
#   sudo bash install.sh --update        newest version, settings kept
#   sudo bash install.sh --domain voice.example.com --email you@example.com --yes   public, HTTPS + TURN
#   sudo bash install.sh --source        build from this folder's source instead of downloading
#
# Options: --name NAME  --domain NAME  --email ADDR  --contact ADDR  --public-ip IP|DDNS  --no-music  --no-turn  --udp 50000-50200
set -euo pipefail

REPO="Stefan3Dx/SpectreVoice-Server"
GO_VERSION=1.26.0
BIN=/usr/local/bin/spectrevoice-server
ETC=/etc/spectrevoice
DATA=/var/lib/spectrevoice
ENVF=$ETC/server.env

YES=0 UPDATE=0 SOURCE=0 MUSIC=1 TURN=1
NAME="" DOMAIN="" EMAIL="" CONTACT="" PUBLIC="" UDP="50000-50200"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --yes|-y) YES=1 ;;
    --update) UPDATE=1; YES=1 ;;
    --source) SOURCE=1 ;;
    --no-music) MUSIC=0 ;;
    --no-turn) TURN=0 ;;
    --name) NAME="$2"; shift ;;
    --domain) DOMAIN="$2"; shift ;;
    --email) EMAIL="$2"; shift ;;
    --contact) CONTACT="$2"; shift ;;
    --public-ip) PUBLIC="$2"; shift ;;
    --udp) UDP="$2"; shift ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown option: $1"; exit 1 ;;
  esac
  shift
done

say()  { printf '\033[1;36m▸ %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m! %s\033[0m\n' "$*"; }
ask() { # ask VAR "question" default
  local var=$1 q=$2 def=${3:-} a=""
  if [[ $YES -eq 1 || ! -t 0 && ! -e /dev/tty ]]; then printf -v "$var" '%s' "${!var:-$def}"; return; fi
  [[ -n "${!var:-}" ]] && def=${!var}
  read -rp "$q [${def}]: " a </dev/tty || true
  printf -v "$var" '%s' "${a:-$def}"
}
yesno() { # yesno VAR "question" 1|0
  local var=$1 q=$2 def=$3 a=""
  if [[ $YES -eq 1 ]]; then return; fi
  read -rp "$q [$([[ $def -eq 1 ]] && echo Y/n || echo y/N)]: " a </dev/tty || true
  case "${a,,}" in y|yes|d|da) printf -v "$var" 1 ;; n|no|nu) printf -v "$var" 0 ;; *) printf -v "$var" '%s' "$def" ;; esac
}
setenv() { # setenv KEY VALUE (in server.env)
  local k=$1 v=$2
  local e=${v//\\/\\\\}; e=${e//|/\\|}; e=${e//&/\\&} # sed's special characters in the value
  if grep -qE "^#?\s*${k}=" "$ENVF"; then sed -i -E "s|^#?\s*${k}=.*|${k}=${e}|" "$ENVF"; else printf '%s=%s\n' "$k" "$v" >>"$ENVF"; fi
}

[[ $EUID -eq 0 ]] || { echo "Run as root: sudo bash install.sh"; exit 1; }
command -v apt-get >/dev/null || { echo "Debian or Ubuntu needed (apt-get)."; exit 1; }
ARCH=$(dpkg --print-architecture)
case "$ARCH" in amd64|arm64) ;; *) echo "Unsupported CPU: $ARCH (amd64 or arm64)"; exit 1 ;; esac
HERE=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || echo /tmp)
grep -qs "spectrevoice-server" "$HERE/go.mod" || HERE=/nonexistent # run through curl: nothing local

say "Packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl ca-certificates tar iproute2 >/dev/null

# ---- Questions (first install only) ----
LANIP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}' | head -1)
FIRST=0
[[ -f $ENVF ]] || FIRST=1
[[ $UPDATE -eq 1 && $FIRST -eq 1 ]] && warn "Nothing installed yet: installing with the defaults"
# An update keeps what was chosen: the music bot only if it's on
if [[ $FIRST -eq 0 ]]; then if grep -q "^SV_MUSIC=true" "$ENVF"; then MUSIC=1; else MUSIC=0; fi; fi
if [[ $FIRST -eq 1 ]]; then
  ask NAME "Server name" "Spectre Voice"
  # You hold the people's data on your server (GDPR): your privacy page (/privacy) gives this address for their requests
  ask CONTACT "Your e-mail for people's data requests (shown at /privacy)" "${EMAIL:-}"
  ask DOMAIN "Domain for HTTPS (empty: LAN only, e.g. voice.example.com)" ""
  if [[ -n "$DOMAIN" ]]; then
    ask EMAIL "E-mail for the HTTPS certificate (Let's Encrypt)" ""
    PUBDEF=$(curl -fsS4 --max-time 5 https://api.ipify.org 2>/dev/null || echo "$DOMAIN")
    ask PUBLIC "Public IP or DDNS name the apps reach for voice" "$PUBDEF"
    yesno TURN "TURN relay for networks that block UDP (coturn)" "$TURN"
  else
    ask PUBLIC "The LAN address of this server" "$LANIP"
    TURN=0
  fi
  yesno MUSIC "Music bot (!play, installs ffmpeg and yt-dlp)" "$MUSIC"
fi

# ---- The program ----
TAG=main # the service file comes from the same release as the program
install_binary() {
  [[ $SOURCE -eq 1 ]] && return 1
  TAG=$(curl -fsSI "https://github.com/${REPO}/releases/latest" 2>/dev/null | awk -F/ 'tolower($1) ~ /^location:/ {print $NF}' | tr -d '\r' || true)
  [[ -n "$TAG" ]] || TAG=main
  local url="https://github.com/${REPO}/releases/latest/download/spectrevoice-server-linux-${ARCH}.tar.gz"
  local tmp; tmp=$(mktemp -d)
  if curl -fsSL "$url" -o "$tmp/s.tgz" && curl -fsSL "https://github.com/${REPO}/releases/latest/download/SHA256SUMS" -o "$tmp/SHA256SUMS"; then
    (cd "$tmp" && grep "linux-${ARCH}.tar.gz" SHA256SUMS | sed "s|spectrevoice-server-linux-${ARCH}.tar.gz|s.tgz|" | sha256sum -c --quiet) || { warn "checksum mismatch"; rm -rf "$tmp"; return 1; }
    tar -xzf "$tmp/s.tgz" -C "$tmp"
    install -m 755 "$tmp/spectrevoice-server" "$BIN"
    rm -rf "$tmp"
    return 0
  fi
  rm -rf "$tmp"
  return 1
}
build_source() {
  local src="$HERE"
  if [[ ! -f "$src/go.mod" ]]; then
    apt-get install -y -qq git >/dev/null
    src=/opt/spectrevoice-server
    if [[ -d $src/.git ]]; then git -C "$src" pull -q --ff-only; else git clone -q --depth 1 "https://github.com/${REPO}.git" "$src"; fi
  fi
  if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
    say "Go ${GO_VERSION}"
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tgz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
  fi
  local ver; ver=$(git -C "$src" describe --tags --always 2>/dev/null || echo dev)
  (cd "$src" && CGO_ENABLED=0 /usr/local/go/bin/go build -trimpath -ldflags "-s -w -X main.version=$ver" -o /tmp/spectrevoice-server .)
  install -m 755 /tmp/spectrevoice-server "$BIN" && rm /tmp/spectrevoice-server
}
say "Spectre Voice Server"
install_binary || { say "No release to download: building from source"; build_source; }

# ---- User, folders, settings, service ----
id spectrevoice &>/dev/null || useradd --system --home "$DATA" --shell /usr/sbin/nologin spectrevoice
install -d -o spectrevoice -g spectrevoice -m 750 "$DATA"
install -d -o root -g spectrevoice -m 750 "$ETC"
if [[ $FIRST -eq 1 ]]; then
  if [[ -f "$HERE/deploy/server.env.example" ]]; then cp "$HERE/deploy/server.env.example" "$ENVF"
  else curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/deploy/server.env.example" -o "$ENVF"; fi
  chown root:spectrevoice "$ENVF" && chmod 640 "$ENVF"
  setenv SV_NAME "$NAME"
  [[ -n "$CONTACT" ]] && setenv SV_CONTACT_EMAIL "$CONTACT"
  setenv SV_PUBLIC_IP "$PUBLIC"
  setenv SV_UDP_MIN "${UDP%-*}"
  setenv SV_UDP_MAX "${UDP#*-}"
  setenv SV_MUSIC "$([[ $MUSIC -eq 1 ]] && echo true || echo false)"
  if [[ -n "$DOMAIN" ]]; then
    setenv SV_INVITE_BASE "https://${DOMAIN}/i/"
    setenv SV_LISTEN "127.0.0.1:7880" # from outside only through Caddy (HTTPS)
  fi
fi
unit=/etc/systemd/system/spectrevoice.service
if [[ -f "$HERE/deploy/spectrevoice.service" ]]; then cp "$HERE/deploy/spectrevoice.service" "$unit"
else curl -fsSL "https://raw.githubusercontent.com/${REPO}/${TAG}/deploy/spectrevoice.service" -o "$unit"; fi

# ---- Music bot ----
if [[ $MUSIC -eq 1 ]]; then
  say "Music bot: ffmpeg and yt-dlp"
  apt-get install -y -qq ffmpeg >/dev/null
  ytarch=$([[ $ARCH == arm64 ]] && echo yt-dlp_linux_aarch64 || echo yt-dlp_linux)
  curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/latest/download/${ytarch}" -o /usr/local/bin/yt-dlp && chmod 755 /usr/local/bin/yt-dlp
  # YouTube breaks old yt-dlp within weeks: keep it new (daily)
  printf '[Unit]\nDescription=Update yt-dlp (Spectre Voice music bot)\n[Service]\nType=oneshot\nExecStart=/usr/local/bin/yt-dlp -U\n' >/etc/systemd/system/spectrevoice-ytdlp.service
  printf '[Unit]\nDescription=Update yt-dlp daily\n[Timer]\nOnCalendar=daily\nRandomizedDelaySec=6h\nPersistent=true\n[Install]\nWantedBy=timers.target\n' >/etc/systemd/system/spectrevoice-ytdlp.timer
  systemctl daemon-reload
  systemctl enable --now spectrevoice-ytdlp.timer >/dev/null 2>&1 || true
fi

# ---- HTTPS (Caddy) ----
if [[ $FIRST -eq 1 && -n "$DOMAIN" ]]; then
  say "HTTPS for ${DOMAIN} (Caddy)"
  if ! command -v caddy >/dev/null; then
    apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https gnupg >/dev/null
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' >/etc/apt/sources.list.d/caddy-stable.list
    apt-get update -qq && apt-get install -y -qq caddy >/dev/null
  fi
  # Our site in its own file; an existing Caddyfile (other sites) is kept and only imports it
  printf '%s {\n\tencode zstd gzip\n\treverse_proxy 127.0.0.1:7880\n}\n' "$DOMAIN" >/etc/caddy/spectrevoice.caddy
  if [[ ! -s /etc/caddy/Caddyfile ]] || grep -q "The Caddyfile is an easy way" /etc/caddy/Caddyfile; then
    {
      [[ -n "$EMAIL" ]] && printf '{\n\temail %s\n}\n\n' "$EMAIL"
      echo "import /etc/caddy/spectrevoice.caddy"
    } >/etc/caddy/Caddyfile
  elif ! grep -q "spectrevoice.caddy" /etc/caddy/Caddyfile; then
    cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%s)"
    printf '\nimport /etc/caddy/spectrevoice.caddy\n' >>/etc/caddy/Caddyfile
  fi
  systemctl enable --now caddy >/dev/null 2>&1 || true
  systemctl reload caddy 2>/dev/null || systemctl restart caddy
fi

# ---- TURN (coturn) ----
if [[ $FIRST -eq 1 && $TURN -eq 1 && -n "$DOMAIN" ]]; then
  say "TURN relay (coturn)"
  apt-get install -y -qq coturn >/dev/null
  tpass=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
  cat >/etc/turnserver.conf <<EOF
listening-port=3478
fingerprint
lt-cred-mech
user=spectre:${tpass}
realm=${DOMAIN}
min-port=49152
max-port=49352
no-cli
no-tlsv1
no-tlsv1_1
no-multicast-peers
# Relay only to the internet, never into this network
denied-peer-ip=0.0.0.0-0.255.255.255
denied-peer-ip=10.0.0.0-10.255.255.255
denied-peer-ip=100.64.0.0-100.127.255.255
denied-peer-ip=127.0.0.0-127.255.255.255
denied-peer-ip=169.254.0.0-169.254.255.255
denied-peer-ip=172.16.0.0-172.31.255.255
denied-peer-ip=192.168.0.0-192.168.255.255
denied-peer-ip=198.18.0.0-198.19.255.255
denied-peer-ip=224.0.0.0-255.255.255.255
denied-peer-ip=::1
denied-peer-ip=fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff
denied-peer-ip=fe80::-febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff
EOF
  # external-ip takes an address, not a name (DDNS): resolve it
  extip=$PUBLIC
  [[ $extip =~ ^[0-9.]+$ ]] || extip=$(getent ahostsv4 "$PUBLIC" 2>/dev/null | awk 'NR==1{print $1}')
  if [[ -n "$extip" ]]; then echo "external-ip=${extip}" >>/etc/turnserver.conf; fi
  sed -i 's/^#\?TURNSERVER_ENABLED=.*/TURNSERVER_ENABLED=1/' /etc/default/coturn 2>/dev/null || true
  systemctl enable --now coturn >/dev/null 2>&1 || true
  systemctl restart coturn || true
  setenv SV_TURN "turn:${DOMAIN}:3478"
  setenv SV_TURN_USER spectre
  setenv SV_TURN_PASS "$tpass"
fi

# The system journal (the server's log: connections, uploads, moderation) kept 30 days at most (privacy policy)
if [[ ! -f /etc/systemd/journald.conf.d/spectrevoice.conf ]]; then
  install -d /etc/systemd/journald.conf.d
  printf '[Journal]\nMaxRetentionSec=30day\n' >/etc/systemd/journald.conf.d/spectrevoice.conf
  systemctl restart systemd-journald 2>/dev/null || true
fi

systemctl daemon-reload
systemctl enable spectrevoice >/dev/null
systemctl restart spectrevoice

# ---- Done ----
sleep 2
if ! systemctl is-active --quiet spectrevoice; then
  journalctl -u spectrevoice -n 30 --no-pager
  echo "The server didn't start; see above."; exit 1
fi
claim=$(cat "$DATA/claim.code" 2>/dev/null || true)
echo
say "Spectre Voice Server $($BIN version 2>/dev/null || echo) is running"
if [[ -n "${DOMAIN:-}" ]]; then echo "  Address in the apps:  ${DOMAIN}"; else echo "  Address in the apps:  ${LANIP}  (LAN)"; fi
if [[ -n "$claim" ]]; then
  echo "  Make it yours:        in the app, Server › Claim, type  ${claim}"
fi
echo "  Ports to open:        TCP 80, 443 (HTTPS) or 7880 (LAN) · UDP ${UDP}$([[ $TURN -eq 1 && -n "${DOMAIN:-}" ]] && echo ' · TCP/UDP 3478, UDP 49152-49352 (TURN)')"
echo "  Settings:             ${ENVF}  (then: systemctl restart spectrevoice)"
echo "  Logs:                 journalctl -u spectrevoice -f"
echo "  Update:               sudo bash install.sh --update"
