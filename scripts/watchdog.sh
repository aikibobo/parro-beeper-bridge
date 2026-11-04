#!/bin/bash
# Run every 5 minutes by launchd or a systemd timer, separately from the bridge, so it still works when the bridge
# is dead. Two bad checks in a row raise an alert, so a computer waking from sleep gets time to catch up first.
DATA="$(cd "$(dirname "$0")" && pwd)"
LABEL=parro-beeper-bridge
HEARTBEAT="$DATA/last-poll-ok"
NEEDS_LOGIN="$DATA/needs-login"
COUNT_FILE="$DATA/watchdog-count"
ALERT_FILE="$DATA/watchdog-last-alert"
STALE_AFTER=2700  # 45 minutes, three missed polls
REPEAT_AFTER=7200 # 2 hours between alerts
OS=$(uname -s)

running() {
  if [ "$OS" = Darwin ]; then
    launchctl list "$LABEL" 2>/dev/null | grep -q '"PID"'
  else
    systemctl --user is-active --quiet "$LABEL.service"
  fi
}

mtime() {
  if [ "$OS" = Darwin ]; then stat -f %m "$1"; else stat -c %Y "$1"; fi
}

restart_bridge() {
  if [ "$OS" = Darwin ]; then
    launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/$LABEL.plist" 2>/dev/null
    launchctl kickstart -k "gui/$(id -u)/$LABEL"
  else
    systemctl --user restart "$LABEL.service"
  fi
}

now=$(date +%s)
problem=""
if ! running; then
  problem="stopped"
elif [ -e "$NEEDS_LOGIN" ]; then
  problem="login"
elif [ ! -e "$HEARTBEAT" ] || [ $((now - $(mtime "$HEARTBEAT"))) -gt "$STALE_AFTER" ]; then
  problem="stale"
fi

if [ -z "$problem" ]; then
  rm -f "$COUNT_FILE" "$ALERT_FILE"
  exit 0
fi

count=$(($(cat "$COUNT_FILE" 2>/dev/null || echo 0) + 1))
echo "$count" > "$COUNT_FILE"
[ "$count" -ge 2 ] || exit 0

last=$(cat "$ALERT_FILE" 2>/dev/null || echo 0)
[ $((now - last)) -ge "$REPEAT_AFTER" ] || exit 0
echo "$now" > "$ALERT_FILE"

case "$problem" in
  stopped)
    message="De bridge draait niet. Je mist niets: gemiste berichten komen binnen zodra hij weer draait.

Wat te doen: klik op Herstart bridge."
    linux_message="De bridge draait niet. Herstart: systemctl --user restart $LABEL"
    action="Herstart bridge"
    ;;
  login)
    message="Parro heeft de login geweigerd. Je mist niets: gemiste berichten komen binnen zodra je weer bent ingelogd.

Wat te doen: klik op Open Beeper, ga naar de chat met @sh-parrobot:beeper.local en stuur: login"
    linux_message="Parro heeft de login geweigerd. Stuur in Beeper: login, aan @sh-parrobot:beeper.local"
    action="Open Beeper"
    ;;
  stale)
    message="Al een tijd niets opgehaald bij Parro. Je mist niets: gemiste berichten komen later alsnog binnen.

Wat te doen: controleer of je internet hebt en klik dan op Herstart bridge. Krijg je deze melding daarna opnieuw, klik dan op Open logboek."
    linux_message="Al 45 minuten niets opgehaald bij Parro. Controleer internet, herstart met: systemctl --user restart $LABEL, en kijk in $DATA/logs/bridge.log"
    action="Herstart bridge"
    ;;
esac

if [ "$OS" != Darwin ]; then
  # Linux machines are often headless: the journal always gets it, a desktop session also gets a notification.
  echo "Parro komt niet meer binnen in Beeper: $linux_message" >&2
  command -v notify-send >/dev/null && notify-send -u critical "Parro komt niet meer binnen in Beeper" "$linux_message" 2>/dev/null
  exit 0
fi

/usr/bin/afplay /System/Library/Sounds/Sosumi.aiff &
buttons='{"Later", "'"$action"'"}'
[ "$problem" = stale ] && buttons='{"Later", "Open logboek", "'"$action"'"}'
choice=$(/usr/bin/osascript -e "on run argv
  button returned of (display alert \"Parro komt niet meer binnen in Beeper\" message (item 1 of argv) as critical buttons $buttons default button \"$action\" cancel button \"Later\")
end run" "$message" 2>/dev/null)

case "$choice" in
  "Herstart bridge") restart_bridge ;;
  "Open Beeper") /usr/bin/open -a "Beeper Desktop" ;;
  "Open logboek") /usr/bin/open -a Console "$DATA/logs/bridge.log" ;;
esac
