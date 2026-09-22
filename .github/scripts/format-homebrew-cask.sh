#!/usr/bin/env bash
set -euo pipefail
if [[ $# -ne 1 || ! -f "$1" ]]; then
  echo "usage: format-homebrew-cask.sh CASK" >&2
  exit 64
fi
formatting_tap=cq-validation/format
if brew tap | grep -Fxq "$formatting_tap"; then
  echo "refusing to replace existing $formatting_tap tap" >&2
  exit 73
fi
HOMEBREW_NO_AUTO_UPDATE=1 brew tap-new --no-git "$formatting_tap" >/dev/null
formatting_root=$(brew --repository "$formatting_tap")
trap 'rm -f "$formatting_root/Casks/cq.rb"; HOMEBREW_NO_AUTO_UPDATE=1 brew untap "$formatting_tap" >/dev/null' EXIT
mkdir -p "$formatting_root/Casks"
cp "$1" "$formatting_root/Casks/cq.rb"
HOMEBREW_NO_AUTO_UPDATE=1 brew style --fix "$formatting_tap/cq"
HOMEBREW_NO_AUTO_UPDATE=1 brew style "$formatting_tap/cq"
cp "$formatting_root/Casks/cq.rb" "$1"
