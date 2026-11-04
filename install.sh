#!/usr/bin/env bash
# Installs parro-beeper-bridge as a background service with a watchdog: launchd on macOS, systemd --user on Linux.
# Safe to run again to update. Builds from source when Go is installed, otherwise downloads a release binary.
set -euo pipefail
cd "$(dirname "$0")"
REPO=$PWD

NAME=sh-parro
APP=parro-beeper-bridge
LABEL=$APP
WATCHDOG_LABEL=$APP-watchdog
# Names used by earlier versions, cleaned up on install.
LEGACY_LABELS="parro-bridge parro-bridge-watchdog nl.deveneijns.parro-bridge nl.deveneijns.parro-bridge-watchdog"
GITHUB_REPO=aikibobo/parro-beeper-bridge
LOGO_URL=https://talk.parro.com/icons/Icon-512.png
BBCTL=$REPO/bin/bbctl
BBCTL_VERSION=v0.15.0

OS=$(uname -s)
case "$OS/$(uname -m)" in
  Darwin/arm64)
    PLATFORM=macos-apple-silicon
    BBCTL_ASSET=bbctl-macos-arm64
    BBCTL_SHA256=4ed8dcd4bfda2500fef7258d6109dfc200a9d7a4046c878d9749962d1219671c ;;
  Darwin/x86_64)
    PLATFORM=macos-intel
    BBCTL_ASSET=bbctl-macos-amd64
    BBCTL_SHA256=887b8a344c2782c150bb84aecd6c144f5b8d028bf9960622cf497730fb5dbf2c ;;
  Linux/x86_64)
    PLATFORM=linux-amd64
    BBCTL_ASSET=bbctl-linux-amd64
    BBCTL_SHA256=ff3091816429d41aeb51871445f8ccd77864ebe479a57a4b4a5981fd5051a73e ;;
  Linux/aarch64 | Linux/arm64)
    PLATFORM=linux-arm64
    BBCTL_ASSET=bbctl-linux-arm64
    BBCTL_SHA256=aea4f129ac1dffd964d1ad8a57bd2d080c969f277666d0aa734b73de7c6f0033 ;;
  *)
    echo "!! Not supported: $OS $(uname -m)." >&2
    exit 1 ;;
esac
if [ "$OS" = Darwin ]; then
  DATA_PARENT="$HOME/Library/Application Support"
else
  DATA_PARENT="${XDG_DATA_HOME:-$HOME/.local/share}"
fi
DATA="$DATA_PARENT/$APP"
LEGACY_DATA="$DATA_PARENT/parro-bridge"

sha256_check() { # file expected
  if command -v shasum >/dev/null; then
    echo "$2  $1" | shasum -a 256 -c - >/dev/null
  else
    echo "$2  $1" | sha256sum -c - >/dev/null
  fi
}

# The Homebrew tap still ships bbctl v0.14.0, which the Beeper server rejects as out of date.
if [ ! -x "$BBCTL" ]; then
  mkdir -p bin
  curl -fsSL -o "$BBCTL.tmp" "https://github.com/beeper/bridge-manager/releases/download/$BBCTL_VERSION/$BBCTL_ASSET"
  sha256_check "$BBCTL.tmp" "$BBCTL_SHA256"
  chmod +x "$BBCTL.tmp"
  mv "$BBCTL.tmp" "$BBCTL"
fi

logged_in() {
  local out
  out=$("$BBCTL" whoami 2>&1) || return 1
  [[ "$out" == *"User ID:"* ]]
}

if ! logged_in; then
  echo "== Log in to Beeper. On a Mac with Beeper Desktop, answer Yes to use its login."
  "$BBCTL" login
  logged_in || { echo "!! Beeper login failed." >&2; exit 1; }
fi

# Everything that can fail on its own happens before the running bridge is touched.
if command -v go >/dev/null; then
  go build -tags goolm -o "$APP" \
    -ldflags "-X main.Tag=$(git describe --tags --exact-match 2>/dev/null) -X main.Commit=$(git rev-parse HEAD 2>/dev/null)" \
    ./cmd/$APP
  go test -tags goolm ./...
else
  # Release files carry the version, so find the latest tag through the redirect of /releases/latest.
  latest=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$GITHUB_REPO/releases/latest")
  TAG=${latest##*/}
  case "$TAG" in v*) ;; *) echo "!! No release found to download." >&2; exit 1 ;; esac
  FILE="$APP-$TAG-$PLATFORM"
  echo "== Go not found, downloading $FILE"
  BASE="https://github.com/$GITHUB_REPO/releases/download/$TAG"
  curl -fsSL -o "$APP.tmp" "$BASE/$FILE"
  curl -fsSL -o SHA256SUMS.tmp "$BASE/SHA256SUMS"
  want=$(awk -v f="$FILE" '$2 == f {print $1}' SHA256SUMS.tmp)
  [ -n "$want" ] || { echo "!! No checksum for $FILE in the release." >&2; exit 1; }
  sha256_check "$APP.tmp" "$want"
  rm SHA256SUMS.tmp
  chmod +x "$APP.tmp"
  mv "$APP.tmp" "$APP"
fi
[ -f scripts/watchdog.sh ] || { echo "!! scripts/watchdog.sh is missing." >&2; exit 1; }

stop_services() {
  local label
  if [ "$OS" = Darwin ]; then
    local domain agents="$HOME/Library/LaunchAgents"
    domain="gui/$(id -u)"
    for label in $LEGACY_LABELS "$LABEL" "$WATCHDOG_LABEL"; do
      launchctl bootout "$domain/$label" 2>/dev/null || true
      # bootout returns before the service is gone; bootstrap fails with an I/O error until it is.
      for _ in $(seq 1 50); do
        launchctl print "$domain/$label" >/dev/null 2>&1 || break
        sleep 0.2
      done
    done
    for label in $LEGACY_LABELS; do rm -f "$agents/$label.plist"; done
  else
    local units="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
    for label in parro-bridge "$LABEL"; do
      systemctl --user disable --now "$label.service" 2>/dev/null || true
      systemctl --user disable --now "$label-watchdog.timer" 2>/dev/null || true
    done
    rm -f "$units/parro-bridge.service" "$units/parro-bridge-watchdog.service" "$units/parro-bridge-watchdog.timer"
  fi
}

# Only now stop the running bridge: from here on its files move and get replaced.
stop_services
if [ -d "$LEGACY_DATA" ] && [ ! -e "$DATA" ]; then
  mv "$LEGACY_DATA" "$DATA"
  rm -f "$DATA/parro-bridge"
fi

umask 077
mkdir -p "$DATA/logs"
chmod 700 "$DATA" "$DATA/logs"
for f in config.yaml sh-parro.db sh-parro.db-wal sh-parro.db-shm; do
  if [ -e "$REPO/$f" ] && [ ! -e "$DATA/$f" ]; then
    mv "$REPO/$f" "$DATA/$f"
  fi
done
if [ ! -s "$DATA/config.yaml" ]; then
  "$BBCTL" config --type bridgev2 -o "$DATA/config.yaml" "$NAME"
  [ -s "$DATA/config.yaml" ] || { echo "!! Creating the config failed." >&2; exit 1; }
fi
# Debug logging records room and request details; only IDs and counts belong in the logs.
sed -i.bak 's/min_level: debug/min_level: info/; s/min_level: trace/min_level: info/' "$DATA/config.yaml"
rm -f "$DATA/config.yaml.bak"
if [ ! -s "$DATA/parro-logo.png" ]; then
  curl -fsSL -m 20 -o "$DATA/parro-logo.png" "$LOGO_URL" || echo "== Parro logo not downloaded, the chats get no picture."
fi

copy_files() {
  cp "$APP" "$DATA/$APP"
  cp scripts/watchdog.sh "$DATA/watchdog.sh"
  chmod 700 "$DATA/$APP" "$DATA/watchdog.sh"
  chmod 600 "$DATA/config.yaml"
  find "$DATA" -maxdepth 1 -name 'sh-parro.db*' -exec chmod 600 {} +
  find "$DATA/logs" -type f -exec chmod 600 {} +
  [ -e "$DATA/last-poll-ok" ] || touch "$DATA/last-poll-ok"
  rm -f "$DATA/watchdog-count" "$DATA/watchdog-last-alert"
}

install_launchd() {
  local agents="$HOME/Library/LaunchAgents" domain
  domain="gui/$(id -u)"
  mkdir -p "$agents"
  cat > "$agents/$LABEL.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$DATA/$APP</string>
    <string>-c</string>
    <string>config.yaml</string>
  </array>
  <key>WorkingDirectory</key><string>$DATA</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>Umask</key><integer>63</integer>
  <key>StandardOutPath</key><string>$DATA/logs/launchd.log</string>
  <key>StandardErrorPath</key><string>$DATA/logs/launchd.log</string>
</dict>
</plist>
PLIST
  cat > "$agents/$WATCHDOG_LABEL.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$WATCHDOG_LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>$DATA/watchdog.sh</string>
  </array>
  <key>StartInterval</key><integer>300</integer>
</dict>
</plist>
PLIST
  copy_files
  launchctl bootstrap "$domain" "$agents/$LABEL.plist"
  launchctl bootstrap "$domain" "$agents/$WATCHDOG_LABEL.plist"
}

install_systemd() {
  local units="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$units"
  cat > "$units/$LABEL.service" <<UNIT
[Unit]
Description=Parro to Beeper bridge
After=network-online.target

[Service]
WorkingDirectory=$DATA
ExecStart=$DATA/$APP -c config.yaml
Restart=always
RestartSec=30
UMask=0077

[Install]
WantedBy=default.target
UNIT
  cat > "$units/$WATCHDOG_LABEL.service" <<UNIT
[Unit]
Description=Parro to Beeper bridge watchdog

[Service]
Type=oneshot
ExecStart=/bin/bash $DATA/watchdog.sh
UNIT
  cat > "$units/$WATCHDOG_LABEL.timer" <<UNIT
[Unit]
Description=Check the Parro to Beeper bridge every 5 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
UNIT
  copy_files
  systemctl --user daemon-reload
  systemctl --user enable --now "$LABEL.service" "$WATCHDOG_LABEL.timer"
  if [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != yes ]; then
    echo "== To keep the bridge running when you are logged out, run once: sudo loginctl enable-linger $USER"
  fi
}

case "$PLATFORM" in
  macos-*) install_launchd ;;
  linux-*) install_systemd ;;
  *) echo "!! No service setup for $PLATFORM." >&2; exit 1 ;;
esac
echo "== $APP is running as $LABEL"
echo "   First time? In Beeper, send 'login' to @${NAME}bot:beeper.local"
