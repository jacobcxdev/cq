#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/submit-winget-manifest.sh"
root=$(mktemp -d /tmp/cq-winget-submit-test.XXXXXX)
trap 'rm -rf "$root"' EXIT
mkdir "$root/bin" "$root/manifests"
for name in jacobcxdev.cq.installer.yaml jacobcxdev.cq.locale.en-US.yaml jacobcxdev.cq.yaml; do
  printf 'fixture: %s\n' "$name" >"$root/manifests/$name"
done
cat >"$root/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root=$CQ_SUBMIT_TEST_ROOT
case "$1 $2" in
  'api user') cat "$root/actor" ;;
  'api repos/microsoft/winget-pkgs') printf '{"full_name":"microsoft/winget-pkgs","permissions":{"push":%s}}\n' "$(cat "$root/push")" ;;
  'release view') echo '{"tagName":"v0.48.0","isDraft":false,"isPrerelease":false}' ;;
  'api repos/microsoft/winget-pkgs/contents/manifests/j/jacobcxdev/cq/0.48.0') echo '{"status":"404"}'; exit 1 ;;
  'pr list')
    if [[ -f "$root/existing" ]]; then jq '[.]' "$root/pr.json"; else echo '[]'; fi ;;
  'api repos/jacobcxdev/winget-pkgs')
    if [[ -f "$root/missing-fork" && ! -f "$root/fork-created" ]]; then echo '{"status":"404"}'; exit 1;
    elif [[ -f "$root/fork-read-error" ]]; then jq -n --arg status "$(cat "$root/fork-read-error")" '{status:$status}'; exit 1;
    else cat "$root/fork.json"; fi ;;
  'repo fork')
    [[ "$3" == microsoft/winget-pkgs && "$4" == --clone=false && $# == 4 ]]
    echo fork >>"$root/writes"; touch "$root/fork-created" ;;
  'api repos/jacobcxdev/winget-pkgs/git/ref/heads/master') echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  'api repos/jacobcxdev/winget-pkgs/git/ref/heads/cq-0.48.0')
    if [[ -f "$root/existing" || -f "$root/committed" ]]; then
      if [[ "$*" == *--jq* ]]; then echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb;
      else echo '{"object":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}'; fi
    elif [[ -f "$root/branched" ]]; then echo '{"object":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}';
    else echo '{"status":"404"}'; exit 1; fi ;;
  'api --method')
    [[ "$3" == POST && "$4" == repos/jacobcxdev/winget-pkgs/git/refs ]]
    echo branch >>"$root/writes"; touch "$root/branched"; echo '{}' ;;
  'api graphql')
    cp "$4" "$root/request.json"
    jq -e '.variables.input.branch | keys == ["branchName","repositoryNameWithOwner"]
      and .branchName == "cq-0.48.0"' "$4" >/dev/null
    echo commit >>"$root/writes"
    [[ ! -e "$root/race" ]] || exit 1
    touch "$root/committed"
    echo '{"data":{"createCommitOnBranch":{"commit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"ref":{"target":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}}}' ;;
  'api repos/jacobcxdev/winget-pkgs/commits/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb') cat "$root/commit.json" ;;
  'api repos/microsoft/winget-pkgs/compare/master...jacobcxdev:cq-0.48.0')
    if [[ -f "$root/unexpected-diff" ]]; then echo '{"files":[{"filename":"unrelated","status":"added"}]}';
    else jq '{files:.files}' "$root/commit.json"; fi ;;
  'pr create')
    [[ "$*" == *'--base master --head jacobcxdev:cq-0.48.0'* ]]
    for argument in "$@"; do [[ "$argument" != --assignee && "$argument" != --assignee=* ]]; done
    echo pr >>"$root/writes"; echo 'https://github.com/microsoft/winget-pkgs/pull/42' ;;
  'pr view') cat "$root/pr.json" ;;
  api\ repos/jacobcxdev/winget-pkgs/contents/manifests/j/jacobcxdev/cq/0.48.0/*)
    name=${2##*/};name=${name%%\?*}
    jq -n --rawfile contents "$root/manifests/$name" '{content:($contents|@base64)}' ;;
  *) echo "unexpected gh call: $1 $2" >&2; exit 2 ;;
esac
SH
chmod +x "$root/bin/gh"
export CQ_SUBMIT_TEST_ROOT="$root" PATH="$root/bin:$PATH" GH_TOKEN=fixture RELEASE_TAG=v0.48.0
echo false >"$root/push"
echo jacobcxdev >"$root/actor"
printf '{"full_name":"jacobcxdev/winget-pkgs","fork":true,"parent":{"full_name":"microsoft/winget-pkgs"},"owner":{"login":"jacobcxdev"},"permissions":{"push":true}}\n' >"$root/fork.json"
jq -n '{sha:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",parents:[{sha:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
  commit:{verification:{verified:true,reason:"valid"}},files:([
    "jacobcxdev.cq.installer.yaml","jacobcxdev.cq.locale.en-US.yaml","jacobcxdev.cq.yaml"]|
    map({filename:("manifests/j/jacobcxdev/cq/0.48.0/"+.),status:"added"}))}' >"$root/commit.json"
jq -n '{number:42,state:"OPEN",baseRefName:"master",title:"New version: jacobcxdev.cq version 0.48.0",
  url:"https://github.com/microsoft/winget-pkgs/pull/42",headRefName:"cq-0.48.0",headRefOid:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  author:{login:"jacobcxdev"},headRepositoryOwner:{login:"jacobcxdev"},headRepository:{name:"winget-pkgs"},
  assignees:[],files:(["jacobcxdev.cq.installer.yaml","jacobcxdev.cq.locale.en-US.yaml","jacobcxdev.cq.yaml"]|
    map({path:("manifests/j/jacobcxdev/cq/0.48.0/"+.)}))}' >"$root/pr.json"

# An ordinary contributor can submit an unassigned public-fork PR.
if ! bash "$script" "$root/manifests" >"$root/output" 2>"$root/error"; then
  cat "$root/output" "$root/error" >&2
  echo 'ordinary contributor submission was rejected' >&2
  exit 1
fi
[[ "$(cat "$root/writes")" == $'branch\ncommit\npr' ]]
rm -f "$root/writes" "$root/branched" "$root/committed"

# Wrong credentials or an unowned, unrelated or non-writable fork must stop
# before any branch, commit or PR write.
echo other >"$root/actor"
if bash "$script" "$root/manifests" >"$root/output" 2>"$root/error"; then exit 1; fi
[[ ! -e "$root/writes" ]]
echo jacobcxdev >"$root/actor"
cp "$root/fork.json" "$root/valid-fork.json"
for change in '.full_name="other/winget-pkgs"' '.owner.login="other"' '.parent.full_name="other/winget-pkgs"' '.fork=false' '.permissions.push=false'; do
  jq "$change" "$root/valid-fork.json" >"$root/fork.json"
  if bash "$script" "$root/manifests" >"$root/output" 2>"$root/error"; then exit 1; fi
  [[ ! -e "$root/writes" ]]
done
cp "$root/valid-fork.json" "$root/fork.json"

# An unsigned server response must never become a community PR.
cp "$root/commit.json" "$root/valid-commit.json"
jq '.commit.verification={verified:false,reason:"unsigned"}' "$root/commit.json" >"$root/unsigned.json"
cp "$root/unsigned.json" "$root/commit.json"
if bash "$script" "$root/manifests" >"$root/output" 2>"$root/error"; then exit 1; fi
[[ "$(cat "$root/writes")" == $'branch\ncommit' ]]
cp "$root/valid-commit.json" "$root/commit.json"
rm -f "$root/writes" "$root/branched" "$root/committed"
touch "$root/race"
if bash "$script" "$root/manifests" >"$root/output" 2>"$root/error"; then exit 1; fi
[[ "$(cat "$root/writes")" == $'branch\ncommit' ]]
rm -f "$root/race" "$root/writes" "$root/branched"
touch "$root/unexpected-diff"
if bash "$script" "$root/manifests" >"$root/output" 2>"$root/error"; then exit 1; fi
[[ "$(cat "$root/writes")" == $'branch\ncommit' ]]
rm -f "$root/unexpected-diff" "$root/writes" "$root/branched" "$root/committed"
bash "$script" "$root/manifests" >"$root/output"
[[ "$(cat "$root/writes")" == $'branch\ncommit\npr' ]]
jq -e '.variables.input.fileChanges.additions|length == 3' "$root/request.json" >/dev/null

# Preserve first-time fork setup, after permission checks, and fail closed on
# lookup errors which do not establish that the fork is absent.
rm -f "$root/writes" "$root/branched" "$root/committed"
for status in 403 503; do
  echo "$status" >"$root/fork-read-error"
  if bash "$script" "$root/manifests" >"$root/output" 2>"$root/error"; then exit 1; fi
  [[ ! -e "$root/writes" ]]
done
rm "$root/fork-read-error"
touch "$root/missing-fork"
# A newly created fork must be writable before branch or commit creation.
jq '.permissions.push=false' "$root/valid-fork.json" >"$root/fork.json"
if bash "$script" "$root/manifests" >"$root/output" 2>"$root/error"; then exit 1; fi
[[ "$(cat "$root/writes")" == fork ]]
rm "$root/writes" "$root/fork-created"
cp "$root/valid-fork.json" "$root/fork.json"
bash "$script" "$root/manifests" >"$root/output"
[[ "$(cat "$root/writes")" == $'fork\nbranch\ncommit\npr' ]]
rm "$root/missing-fork"

# A crash after the signed commit can resume without another commit or ref write.
rm "$root/writes"
bash "$script" "$root/manifests" >"$root/output"
[[ "$(cat "$root/writes")" == pr ]]

# Reusing the exact unassigned signed submission performs no writes.
touch "$root/existing"
rm "$root/writes"
bash "$script" "$root/manifests" >"$root/output"
[[ ! -e "$root/writes" ]]

# An owned PR retargeted away from upstream master is not a reusable
# submission. Refuse it without creating a replacement or touching its branch.
cp "$root/pr.json" "$root/valid-pr.json"
jq '.baseRefName="not-master"' "$root/valid-pr.json" >"$root/pr.json"
if bash "$script" "$root/manifests" >"$root/output" 2>"$root/error"; then exit 1; fi
[[ ! -e "$root/writes" ]]
grep -q 'submission identity or files could not be verified' "$root/error"
cp "$root/valid-pr.json" "$root/pr.json"
echo 'WinGet submission regression checks passed.'
