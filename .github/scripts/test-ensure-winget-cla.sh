#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/ensure-winget-cla.sh"
root=$(mktemp -d /tmp/cq-winget-cla-test.XXXXXX)
trap 'rm -rf "$root"' EXIT
mkdir "$root/bin"
cat >"$root/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'api user') cat "$CQ_WINGET_TEST_ROOT/actor" ;;
  'pr list') cat "$CQ_WINGET_TEST_ROOT/prs.json" ;;
  'pr view')
    if [[ -f "$CQ_WINGET_TEST_ROOT/viewed" && -f "$CQ_WINGET_TEST_ROOT/current-after.json" ]]; then
      cat "$CQ_WINGET_TEST_ROOT/current-after.json"
    else
      cat "$CQ_WINGET_TEST_ROOT/current.json"
    fi
    touch "$CQ_WINGET_TEST_ROOT/viewed" ;;
  'api repos/jacobcxdev/winget-pkgs/git/ref/heads/cq-0.47.3') cat "$CQ_WINGET_TEST_ROOT/fork-head" ;;
  'api --paginate') cat "$CQ_WINGET_TEST_ROOT/comments.json" ;;
  'api repos/microsoft/winget-pkgs/contents/manifests/j/jacobcxdev/cq/0.47.3') test -f "$CQ_WINGET_TEST_ROOT/published" ;;
  'pr comment')
    [[ $# == 7 && "$3" == 42 && "$4" == --repo && "$5" == microsoft/winget-pkgs && "$6" == --body-file ]]
    cat "$7" >"$CQ_WINGET_TEST_ROOT/posted"
    echo 42 >>"$CQ_WINGET_TEST_ROOT/writes" ;;
  *) exit 2 ;;
esac
SH
chmod +x "$root/bin/gh"
export CQ_WINGET_TEST_ROOT="$root" GH_TOKEN=fixture RELEASE_TAG=v0.47.3
export PATH="$root/bin:$PATH"
agreement='@microsoft-github-policy-service agree company="JacobCX Development Ltd."'
printf 'jacobcxdev\n' >"$root/actor"
printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' >"$root/fork-head"
jq -n '{number:42,state:"OPEN",title:"New version: jacobcxdev.cq version 0.47.3",
  url:"https://github.com/microsoft/winget-pkgs/pull/42",headRefName:"cq-0.47.3",
  headRefOid:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",headRepository:{name:"winget-pkgs"},
  headRepositoryOwner:{login:"jacobcxdev"},author:{login:"jacobcxdev"},
  files:[{path:"manifests/j/jacobcxdev/cq/0.47.3/jacobcxdev.cq.installer.yaml"},
    {path:"manifests/j/jacobcxdev/cq/0.47.3/jacobcxdev.cq.locale.en-US.yaml"},
    {path:"manifests/j/jacobcxdev/cq/0.47.3/jacobcxdev.cq.yaml"}]}' >"$root/current.json"
jq '[.]' "$root/current.json" >"$root/prs.json"
printf '[[]]\n' >"$root/comments.json"

# New and already-existing submissions use the same guarded, idempotent step.
bash "$script"
[[ "$(cat "$root/posted")" == "$agreement" ]]
jq -n --arg body "$agreement" '[[{user:{login:"jacobcxdev"},body:($body+"\n")}]]' >"$root/comments.json"
bash "$script"
[[ "$(cat "$root/writes")" == 42 ]]

# A copied agreement from somebody else and a quoted command do not count.
for body in "$agreement" "\`$agreement\`"; do
  author=other
  [[ "$body" != "$agreement" ]] && author=jacobcxdev
  jq -n --arg body "$body" --arg author "$author" '[[{user:{login:$author},body:$body}]]' >"$root/comments.json"
  rm -f "$root/writes"
  bash "$script"
  [[ "$(cat "$root/writes")" == 42 ]]
done

cp "$root/current.json" "$root/valid.json"
for mutation in '.author.login="other"' '.headRepositoryOwner.login="other"' \
  '.headRepository.name="other"' '.headRefOid="invalid"' \
  '.headRefName="cq-0.47.2"' '.title="Other.Package"' '.state="CLOSED"' \
  '.url="https://github.com/other/winget-pkgs/pull/42"' \
  '.files[0].path="manifests/j/jacobcxdev/cq/0.47.2/jacobcxdev.cq.installer.yaml"'; do
  jq "$mutation" "$root/valid.json" >"$root/current.json"
  rm -f "$root/writes"
  if bash "$script" >"$root/error" 2>&1; then exit 1; fi
  [[ ! -e "$root/writes" ]]
done
cp "$root/valid.json" "$root/current.json"
printf '[[]]\n' >"$root/comments.json"
jq '.headRefOid="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' "$root/valid.json" >"$root/current-after.json"
rm -f "$root/viewed"
if bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" ]]
rm "$root/current-after.json"
printf 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n' >"$root/fork-head"
if bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" ]]
printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' >"$root/fork-head"
for invalid in 'not JSON' '{}' '[[{"body":"agreement","user":null}]]'; do
  printf '%s\n' "$invalid" >"$root/comments.json"
  if bash "$script" >"$root/error" 2>&1; then exit 1; fi
  [[ ! -e "$root/writes" ]]
done
printf '[[]]\n' >"$root/comments.json"
printf 'other\n' >"$root/actor"
if bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" ]]
printf 'jacobcxdev\n' >"$root/actor"
jq '[.,.]' "$root/current.json" >"$root/prs.json"
if bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" ]]
printf '[]\n' >"$root/prs.json"
if bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" ]]
touch "$root/published"
bash "$script"
[[ ! -e "$root/writes" ]]
if RELEASE_TAG=v0.47.3-rc bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" ]]
echo 'WinGet CLA checks passed: author, repository, version, files, idempotency and invalid targets.'
