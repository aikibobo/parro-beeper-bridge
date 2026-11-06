# Changelog

What changed per release, newest first. The release notes on GitHub show the same text.

## [Unreleased]

## [v0.2.2] - 2026-11-06

### Fixed
- No more false "nothing fetched" alert after the computer was asleep or off. The watchdog now waits 30 minutes after waking before it calls the bridge stale.
- After waking without a network connection, the bridge retries after 1, 2, 4 and 8 minutes instead of waiting 30 minutes, and polls at once when the computer wakes.

### Changed
- The install section of the README makes clear that `install.sh` is always the way to install, and what the release files are for.

## [v0.2.1] - 2026-11-04

### Fixed
- Release builds show their real version. v0.2.0 reported itself as `v0.1.0+dev`.
- Builds from source are stamped with the git tag and commit.

## [v0.2.0] - 2026-11-04

### Changed
- The program, its background services and its data folder are now called `parro-beeper-bridge`, like the repository. `install.sh` moves an existing installation, including the Parro login.
- Release files carry the version and a readable platform name: `macos-apple-silicon`, `macos-intel`, `linux-amd64`, `linux-arm64`.

### Fixed
- The installer refuses an unknown platform instead of skipping the service setup.

## [v0.1.0] - 2026-11-04

First public release.

### Added
- Read-only bridge that shows Parro announcements and chats in Beeper, with photos and PDFs and 30 days of history.
- Installer for macOS (launchd) and Linux (`systemd --user`), with a watchdog that alerts when the bridge stops working.
- Release binaries for macOS and Linux on arm64 and amd64, used by the installer when Go is not installed.
- End-to-end test of the bridge against fake Parro and Matrix servers.

[Unreleased]: https://github.com/aikibobo/parro-beeper-bridge/compare/v0.2.2...HEAD
[v0.2.2]: https://github.com/aikibobo/parro-beeper-bridge/compare/v0.2.1...v0.2.2
[v0.2.1]: https://github.com/aikibobo/parro-beeper-bridge/compare/v0.2.0...v0.2.1
[v0.2.0]: https://github.com/aikibobo/parro-beeper-bridge/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/aikibobo/parro-beeper-bridge/releases/tag/v0.1.0
