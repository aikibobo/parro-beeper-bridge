# parro-beeper-bridge

> **Nederlands:** deze bridge zet je [Parro](https://www.parro.com)-gesprekken als chats in [Beeper](https://www.beeper.com), zodat je schoolberichten naast je andere chats staan. Alleen lezen. Niet officieel: geen band met Topicus, ParnasSys of Beeper. De rest van deze pagina is in het Engels.

Read your Parro school conversations in Beeper. One program on your Mac or Linux machine, read-only.

**Early version.** It runs daily on one Mac with one Parro account. An end-to-end test covers the rest against a fake Parro, but edited and deleted messages, multiple accounts on one login, Intel Macs and Linux have not been seen in practice yet.

**Unofficial.** Not affiliated with or endorsed by Topicus, ParnasSys or Beeper. Parro has no public API: this uses an undocumented one, which can change or stop working at any time. If Topicus asks us to stop, this project stops and the repository is archived. We do not try to work around a block.

## What are Parro and Beeper?

[Parro](https://www.parro.com) is the app many Dutch primary schools use to talk to parents. [Beeper](https://www.beeper.com) puts your chats from WhatsApp, Signal, Instagram and more into one app, using bridges that translate each network to the Matrix protocol. Parro is not one of its networks, but Beeper lets you connect your own: a *self-hosted bridge*, which runs on your own computer and connects to the Beeper server. This project is such a bridge.

```
Parro  <-- reads every 15 min --  parro-beeper-bridge on your computer  -- encrypted -->  Beeper  -->  your chats
```

## What you get

| In Parro | In Beeper |
|---|---|
| Home: school group | A chat with the announcements, title in bold, with attachments |
| Home: class group | Same |
| Chat: child chat, group chats | A chat per conversation, with senders, photos and files |

New groups and chats appear by themselves. On first start each chat gets the last 30 days of history, marked as read.

**Not included:** calendar, likes, read status, and sending. A message typed in Beeper fails with a clear message and never reaches Parro. Reply in the Parro app.

## Requirements

1. A computer that stays on: a Mac (Apple Silicon or Intel) or Linux with systemd (x86_64 or arm64).
2. A Beeper account. On a Mac with [Beeper Desktop](https://www.beeper.com/download) the installer reuses its login, elsewhere `bbctl` asks for your email and a code.
3. A Parro account.
4. Optional: [Go](https://go.dev) to build from source, plus Xcode Command Line Tools on a Mac or a C compiler on Linux. Not needed: without Go the installer downloads the program.

## Install

Always install with `install.sh`. It sets up the program, the Beeper connection, the background services and the watchdog. You never need to download a release file by hand.

```bash
git clone https://github.com/aikibobo/parro-beeper-bridge.git
cd parro-beeper-bridge
./install.sh
```

No `git`? Download the source code zip of the [latest release](https://github.com/aikibobo/parro-beeper-bridge/releases/latest), unpack it and run `./install.sh` in that folder.

What `install.sh` does:

1. **Gets the program.** With [Go](https://go.dev) installed it builds from source. Without Go it downloads the file for your computer from the latest release and checks it against `SHA256SUMS`.
2. **Connects to Beeper.** On a Mac with Beeper Desktop it reuses that login, elsewhere `bbctl` asks for your email and a code.
3. **Installs two background services:** the bridge and a watchdog. launchd agents on macOS, `systemd --user` units on Linux. On Linux, run `sudo loginctl enable-linger $USER` once so they keep running when you are logged out.

Then start a chat in Beeper with `@sh-parrobot:beeper.local`, send `login`, and enter your Parro email and password. The password message is deleted right away and only the login tokens are stored.

Run `./install.sh` again to update.

### Release files

These are what `install.sh` downloads when Go is missing. You only need them yourself if you want to run the program without the installer. Each [release](https://github.com/aikibobo/parro-beeper-bridge/releases) has one file per platform, named `parro-beeper-bridge-<version>-<platform>`:

| Platform | For |
|---|---|
| `macos-apple-silicon` | Mac with M1 or newer |
| `macos-intel` | Mac with an Intel processor |
| `linux-amd64` | Linux on x86_64 |
| `linux-arm64` | Linux on arm64, such as a Raspberry Pi 4 or 5 |

## Privacy: please read

You are moving messages and photos of **other parents and children** out of Parro. Parro's [privacy statement](https://www.parnassys.nl/juridisch/privacystatement-parro/) makes parents responsible for what they share outside the closed group. Keep it to yourself.

1. **Encrypted to Beeper.** The bridge refuses to start unless end-to-bridge encryption is on, so Beeper's servers cannot read the messages.
2. **On your computer.** The bridge database holds your Parro tokens and message IDs, in `~/Library/Application Support/parro-beeper-bridge/` on macOS or `~/.local/share/parro-beeper-bridge/` on Linux, readable only by your user. Message text is not kept by the bridge, but stays in your Beeper apps.
3. **Parro stays untouched.** The client can only send GET requests to the Parro API. Login is the only exception.
4. **Logs** contain IDs and counts, not message text or tokens.

Use it for your own account only.

## FAQ

**Do I miss messages when my Mac is off?** No. Parro keeps them, and they come in when the Mac is back on.

**Why is it called `sh-parro`?** Beeper's `bbctl` requires self-hosted bridge names to start with `sh-`. The program itself is `parro-beeper-bridge`; `sh-parro` is only its name inside Beeper.

**Why is the icon in the Beeper sidebar still a green speech bubble?** Beeper seems to choose that one itself for self-hosted bridges. The chats do show the Parro logo, which the installer downloads to your own Mac.

**What is that alert with a sound?** The watchdog checks every 5 minutes, separately from the bridge. It alerts when the bridge is gone or has not read anything for 45 minutes, and tells you what to do. After the computer wakes from sleep it waits 30 minutes before calling the bridge stale, because nothing could be read while it was off. On Linux it writes to the journal and, with a desktop session, shows a notification. It cannot warn you when the computer itself is off.

**Why not the Homebrew version of `bbctl`?** The tap ships v0.14.0, which the Beeper server rejects as out of date. The installer fetches v0.15.0 and checks its checksum.

## Troubleshooting

1. Logs: `logs/bridge.log` in the data folder above. On Linux also `journalctl --user -u parro-beeper-bridge -u parro-beeper-bridge-watchdog`.
2. Restart: `launchctl kickstart -k gui/$(id -u)/parro-beeper-bridge` on macOS, `systemctl --user restart parro-beeper-bridge` on Linux.
3. Log in again: send `login` to `@sh-parrobot:beeper.local`
4. Recreate all chats with fresh history: create an empty file `reset-portals` in the data folder, then restart.

## Remove

macOS:

```bash
launchctl bootout gui/$(id -u)/parro-beeper-bridge
launchctl bootout gui/$(id -u)/parro-beeper-bridge-watchdog
rm ~/Library/LaunchAgents/parro-beeper-bridge*.plist
bin/bbctl delete sh-parro
rm -r ~/Library/Application\ Support/parro-beeper-bridge
```

Linux:

```bash
systemctl --user disable --now parro-beeper-bridge.service parro-beeper-bridge-watchdog.timer
rm ~/.config/systemd/user/parro-beeper-bridge*
bin/bbctl delete sh-parro
rm -r ~/.local/share/parro-beeper-bridge
```

## How it works

See [docs/DESIGN.md](docs/DESIGN.md).

## Thanks

This project stands on other people's work:

1. **[Willem de Groot](https://github.com/gwillem)**: [gwillem/parro](https://github.com/gwillem/parro) (MIT). The API documentation and login flow that made this possible.
2. **[Anne Schuth](https://github.com/anneschuth)**: [anneschuth/parro-cli](https://github.com/anneschuth/parro-cli) (MIT). Client ID, account chooser, form field handling and paging.
3. **Tulir Asokan and contributors**: [mautrix/go](https://github.com/mautrix/go) (MPL-2.0). The bridgev2 framework this runs on.
4. **Beeper**: [bridge-manager](https://github.com/beeper/bridge-manager) (Apache-2.0). `bbctl`, downloaded during install.

## License

[MIT](LICENSE). Third-party notices are in [NOTICE](NOTICE). This is a hobby project without guarantees: issues may go unanswered.
