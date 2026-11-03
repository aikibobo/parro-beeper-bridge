#!/usr/bin/env bash
# Builds parro-bridge and installs it as one launchd agent. Safe to run again after an update.
set -euo pipefail
cd "$(dirname "$0")"
REPO=$PWD

NAME=sh-parro
LABEL=parro-bridge
WATCHDOG_LABEL=parro-bridge-watchdog
LEGACY_LABELS="nl.deveneijns.parro-bridge nl.deveneijns.parro-bridge-watchdog"
AGENTS="$HOME/Library/LaunchAgents"
PLIST="$AGENTS/$LABEL.plist"
WATCHDOG_PLIST="$AGENTS/$WATCHDOG_LABEL.plist"
DOMAIN="gui/$(id -u)"
DATA="$HOME/Library/Application Support/parro-bridge"
LOGO_URL=https://talk.parro.com/icons/Icon-512.png
BBCTL=$REPO/bin/bbctl
BBCTL_VERSION=v0.15.0

[ "$(uname -s)" = Darwin ] || { echo "!! Dit script ondersteunt alleen macOS." >&2; exit 1; }
command -v go >/dev/null || { echo "!! Go ontbreekt. Installeer het met: brew install go" >&2; exit 1; }
case "$(uname -m)" in
  arm64)
    BBCTL_ASSET=bbctl-macos-arm64
    BBCTL_SHA256=4ed8dcd4bfda2500fef7258d6109dfc200a9d7a4046c878d9749962d1219671c
    ;;
  x86_64)
    BBCTL_ASSET=bbctl-macos-amd64
    BBCTL_SHA256=887b8a344c2782c150bb84aecd6c144f5b8d028bf9960622cf497730fb5dbf2c
    ;;
  *)
    echo "!! Onbekende processor: $(uname -m)" >&2
    exit 1
    ;;
esac

# The Homebrew tap still ships v0.14.0, which the Beeper server rejects as out of date.
if [ ! -x "$BBCTL" ]; then
  mkdir -p bin
  curl -fsSL -o "$BBCTL.tmp" "https://github.com/beeper/bridge-manager/releases/download/$BBCTL_VERSION/$BBCTL_ASSET"
  echo "$BBCTL_SHA256  $BBCTL.tmp" | shasum -a 256 -c - >/dev/null
  chmod +x "$BBCTL.tmp"
  mv "$BBCTL.tmp" "$BBCTL"
fi

logged_in() {
  local out
  out=$("$BBCTL" whoami 2>&1) || return 1
  [[ "$out" == *"User ID:"* ]]
}

if ! logged_in; then
  echo "== Log in bij Beeper. Kies Yes als gevraagd wordt om je Beeper Desktop-login te gebruiken."
  "$BBCTL" login
  logged_in || { echo "!! Beeper-login is niet gelukt." >&2; exit 1; }
fi

# Everything that can fail on its own happens before the running bridge is touched.
go build -tags goolm -o parro-bridge ./cmd/parro-bridge
go test -tags goolm ./...
[ -f scripts/watchdog.sh ] || { echo "!! scripts/watchdog.sh ontbreekt." >&2; exit 1; }

umask 077
mkdir -p "$DATA/logs" "$AGENTS"
chmod 700 "$DATA" "$DATA/logs"
for f in config.yaml sh-parro.db sh-parro.db-wal sh-parro.db-shm; do
  if [ -e "$REPO/$f" ] && [ ! -e "$DATA/$f" ]; then
    mv "$REPO/$f" "$DATA/$f"
  fi
done
if [ ! -s "$DATA/config.yaml" ]; then
  "$BBCTL" config --type bridgev2 -o "$DATA/config.yaml" "$NAME"
  [ -s "$DATA/config.yaml" ] || { echo "!! Config maken is niet gelukt." >&2; exit 1; }
fi
# Debug logging records room and request details; only IDs and counts belong in the logs.
sed -i '' 's/min_level: debug/min_level: info/; s/min_level: trace/min_level: info/' "$DATA/config.yaml"
if [ ! -s "$DATA/parro-logo.png" ]; then
  curl -fsSL -m 20 -o "$DATA/parro-logo.png" "$LOGO_URL" || echo "== Parro-logo niet opgehaald, de chats krijgen geen plaatje."
fi

write_plist() {
  cat > "$1"
}

write_plist "$PLIST" <<PLIST
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

write_plist "$WATCHDOG_PLIST" <<PLIST
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

for old in $LEGACY_LABELS; do
  launchctl bootout "$DOMAIN/$old" 2>/dev/null || true
  rm -f "$AGENTS/$old.plist"
done
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
launchctl bootout "$DOMAIN/$WATCHDOG_LABEL" 2>/dev/null || true

cp parro-bridge "$DATA/parro-bridge"
cp scripts/watchdog.sh "$DATA/watchdog.sh"
chmod 700 "$DATA/parro-bridge" "$DATA/watchdog.sh"
chmod 600 "$DATA/config.yaml"
find "$DATA" -maxdepth 1 -name 'sh-parro.db*' -exec chmod 600 {} +
find "$DATA/logs" -type f -exec chmod 600 {} +
[ -e "$DATA/last-poll-ok" ] || touch "$DATA/last-poll-ok"
rm -f "$DATA/watchdog-count" "$DATA/watchdog-last-alert"

launchctl bootstrap "$DOMAIN" "$PLIST"
launchctl bootstrap "$DOMAIN" "$WATCHDOG_PLIST"
echo "== parro-bridge draait als launchd-agent $LABEL"
echo "   Is dit de eerste keer? Stuur in Beeper 'login' aan @${NAME}bot:beeper.local"
