#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 || ! "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ || ! "$2" =~ ^[1-9][0-9]*$ ]]; then
  echo 'usage: prepare-native-ci-release.sh vVERSION BUILD_NUMBER OUTPUT_DIRECTORY' >&2
  exit 64
fi
tag=$1
build=$2
output=$3
scripts="$(cd "$(dirname "$0")" && pwd)"
repository=jacobcxdev/cq
stage_tag="cq-native-stage-$tag"
asset="CQNetwork_${tag#v}_universal.zip"
temporary=$(mktemp -d "${TMPDIR:-/tmp}/cq-native-ci.XXXXXX")
trap 'rm -rf "$temporary"' EXIT
fail() { echo "$*" >&2; exit 1; }
[[ "${GITHUB_REPOSITORY:-$repository}" == "$repository" ]] || fail 'unexpected release repository'
case "$(git remote get-url origin)" in
  https://github.com/jacobcxdev/cq|https://github.com/jacobcxdev/cq.git|git@github.com:jacobcxdev/cq.git|ssh://git@github.com/jacobcxdev/cq.git) ;;
  *) fail 'origin is not the release repository' ;;
esac

# Refresh all identities without rewriting existing tags. GitHub must verify
# both annotated signatures before any signing credentials are requested.
git fetch --no-tags origin main "refs/tags/$tag:refs/tags/$tag" "refs/tags/$stage_tag:refs/tags/$stage_tag"
source_commit=$(git rev-parse --verify 'HEAD^{commit}')
[[ "$source_commit" == "$(git rev-parse --verify 'FETCH_HEAD^{commit}')" ]] || fail 'checkout is not current origin/main'
verified_tag() {
  local name=$1 object commit
  gh api "repos/$repository/git/ref/tags/$name" > "$temporary/ref.json"
  object=$(jq -er 'select(.object.type == "tag") | .object.sha' "$temporary/ref.json")
  [[ "$object" =~ ^[0-9a-f]{40}$ ]] || fail 'release tag is not annotated'
  gh api "repos/$repository/git/tags/$object" > "$temporary/tag.json"
  jq -e --arg name "$name" '.tag == $name and .verification.verified == true and .object.type == "commit"' "$temporary/tag.json" >/dev/null || fail 'release tag signature is not verified'
  commit=$(jq -er .object.sha "$temporary/tag.json")
  [[ "$commit" == "$source_commit" && "$commit" == "$(git rev-parse --verify "refs/tags/$name^{commit}")" ]] || fail 'release tag does not identify current main'
  [[ "$(git rev-parse --verify "$commit:native/macos-network")" == "$(git rev-parse --verify HEAD:native/macos-network)" ]] || fail 'native source identity differs'
}
verified_tag "$tag"
verified_tag "$stage_tag"
git diff --quiet HEAD -- native/macos-network || fail 'native sources are dirty'
[[ -z "$(git ls-files --others --exclude-standard -- native/macos-network)" ]] || fail 'native sources are untracked'

gh api --paginate --slurp "repos/$repository/releases?per_page=100" > "$temporary/pages.json"
jq -e 'add | type == "array"' "$temporary/pages.json" >/dev/null
jq 'add' "$temporary/pages.json" > "$temporary/releases.json"
previous_tag=$(jq -er '[.[] | select(.draft == false and .prerelease == false and (.tag_name | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")))] | max_by(.tag_name | ltrimstr("v") | split(".") | map(tonumber)) | .tag_name' "$temporary/releases.json")
[[ "$previous_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'published release baseline unavailable'
IFS=. read -r major minor patch <<< "${previous_tag#v}"
[[ "${tag#v}" == "$major.$minor.$((patch + 1))" || "${tag#v}" == "$major.$((minor + 1)).0" || "${tag#v}" == "$((major + 1)).0.0" ]] || fail 'release is not the next stable version'
git merge-base --is-ancestor "$previous_tag" "$source_commit"

# A failed or concurrent attempt must be reconciled, never silently replaced.
jq --arg tag "$tag" '[.[] | select(.tag_name == $tag)]' "$temporary/releases.json" > "$temporary/parent.json"
[[ "$(jq 'length' "$temporary/parent.json")" -le 1 ]] || fail 'ambiguous parent release'
jq -e --arg tag "$tag" '[.[] | select(.draft == true and .tag_name != $tag)] | length == 0' "$temporary/releases.json" >/dev/null || fail 'another release draft is active'
existing=false
if [[ "$(jq 'length' "$temporary/parent.json")" == 1 ]]; then
  existing=true
  jq -e --arg tag "$tag" --arg commit "$source_commit" '.[0] | .tag_name == $tag and .name == $tag and .target_commitish == $commit and .draft == true and .prerelease == false' "$temporary/parent.json" >/dev/null || fail 'existing parent release identity differs'
  jq -e --arg asset "$asset" '.[0] | ([.assets[] | select(.name == $asset)] | length) as $count | $count == 0 or ($count == 1 and ([.assets[] | select(.name == $asset and .state == "uploaded")] | length == 1))' "$temporary/parent.json" >/dev/null || fail 'native asset is incomplete or ambiguous'
fi

mkdir -p "$output" "$temporary/previous"
"$scripts/resolve-native-network.sh" "$previous_tag" "$temporary/previous.json" --published-fixture
"$scripts/prepare-native-network.sh" "$temporary/previous.json" "$temporary/previous" --published-fixture
bundle_build() {
  python3 - "$1" <<'PY'
import plistlib,re,sys,zipfile
with zipfile.ZipFile(sys.argv[1]) as package:
    path = 'CQNetwork.app/Contents/Info.plist'
    if package.namelist().count(path) != 1:
        raise SystemExit('ambiguous native bundle metadata')
    value = plistlib.loads(package.read(path)).get('CFBundleVersion')
    if not isinstance(value,str) or not re.fullmatch(r'[1-9][0-9]*',value):
        raise SystemExit('native build number must be a positive integer')
    print(value)
PY
}
previous_build_number=$(bundle_build "$temporary/previous/CQNetwork_${previous_tag#v}_universal.zip")
python3 - "$build" "$previous_build_number" <<'PY'
import sys
if int(sys.argv[1]) <= int(sys.argv[2]):
    raise SystemExit('native build number must increase beyond the latest published companion')
PY
skip_build=false
manifest="$output/native-network-release.json"
if [[ "$existing" == true && "$(jq --arg asset "$asset" '[.[0].assets[] | select(.name == $asset)] | length' "$temporary/parent.json")" == 1 ]]; then
  "$scripts/resolve-native-network.sh" "$tag" "$manifest"
  "$scripts/prepare-native-network.sh" "$manifest" "$output" --allow-draft
  [[ "$(bundle_build "$output/$asset")" == "$build" ]] || fail 'existing immutable native asset has a different build number'
  skip_build=true
elif [[ "$existing" == false ]]; then
  {
    printf '## Changelog\n\n'
    git log '--format=- %s' "$previous_tag..$source_commit"
  } > "$temporary/notes.md"
  gh release create "$tag" --repo "$repository" --verify-tag --draft --title "$tag" --target "$source_commit" --notes-file "$temporary/notes.md"
fi
{
  printf 'existing=%s\nskip_build=%s\nprevious_build_number=%s\nCQ_PREVIOUS_BUILD_NUMBER=%s\nsource_commit=%s\nmanifest=%s\n' "$existing" "$skip_build" "$previous_build_number" "$previous_build_number" "$source_commit" "$manifest"
} > "$output/prepare-native-ci-release.env"
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  cat "$output/prepare-native-ci-release.env" >> "$GITHUB_OUTPUT"
fi
printf 'Verified native CI release %s, build %s (previous %s, reuse %s).\n' "$tag" "$build" "$previous_build_number" "$skip_build"
