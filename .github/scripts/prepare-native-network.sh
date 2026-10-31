#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 || "$(uname -s)" != Darwin || ( $# -eq 3 && "${3:-}" != --allow-draft ) ]]; then
  echo "usage (macOS): prepare-native-network.sh MANIFEST OUTPUT_DIRECTORY [--allow-draft]" >&2
  exit 64
fi
manifest=$1
output=$2
mkdir -p "$output"
temporary=$(mktemp -d "${TMPDIR:-/tmp}/cq-native-release.XXXXXX")
trap 'rm -rf "$temporary"' EXIT
go build -o "$temporary/nativepackage" ./internal/tools/nativepackage
"$temporary/nativepackage" info "$manifest" >"$temporary/info.json"
tag=$(jq -er .tag "$temporary/info.json")
asset=$(jq -er .asset "$temporary/info.json")
asset_id=$(jq -er .asset_id "$temporary/info.json")
version=$(jq -er .version "$temporary/info.json")
source_commit=$(jq -er .source_commit "$temporary/info.json")
minimum_macos=$(jq -er .minimum_macos "$temporary/info.json")
# Go-only releases may reuse a native pin, but changed native sources may not.
git fetch --no-tags origin "$source_commit"
[[ "$(git rev-parse --verify 'FETCH_HEAD^{commit}')" == "$source_commit" ]]
if ! git diff --quiet HEAD -- native/macos-network || [[ -n "$(git ls-files --others --exclude-standard -- native/macos-network)" ]]; then
  echo "native sources have uncommitted changes; commit and sign a new companion before release" >&2
  exit 1
fi
if [[ "$(git rev-parse --verify "$source_commit:native/macos-network")" != "$(git rev-parse --verify HEAD:native/macos-network)" ]]; then
  echo "native sources differ from the pinned companion; sign and pin a new native release" >&2
  exit 1
fi
release_api=$(gh release view "$tag" --repo jacobcxdev/cq --json apiUrl --jq .apiUrl)
if [[ ! "$release_api" =~ ^https://api\.github\.com/repos/jacobcxdev/cq/releases/[1-9][0-9]*$ ]]; then
  echo "native release resolved outside its expected repository API" >&2
  exit 1
fi
gh api "${release_api#https://api.github.com/}" >"$temporary/release.json"
gh api "repos/jacobcxdev/cq/releases/assets/$asset_id" --header 'Accept: application/octet-stream' >"$temporary/$asset"
if [[ $# -eq 3 ]]; then
  "$temporary/nativepackage" verify "$manifest" "$temporary/$asset" "$temporary/release.json" --allow-draft
else
  "$temporary/nativepackage" verify "$manifest" "$temporary/$asset" "$temporary/release.json"
fi
/usr/bin/ditto -x -k "$temporary/$asset" "$temporary/unpacked"
app="$temporary/unpacked/CQNetwork.app"
provider="$app/Contents/Library/SystemExtensions/CQNetworkProxy.systemextension"
requirement='anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "K2648T24P4"'
/usr/bin/codesign --verify --deep --strict --all-architectures "-R=$requirement and identifier \"dev.jacobcx.cq.network\"" "$app"
/usr/bin/codesign --verify --strict --all-architectures "-R=$requirement and identifier \"dev.jacobcx.cq.network.proxy\"" "$provider"
/usr/bin/xcrun stapler validate "$app"
/usr/sbin/spctl --assess --type execute "$app"
for bundle in "$app" "$provider"; do
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$bundle/Contents/Info.plist")" == "$version" ]]
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :LSMinimumSystemVersion' "$bundle/Contents/Info.plist")" == "$minimum_macos" ]]
done
for architecture in arm64 x86_64; do
  /usr/bin/lipo "$app/Contents/MacOS/CQNetwork" -verify_arch "$architecture"
  /usr/bin/lipo "$provider/Contents/MacOS/CQNetworkProxy" -verify_arch "$architecture"
done
"$temporary/nativepackage" cask "$manifest" "$output/cq-network.rb"
cp "$temporary/$asset" "$output/$asset"
echo "Verified pinned CQ Network $version: asset identity, SHA-256, Developer ID, notarisation and universal slices."
