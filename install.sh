#!/usr/bin/env bash
# Installs parro-bridge as a background service with a watchdog: launchd on macOS, systemd --user on Linux.
# Safe to run again to update. Builds from source when Go is installed, otherwise downloads a release binary.
set -euo pipefail
cd "$(dirname "$0")"
REPO=$PWD

NAME=sh-parro
LABEL=parro-bridge
WATCHDOG_LABEL=parro-bridge-watchdog
GITHUB_REPO=aikibobo/parro-beeper-bridge
LOGO_URL=https://talk.parro.com/icons/Icon-512.png
BBCTL=$REPO/bin/bbctl
BBCTL_VERSION=v0.15.0

OS=$(uname -s)
case "$OS/$(uname -m)" in
  Darwin/arm64)
    PLATFORM=darwin-arm64
    BBCTL_SHA256=4ed8dcd4bfda2500fef7258d6109dfc200a9d7a4046c878d9749962d1219671c ;;
  Darwin/x86_64)
    PLATFORM=darwin-amd64
    BBCTL_SHA256=887b8a344c2782c150bb84aecd6c144f5b8d028bf9960622cf497730fb5dbf2c ;;
  Linux/x86_64)
    PLATFORM=linux-amd64
    BBCTL_SHA256=ff3091816429d41aeb51871445f8ccd77864ebe479a57a4b4a5981fd5051a73e ;;
  Linux/aarch64 | Linux/arm64)
    PLATFORM=linux-arm64
    BBCTL_SHA256=aea4f129ac1dffd964d1ad8a57bd2d080c969f277666d0aa734b73de7c6f0033 ;;
  *)
    echo "!! Not supported: $OS $(uname -m)." >&2
    exit 1 ;;
esac
case "$PLATFORM" in
  darwin-*) BBCTL_ASSET=bbctl-macos-${PLATFORM#darwin-}; DATA="$HOME/Library/Application Support/parro-bridge" ;;
  linux-*) BBCTL_ASSET=bbctl-$PLATFORM; DATA="${XDG_DATA_HOME:-$HOME/.local/share}/parro-bridge" ;;
esac

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
  go build -tags goolm -o parro-bridge ./cmd/parro-bridge
  go test -tags goolm ./...
else
  echo "== Go not found, downloading the latest release binary for $PLATFORM"
  BASE="https://github.com/$GITHUB_REPO/releases/latest/download"
  curl -fsSL -o parro-bridge.tmp "$BASE/parro-bridge-$PLATFORM"
  curl -fsSL -o SHA256SUMS.tmp "$BASE/SHA256SUMS"
  want=$(awk -v f="parro-bridge-$PLATFORM" '$2 == f {print $1}' SHA256SUMS.tmp)
  [ -n "$want" ] || { echo "!! No checksum for parro-bridge-$PLATFORM in the release." >&2; exit 1; }
  sha256_check parro-bridge.tmp "$want"
  rm SHA256SUMS.tmp
  chmod +x parro-bridge.tmp
  mv parro-bridge.tmp parro-bridge
fi
[ -f scripts/watchdog.sh ] || { echo "!! scripts/watchdog.sh is missing." >&2; exit 1; }

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
  cp parro-bridge "$DATA/parro-bridge"
  cp scripts/watchdog.sh "$DATA/watchdog.sh"
  chmod 700 "$DATA/parro-bridge" "$DATA/watchdog.sh"
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
    <string>$DATA/parro-bridge</string>
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
  for old in nl.deveneijns.parro-bridge nl.deveneijns.parro-bridge-watchdog; do
    launchctl bootout "$domain/$old" 2>/dev/null || true
    rm -f "$agents/$old.plist"
  done
  launchctl bootout "$domain/$LABEL" 2>/dev/null || true
  launchctl bootout "$domain/$WATCHDOG_LABEL" 2>/dev/null || true
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
ExecStart=$DATA/parro-bridge -c config.yaml
Restart=always
RestartSec=30
UMask=0077

[Install]
WantedBy=default.target
UNIT
  cat > "$units/$WATCHDOG_LABEL.service" <<UNIT
[Unit]
Description=Parro bridge watchdog

[Service]
Type=oneshot
ExecStart=/bin/bash $DATA/watchdog.sh
UNIT
  cat > "$units/$WATCHDOG_LABEL.timer" <<UNIT
[Unit]
Description=Check the Parro bridge every 5 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
UNIT
  systemctl --user stop "$LABEL.service" 2>/dev/null || true
  copy_files
  systemctl --user daemon-reload
  systemctl --user enable --now "$LABEL.service" "$WATCHDOG_LABEL.timer"
  if [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != yes ]; then
    echo "== To keep the bridge running when you are logged out, run once: sudo loginctl enable-linger $USER"
  fi
}

case "$PLATFORM" in
  darwin-*) install_launchd ;;
  linux-*) install_systemd ;;
esac
echo "== parro-bridge is running as $LABEL"
echo "   First time? In Beeper, send 'login' to @${NAME}bot:beeper.local"
