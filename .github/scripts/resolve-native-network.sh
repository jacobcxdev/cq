#!/usr/bin/env bash
set -euo pipefail

if [[ ( $# -ne 2 && $# -ne 3 ) || ! "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ || ( $# -eq 3 && "$3" != --staging-draft && "$3" != --published-fixture ) ]]; then
  echo 'usage: resolve-native-network.sh vVERSION OUTPUT_MANIFEST [--staging-draft|--published-fixture]' >&2
  exit 64
fi

tag=$1
output=$2
version=${tag#v}
stage_tag="cq-native-stage-$tag"
release_tag=$tag
if [[ "${3:-}" == --staging-draft ]]; then
  release_tag=$stage_tag
fi
asset="CQNetwork_${version}_universal.zip"
temporary=$(mktemp -d "${TMPDIR:-/tmp}/cq-native-resolve.XXXXXX")
trap 'rm -rf "$temporary"' EXIT

verified_tag_commit() {
  local ref_tag=$1 object commit
  gh api "repos/jacobcxdev/cq/git/ref/tags/$ref_tag" >"$temporary/$ref_tag-ref.json"
  object=$(jq -er 'select(.object.type == "tag") | .object.sha' "$temporary/$ref_tag-ref.json")
  if [[ ! "$object" =~ ^[0-9a-f]{40}$ ]]; then
    echo 'release source tag is not annotated' >&2
    return 1
  fi
  gh api "repos/jacobcxdev/cq/git/tags/$object" >"$temporary/$ref_tag-tag.json"
  if ! jq -e --arg tag "$ref_tag" '.tag == $tag and .verification.verified == true and .object.type == "commit"' "$temporary/$ref_tag-tag.json" >/dev/null; then
    echo 'release source tag signature is not verified' >&2
    return 1
  fi
  commit=$(jq -er .object.sha "$temporary/$ref_tag-tag.json")
  if [[ "$commit" != "$(git rev-parse --verify "refs/tags/$ref_tag^{commit}")" ]]; then
    echo 'release source tag differs from GitHub identity' >&2
    return 1
  fi
  printf '%s\n' "$commit"
}

# Draft releases are discoverable by the CLI but not the REST tag endpoint.
release_api=$(gh release view "$release_tag" --repo jacobcxdev/cq --json apiUrl --jq .apiUrl)
if [[ ! "$release_api" =~ ^https://api\.github\.com/repos/jacobcxdev/cq/releases/[1-9][0-9]*$ ]]; then
  echo 'native release API identity differs' >&2
  exit 1
fi
gh api "${release_api#https://api.github.com/}" >"$temporary/release.json"
if ! jq -e --arg tag "$tag" --arg release_tag "$release_tag" --arg asset "$asset" '
  .tag_name == $release_tag and .name == $tag and .prerelease == false and
  (.draft | type == "boolean") and
  ([.assets[] | select(.name == $asset)] | length == 1) and
  ([.assets[] | select(.name == $asset and .state == "uploaded")] | length == 1)
' "$temporary/release.json" >/dev/null; then
  echo 'native companion is missing or release identity differs' >&2
  exit 1
fi

source_commit=$(verified_tag_commit "$stage_tag")
target_commit=$(jq -er .target_commitish "$temporary/release.json")
expected_target=$source_commit
if [[ "$release_tag" != "$stage_tag" ]]; then
  expected_target=$(verified_tag_commit "$release_tag")
fi
if [[ "$target_commit" != "$expected_target" ]]; then
  echo 'native release target differs from its tag' >&2
  exit 1
fi
if [[ "$release_tag" == "$stage_tag" && "$(jq -r .draft "$temporary/release.json")" != true ]]; then
  echo 'native staging release must remain a draft' >&2
  exit 1
fi
if [[ "${3:-}" == --published-fixture ]]; then
  if [[ "$(jq -r .draft "$temporary/release.json")" != false ]]; then
    echo 'native packaging fixture must be published' >&2
    exit 1
  fi
  source_commit=$target_commit
fi
asset_id=$(jq -er --arg asset "$asset" '.assets[] | select(.name == $asset) | .id' "$temporary/release.json")
digest=$(jq -er --arg asset "$asset" '.assets[] | select(.name == $asset) | .digest' "$temporary/release.json")
if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo 'native companion digest is missing or invalid' >&2
  exit 1
fi
go run ./internal/tools/nativepackage pin-parent "$version" "$source_commit" "$asset_id" "${digest#sha256:}" 13.0 >"$temporary/manifest.json"
mv "$temporary/manifest.json" "$output"
