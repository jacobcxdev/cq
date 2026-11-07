#!/usr/bin/env bash
set -euo pipefail

# CI runners are disposable. Install the validated dependency without enabling
# the extension so the existing CQ Cask lifecycle checks use the real dependency.
if [[ $# -ne 2 || "$(uname -s)" != Darwin || "${CI:-}" != true ]]; then
  echo "usage (disposable macOS CI only): install-native-network-validation.sh VERIFIED_CASK VERIFIED_ARCHIVE" >&2
  exit 64
fi
[[ -f "$1" && -f "$2" ]]
if brew tap | grep -Fxq jacobcxdev/tap; then
  echo "refusing to replace an existing jacobcxdev/tap checkout" >&2
  exit 73
fi
if [[ -e /Applications/CQNetwork.app ]] || brew list --cask cq-network >/dev/null 2>&1; then
  echo "refusing to replace an existing CQ Network installation" >&2
  exit 73
fi
HOMEBREW_NO_AUTO_UPDATE=1 brew tap-new --no-git jacobcxdev/tap
tap_root=$(brew --repository jacobcxdev/tap)
mkdir -p "$tap_root/Casks"
verified_cask=$1
selected_cask="$tap_root/Casks/cq-network.rb"
ruby - "$verified_cask" "$2" "$selected_cask" <<'RUBY'
source, archive, destination = ARGV
text = File.read(source)
text.sub!(/^  url ".*"$/, "  url \"file://#{File.expand_path(archive)}\"") or abort "missing native Cask URL"
File.write(destination, text)
RUBY
# Only the installation uses the local archive. Later dependency checks must
# resolve the original reviewed release URL, including after a failed install.
trap 'cp "$verified_cask" "$selected_cask"' EXIT
HOMEBREW_NO_AUTO_UPDATE=1 brew install --cask jacobcxdev/tap/cq-network
