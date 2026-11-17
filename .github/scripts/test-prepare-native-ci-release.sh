#!/usr/bin/env bash
set -euo pipefail
script="$(cd "$(dirname "$0")" && pwd)/prepare-native-ci-release.sh"
real_git=$(command -v git)
repository_root="$(cd "$(dirname "$0")/../.." && pwd)"
root=$(mktemp -d "${TMPDIR:-/tmp}/cq-native-ci-test.XXXXXX")
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/bin" "$root/scripts"
export FIXTURE="$root" HEAD_COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa MAIN_COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export VERIFIED=true ORIGIN=https://github.com/jacobcxdev/cq.git PREVIOUS_BUILD=100 EXISTING_BUILD=101
export PATH="$root/bin:$PATH" GITHUB_REPOSITORY=jacobcxdev/cq
cat > "$root/bin/git" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'remote get-url origin') echo "$ORIGIN" ;;
  fetch*) ;;
  'rev-parse --verify FETCH_HEAD^{commit}'|'rev-parse --verify refs/remotes/origin/main^{commit}') echo "$MAIN_COMMIT" ;;
  'rev-parse --verify HEAD^{commit}'|'rev-parse HEAD'|*'^{commit}') echo "$HEAD_COMMIT" ;;
  *':native/macos-network') echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
  'diff --quiet HEAD -- native/macos-network'|'ls-files --others --exclude-standard -- native/macos-network'|'merge-base --is-ancestor '*) ;;
  'log --format=- %s '*) printf '%s\n' '- fix: retained full published range' ;;
  *) echo "unexpected git: $*" >&2; exit 1 ;;
esac
FAKE
cat > "$root/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE/gh.log"
case "$*" in
  'api repos/jacobcxdev/cq/git/ref/tags/'*) printf '{"object":{"type":"%s","sha":"cccccccccccccccccccccccccccccccccccccccc"}}\n' "${TAG_TYPE:-tag}" ;;
  'api repos/jacobcxdev/cq/git/tags/'*)
    tag=$(tail -2 "$FIXTURE/gh.log" | head -1); tag=${tag##*/}
    printf '{"tag":"%s","verification":{"verified":%s},"object":{"type":"commit","sha":"%s"}}\n' "$tag" "${STAGE_VERIFIED:-$VERIFIED}" "${TAG_COMMIT:-$HEAD_COMMIT}" ;;
  'api --paginate --slurp repos/jacobcxdev/cq/releases?per_page=100') cat "$FIXTURE/releases.json" ;;
  'release create '*)
    printf '%s\n' "$*" > "$FIXTURE/create.log"
    while [[ $# -gt 0 ]]; do
      if [[ "$1" == --notes-file ]]; then
        cp "$2" "$FIXTURE/created-notes.md"
        break
      fi
      shift
    done ;;

  *) echo "unexpected gh: $*" >&2; exit 1 ;;
esac
FAKE
cat > "$root/scripts/resolve-native-network.sh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE/helpers.log"
printf '{"tag":"%s"}\n' "$1" > "$2"
FAKE
cat > "$root/scripts/prepare-native-network.sh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE/helpers.log"
tag=$(jq -r .tag "$1")
build=$EXISTING_BUILD
[[ "$tag" != v1.2.2 ]] || build=$PREVIOUS_BUILD
python3 - "$2/CQNetwork_${tag#v}_universal.zip" "$build" <<'PY'
import plistlib,sys,zipfile
with zipfile.ZipFile(sys.argv[1], 'w') as z:
    z.writestr('CQNetwork.app/Contents/Info.plist', plistlib.dumps({'CFBundleVersion':sys.argv[2]}))
PY
FAKE
chmod +x "$root/bin/"* "$root/scripts/"*
fixture() {
  jq -n --arg commit "$HEAD_COMMIT" '[ [{tag_name:"v1.2.2",name:"v1.2.2",draft:false,prerelease:false,target_commitish:$commit,assets:[]}] ]' > "$root/releases.json"
  rm -rf "$root/output" "$root/create.log" "$root/helpers.log"
  : > "$root/gh.log"
  export GITHUB_OUTPUT="$root/github-output"
  : > "$GITHUB_OUTPUT"
}
run() {
  cp "$script" "$root/scripts/prepare-native-ci-release.sh"
  bash "$root/scripts/prepare-native-ci-release.sh" v1.2.3 "${1:-101}" "$root/output"
}
reject() {
  : > "$GITHUB_OUTPUT"
  if run "${1:-101}" > "$root/failure.log" 2>&1; then
    echo 'invalid preflight accepted' >&2; exit 1
  fi
  [[ ! -e "$root/create.log" && ! -s "$GITHUB_OUTPUT" ]]
}
fixture
run
[[ -s "$root/create.log" ]]
[[ "$(head -1 "$root/created-notes.md")" == '## Changelog' ]]
grep -Fxq -- '- fix: retained full published range' "$root/created-notes.md"
grep -Fxq previous_build_number=100 "$GITHUB_OUTPUT"
grep -Fxq existing=false "$GITHUB_OUTPUT"
grep -Fxq skip_build=false "$GITHUB_OUTPUT"
[[ -s "$root/output/prepare-native-ci-release.env" ]]
fixture
ORIGIN=https://github.com/other/cq.git reject
[[ ! -s "$root/helpers.log" ]]
fixture
MAIN_COMMIT=dddddddddddddddddddddddddddddddddddddddd reject
[[ ! -s "$root/helpers.log" ]]
fixture
VERIFIED=false reject
[[ ! -s "$root/helpers.log" ]]
fixture
TAG_TYPE=commit reject
[[ ! -s "$root/helpers.log" ]]
fixture
TAG_COMMIT=dddddddddddddddddddddddddddddddddddddddd reject
[[ ! -s "$root/helpers.log" ]]
fixture
GITHUB_REPOSITORY=other/cq reject
[[ ! -s "$root/helpers.log" ]]
fixture
reject 100
fixture
jq '.[0] += [{tag_name:"v9.9.9",name:"v9.9.9",draft:true,prerelease:false,target_commitish:"other",assets:[]}]' "$root/releases.json" > "$root/temp.json"
mv "$root/temp.json" "$root/releases.json"
reject
fixture
jq --arg commit "$HEAD_COMMIT" '.[0] += [{tag_name:"v1.2.3",name:"v1.2.3",draft:true,prerelease:false,target_commitish:$commit,assets:[{name:"CQNetwork_1.2.3_universal.zip",state:"uploaded"}]}]' "$root/releases.json" > "$root/temp.json"
mv "$root/temp.json" "$root/releases.json"
run
[[ ! -e "$root/create.log" ]]
grep -Fxq existing=true "$GITHUB_OUTPUT"
grep -Fxq skip_build=true "$GITHUB_OUTPUT"
grep -q -- '--allow-draft' "$root/helpers.log"
EXISTING_BUILD=102 reject
fixture
jq --arg commit "$HEAD_COMMIT" '.[0] += [{tag_name:"v1.2.3",name:"v1.2.3",draft:true,prerelease:false,target_commitish:$commit,assets:[]}]' "$root/releases.json" > "$root/temp.json"
mv "$root/temp.json" "$root/releases.json"
run
[[ ! -e "$root/create.log" ]]
grep -Fxq existing=true "$GITHUB_OUTPUT"
grep -Fxq skip_build=false "$GITHUB_OUTPUT"
for change in '.name="wrong"' '.target_commitish="wrong"' '.prerelease=true' '.assets += [.assets[0]]' '.assets[0].state="starter"'; do
  fixture
  jq --arg commit "$HEAD_COMMIT" --arg change "$change" '.[0] += [{tag_name:"v1.2.3",name:"v1.2.3",draft:true,prerelease:false,target_commitish:$commit,assets:[{name:"CQNetwork_1.2.3_universal.zip",state:"uploaded"}]}]' "$root/releases.json" > "$root/temp.json"
  jq ".[0][1] |= ($change)" "$root/temp.json" > "$root/releases.json"
  reject
done
# Exercise the actual multi-ref Git fetch with stale tracking state and tags
# pointing at an older commit. Existing local objects keep this probe small.
"$real_git" -c gc.auto=0 -c maintenance.auto=false clone --quiet --bare --shared "$repository_root" "$root/origin.git"
current=$("$real_git" -C "$repository_root" rev-parse HEAD)
older=$("$real_git" -C "$repository_root" rev-parse 'HEAD^')
"$real_git" --git-dir="$root/origin.git" update-ref refs/heads/main "$current"
"$real_git" --git-dir="$root/origin.git" update-ref refs/tags/native-ci-fixture "$older"
"$real_git" --git-dir="$root/origin.git" update-ref refs/tags/native-ci-stage-fixture "$older"
"$real_git" init --quiet "$root/fetch-probe"
"$real_git" -C "$root/fetch-probe" config gc.auto 0
"$real_git" -C "$root/fetch-probe" config maintenance.auto false
"$real_git" -C "$root/fetch-probe" remote add origin "$root/origin.git"
"$real_git" -C "$root/fetch-probe" config --unset-all remote.origin.fetch
"$real_git" -C "$root/fetch-probe" fetch --quiet --no-auto-maintenance --no-tags origin "$older"
"$real_git" -C "$root/fetch-probe" update-ref refs/remotes/origin/main "$older"
"$real_git" -C "$root/fetch-probe" fetch --quiet --no-auto-maintenance --no-tags origin main refs/tags/native-ci-fixture:refs/tags/native-ci-fixture refs/tags/native-ci-stage-fixture:refs/tags/native-ci-stage-fixture
[[ "$("$real_git" -C "$root/fetch-probe" rev-parse --verify 'FETCH_HEAD^{commit}')" == "$current" ]]
[[ "$("$real_git" -C "$root/fetch-probe" rev-parse --verify 'refs/remotes/origin/main^{commit}')" == "$older" ]]
[[ "$("$real_git" -C "$root/fetch-probe" rev-parse --verify 'refs/tags/native-ci-fixture^{commit}')" == "$older" ]]
echo 'Native CI preparation checks passed.'
