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
schema_version=$(jq -er .schema_version "$temporary/info.json")
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
if [[ "${schema_version:-1}" == 2 ]]; then
  "$temporary/nativepackage" verify-source "$manifest" "$(git rev-parse --verify 'HEAD^{commit}')"
fi
release_api=$(gh release view "$tag" --repo jacobcxdev/cq --json apiUrl --jq .apiUrl) || {
  # Only a confirmed absent parent release may use its fixed staging draft.
  if [[ "$schema_version" != 2 || "${3:-}" != --allow-draft ]]; then
    exit 1
  fi
  if gh api --include "repos/jacobcxdev/cq/releases/tags/$tag" >"$temporary/parent-response"; then
    echo "parent release exists but could not be resolved" >&2
    exit 1
  fi
  if ! /usr/bin/grep -Eq '^HTTP/[0-9.]+ 404([[:space:]]|$)' "$temporary/parent-response"; then
    echo "parent release absence could not be verified" >&2
    exit 1
  fi
  staging_tag=$(jq -er .staging_tag "$temporary/info.json")
  release_api=$(gh release view "$staging_tag" --repo jacobcxdev/cq --json apiUrl --jq .apiUrl) || exit 1
}
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
provider="$app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension"
provider_identifier=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$provider/Contents/Info.plist")
if [[ "$(/usr/bin/basename "$provider" .systemextension)" != "$provider_identifier" ]]; then
  echo "system extension filename must match its bundle identifier" >&2
  exit 1
fi
requirement='anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "K2648T24P4"'
/usr/bin/codesign --verify --deep --strict --all-architectures "-R=$requirement and identifier \"dev.jacobcx.cq.network\"" "$app"
/usr/bin/codesign --verify --strict --all-architectures "-R=$requirement and identifier \"dev.jacobcx.cq.network.proxy\"" "$provider"
# Validate the stapled ticket.
for attempt in 1 2 3; do
  if /usr/bin/xcrun stapler validate "$app" >"$temporary/stapler-validation.log" 2>&1; then
    cat "$temporary/stapler-validation.log"
    break
  else
    validation_status=$?
  fi
  cat "$temporary/stapler-validation.log" >&2
  if [[ "$validation_status" -ne 68 || "$attempt" -eq 3 ]] || ! /usr/bin/grep -Fq 'NSURLErrorDomain Code=-1005' "$temporary/stapler-validation.log"; then
    exit "$validation_status"
  fi
  echo "Apple ticket connection lost; retrying validation ($attempt/3)." >&2
  sleep 2
done
/usr/sbin/spctl --assess --type execute "$app"
build_number=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleVersion' "$app/Contents/Info.plist")
[[ "$build_number" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]]
[[ "$(/usr/libexec/PlistBuddy -c 'Print :NetworkExtension:NEMachServiceName' "$provider/Contents/Info.plist")" == "K2648T24P4.dev.jacobcx.cq.network.proxy.$build_number" ]]
for bundle in "$app" "$provider"; do
  usage_description=$(/usr/libexec/PlistBuddy -c 'Print :NSSystemExtensionUsageDescription' "$bundle/Contents/Info.plist")
  if ! printf '%s' "$usage_description" | /usr/bin/grep -q '[^[:space:]]'; then
    echo "system extension bundles require a usage description" >&2
    exit 1
  fi
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$bundle/Contents/Info.plist")" == "$version" ]]
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleVersion' "$bundle/Contents/Info.plist")" == "$build_number" ]]
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :LSMinimumSystemVersion' "$bundle/Contents/Info.plist")" == "$minimum_macos" ]]
done
for architecture in arm64 x86_64; do
  /usr/bin/lipo "$app/Contents/MacOS/CQNetwork" -verify_arch "$architecture"
  /usr/bin/lipo "$provider/Contents/MacOS/CQNetworkProxy" -verify_arch "$architecture"
done
"$temporary/nativepackage" cask "$manifest" "$output/cq-network.rb"
cp "$temporary/$asset" "$output/$asset"
echo "Verified pinned CQ Network $version: asset identity, SHA-256, Developer ID, notarisation and universal slices."
