#!/usr/bin/env bash
set -euo pipefail

[[ $# == 3 && "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo 'usage: generate-scoop-manifest.sh RELEASE_TAG RELEASE_JSON CHECKSUMS' >&2
  exit 1
}
tag=$1
version=${tag#v}
release=$2
checksums=$3
homepage=https://github.com/jacobcxdev/cq
downloads=$homepage/releases/download/$tag
release_api=https://api.github.com/repos/jacobcxdev/cq/releases
asset_downloads=$(jq -er --arg tag "$tag" --arg homepage "$homepage" --arg api "$release_api" '
  select(.tag_name == $tag and (.draft | type == "boolean") and (.assets | type == "array"))
  | select(.id | type == "number" and . > 0 and . == floor)
  | ($api + "/" + (.id | tostring)) as $release_url
  | select(.url == $release_url and .assets_url == ($release_url + "/assets"))
  | ($homepage + "/releases/tag/") as $tag_prefix
  | if .draft then
      .html_url | select(startswith($tag_prefix)) | ltrimstr($tag_prefix)
      | select(test("\\Auntagged-[a-f0-9]+\\z"))
    else
      select(.html_url == ($tag_prefix + $tag)) | $tag
    end
  | $homepage + "/releases/download/" + .
' "$release") || { echo 'invalid GitHub release identity' >&2; exit 1; }

asset_hash() {
  jq -er --arg name "$1" --arg url "$asset_downloads/$1" '
    [.assets[] | select(.name == $name)]
    | select(length == 1) | .[0]
    | select(.state == "uploaded" and .browser_download_url == $url)
    | .digest | select(test("^sha256:[a-f0-9]{64}$")) | ltrimstr("sha256:")
  ' "$release"
}

expected=$(asset_hash checksums.txt)
actual=$(openssl dgst -sha256 -r "$checksums" | cut -d ' ' -f 1)
[[ "$expected" == "$actual" ]] || { echo 'release checksums.txt digest mismatch' >&2; exit 1; }

archive_hash() {
  local expected checksum
  expected=$(asset_hash "$1") || { echo "invalid release asset: $1" >&2; return 1; }
  checksum=$(awk -v name="$1" 'NF == 2 && $2 == name {print $1}' "$checksums")
  [[ "$expected" == "$checksum" ]] || { echo "release checksum mismatch: $1" >&2; return 1; }
  printf '%s\n' "$expected"
}

amd64=cq_${version}_windows_amd64.zip
arm64=cq_${version}_windows_arm64.zip
amd64_hash=$(archive_hash "$amd64")
arm64_hash=$(archive_hash "$arm64")
jq -n --arg version "$version" --arg homepage "$homepage" \
  --arg amd64_url "$downloads/$amd64" --arg amd64_hash "$amd64_hash" \
  --arg arm64_url "$downloads/$arm64" --arg arm64_hash "$arm64_hash" '
  {version:$version, description:"CLI tool to check AI provider quota usage", homepage:$homepage, license:"MIT",
    architecture:{"64bit":{url:$amd64_url,hash:$amd64_hash},arm64:{url:$arm64_url,hash:$arm64_hash}},
    bin:"cq.exe"}'
