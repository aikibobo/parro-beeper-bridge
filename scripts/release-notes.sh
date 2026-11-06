#!/bin/bash
# Prints the release notes for a tag: its CHANGELOG.md section, then the table of release files.
# Fails when the changelog has no section for the tag, so a release cannot go out without one.
set -euo pipefail
tag=$1
section=$(awk -v tag="$tag" '
  $0 ~ "^## \\[" tag "\\]" { found = 1; next }
  found && /^## \[/ { exit }
  found { print }
' CHANGELOG.md | sed -e '/./,$!d')
[ -n "$section" ] || { echo "CHANGELOG.md has no section for $tag" >&2; exit 1; }
printf '%s\n\n' "$section"
cat <<NOTES
### Files

Most people do not need these files: \`./install.sh\` builds from source, or downloads the right file itself when Go is missing.

| File | For |
|---|---|
| \`parro-beeper-bridge-$tag-macos-apple-silicon\` | Mac with M1 or newer |
| \`parro-beeper-bridge-$tag-macos-intel\` | Mac with an Intel processor |
| \`parro-beeper-bridge-$tag-linux-amd64\` | Linux on x86_64 |
| \`parro-beeper-bridge-$tag-linux-arm64\` | Linux on arm64, such as a Raspberry Pi 4 or 5 |
| \`SHA256SUMS\` | Checksums of the files above |
NOTES
