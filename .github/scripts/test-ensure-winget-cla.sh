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
  'release view') cat "$CQ_WINGET_TEST_ROOT/release.json" ;;
  'pr list') cat "$CQ_WINGET_TEST_ROOT/prs.json" ;;
  'pr view')
    if [[ -f "$CQ_WINGET_TEST_ROOT/viewed" && -f "$CQ_WINGET_TEST_ROOT/current-after.json" ]]; then
      cat "$CQ_WINGET_TEST_ROOT/current-after.json"
    else
      cat "$CQ_WINGET_TEST_ROOT/current.json"
    fi
    touch "$CQ_WINGET_TEST_ROOT/viewed" ;;
  'api repos/jacobcxdev/winget-pkgs/git/ref/heads/cq-0.47.3') cat "$CQ_WINGET_TEST_ROOT/fork-head" ;;
  'api repos/microsoft/winget-pkgs/pulls/42')
    if [[ -f "$CQ_WINGET_TEST_ROOT/policy-read" && -f "$CQ_WINGET_TEST_ROOT/policy-after.json" ]]; then
      cat "$CQ_WINGET_TEST_ROOT/policy-after.json"
    else
      cat "$CQ_WINGET_TEST_ROOT/policy.json"
    fi
    touch "$CQ_WINGET_TEST_ROOT/policy-read" ;;
  'api --paginate')
    case "$4" in
      */comments) cat "$CQ_WINGET_TEST_ROOT/comments.json" ;;
      */events) cat "$CQ_WINGET_TEST_ROOT/events.json" ;;
      */check-runs\?*) cat "$CQ_WINGET_TEST_ROOT/checks.json" ;;
      *) exit 2 ;;
    esac ;;
  'api repos/microsoft/winget-pkgs/issues/comments/123')
    if [[ -f "$CQ_WINGET_TEST_ROOT/comment-current.json" ]]; then
      cat "$CQ_WINGET_TEST_ROOT/comment-current.json"
    else
      jq 'flatten | .[] | select(.id == 123)' "$CQ_WINGET_TEST_ROOT/comments.json"
    fi ;;
  'api --method')
    [[ $# == 6 && "$3" == PATCH && "$4" == repos/microsoft/winget-pkgs/issues/comments/123 && "$5" == --input ]]
    cat "$6" >"$CQ_WINGET_TEST_ROOT/edited"
    jq --slurpfile edit "$6" 'map(map(if .id == 123 then .body=$edit[0].body | .updated_at="2026-11-07T17:01:00Z" else . end))' \
      "$CQ_WINGET_TEST_ROOT/comments.json" >"$CQ_WINGET_TEST_ROOT/comments-next.json"
    mv "$CQ_WINGET_TEST_ROOT/comments-next.json" "$CQ_WINGET_TEST_ROOT/comments.json"
    jq 'flatten | .[] | select(.id == 123)' "$CQ_WINGET_TEST_ROOT/comments.json"
    echo edit >>"$CQ_WINGET_TEST_ROOT/writes" ;;
  'api repos/microsoft/winget-pkgs/contents/manifests/j/jacobcxdev/cq/0.47.3') test -f "$CQ_WINGET_TEST_ROOT/published" ;;
  'pr comment')
    [[ $# == 7 && "$3" == 42 && "$4" == --repo && "$5" == microsoft/winget-pkgs && "$6" == --body-file ]]
    cat "$7" >"$CQ_WINGET_TEST_ROOT/posted"
    echo 42 >>"$CQ_WINGET_TEST_ROOT/writes" ;;
  *) exit 2 ;;
esac
SH
cat >"$root/bin/sleep" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CQ_WINGET_TEST_ROOT/sleeps"
SH
chmod +x "$root/bin/gh" "$root/bin/sleep"
export CQ_WINGET_TEST_ROOT="$root" GH_TOKEN=fixture RELEASE_TAG=v0.47.3
export PATH="$root/bin:$PATH"
agreement='@microsoft-github-policy-service agree company="JacobCX Development Ltd."'
printf 'jacobcxdev\n' >"$root/actor"
printf '{"tagName":"v0.47.3","isDraft":false,"isPrerelease":false}\n' >"$root/release.json"
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
printf '[[]]\n' >"$root/events.json"
printf '[{"check_runs":[]}]\n' >"$root/checks.json"
printf '{"number":42,"state":"open","author":"jacobcxdev","author_association":"CONTRIBUTOR","labels":[]}\n' >"$root/policy.json"

# New and already-existing submissions use the same guarded, idempotent step.
bash "$script"
[[ "$(cat "$root/posted")" == "$agreement" ]]
jq -n --arg body "$agreement" '[[{id:123,user:{login:"jacobcxdev"},body:($body+"\n"),created_at:"2026-11-07T16:00:00Z",updated_at:"2026-11-07T16:00:00Z",issue_url:"https://api.github.com/repos/microsoft/winget-pkgs/issues/42"}]]' >"$root/comments.json"
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
# The upstream label can arrive after the author agreement. Replay the same
# comment through a genuine edit, once per label generation, without another post.
rm -f "$root/published" "$root/writes" "$root/policy-read" "$root/sleeps"
jq '[.]' "$root/current.json" >"$root/prs.json"
jq '.author_association="NONE" | .labels=["Needs-CLA"]' "$root/policy.json" >"$root/policy-next.json"
mv "$root/policy-next.json" "$root/policy.json"
printf '[[{"id":99,"event":"labeled","label":{"name":"Needs-CLA"},"created_at":"2026-11-07T17:00:00Z"}]]\n' >"$root/events.json"
jq -n --arg body "$agreement" '[[{id:123,user:{login:"jacobcxdev"},body:($body+"\n"),created_at:"2026-11-07T16:00:00Z",updated_at:"2026-11-07T16:00:00Z",issue_url:"https://api.github.com/repos/microsoft/winget-pkgs/issues/42"}]]' >"$root/comments.json"
bash "$script"
[[ "$(cat "$root/writes")" == edit ]]
jq -e --arg body "$agreement" '.body == ($body+"\n\n")' "$root/edited" >/dev/null
bash "$script"
[[ "$(cat "$root/writes")" == edit ]]

# A new label generation permits one more edit, toggling the trailing blank line.
jq '.[0][0].created_at="2026-11-07T17:02:00Z"' "$root/events.json" >"$root/events-next.json"
mv "$root/events-next.json" "$root/events.json"
# The fake server must advance updated_at beyond the new label for this edit.
sed -i.bak 's/2026-11-07T17:01:00Z/2026-11-07T17:03:00Z/g' "$root/bin/gh"
bash "$script"
[[ "$(wc -l <"$root/writes" | tr -d ' ')" == 2 ]]
jq -e --arg body "$agreement" '.body == ($body+"\n")' "$root/edited" >/dev/null
bash "$script"
[[ "$(wc -l <"$root/writes" | tr -d ' ')" == 2 ]]

# Once the opening label has been cleared, a first-time author rerun is a no-op.
rm -f "$root/sleeps"
jq '.labels=[]' "$root/policy.json" >"$root/policy-next.json"
mv "$root/policy-next.json" "$root/policy.json"
jq '.[0] += [{id:100,event:"unlabeled",label:{name:"Needs-CLA"},created_at:"2026-11-07T17:04:00Z"}]' "$root/events.json" >"$root/events-next.json"
mv "$root/events-next.json" "$root/events.json"
bash "$script"
[[ "$(wc -l <"$root/writes" | tr -d ' ')" == 2 && ! -e "$root/sleeps" ]]
# Reset the event fixture to represent a pending first opening, not a cleared label.
printf '[[]]\n' >"$root/events.json"

# Wait for initial label processing for every association used by the upstream rule.
for association in NONE FIRST_TIMER FIRST_TIME_CONTRIBUTOR; do
  rm -f "$root/writes" "$root/policy-read" "$root/sleeps"
  printf '[[]]\n' >"$root/comments.json"
  jq --arg association "$association" '.author_association=$association | .labels=[]' "$root/policy.json" >"$root/policy-next.json"
  mv "$root/policy-next.json" "$root/policy.json"
  jq '.labels=["Needs-CLA"]' "$root/policy.json" >"$root/policy-after.json"
  bash "$script"
  [[ "$(cat "$root/writes")" == 42 && -s "$root/sleeps" ]]
done
rm "$root/policy-after.json"
rm -f "$root/writes" "$root/policy-read" "$root/sleeps"
if bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" && "$(wc -l <"$root/sleeps" | tr -d ' ')" == 23 ]]
grep -F 'timed out waiting for initial Needs-CLA' "$root/error" >/dev/null

# Malformed association, event and comment metadata cannot authorise an edit.
jq '.labels=["Needs-CLA"]' "$root/policy.json" >"$root/policy-next.json"
mv "$root/policy-next.json" "$root/policy.json"
jq -n --arg body "$agreement" '[[{id:123,user:{login:"jacobcxdev"},body:$body,created_at:"2026-11-07T16:00:00Z",updated_at:"2026-11-07T16:00:00Z",issue_url:"https://api.github.com/repos/microsoft/winget-pkgs/issues/42"}]]' >"$root/comments.json"
printf '[[{"id":99,"event":"labeled","label":{"name":"Needs-CLA"},"created_at":"2026-11-07T17:00:00Z"}]]\n' >"$root/events.json"
cp "$root/events.json" "$root/events-valid.json"
for invalid in 'not JSON' '{}' '[[]]' '[[{"id":99,"event":"labeled","label":{"name":"Needs-CLA"},"created_at":"invalid"}]]'; do
  printf '%s\n' "$invalid" >"$root/events.json"
  if bash "$script" >"$root/error" 2>&1; then exit 1; fi
  [[ ! -e "$root/writes" ]]
done
cp "$root/events-valid.json" "$root/events.json"
cp "$root/comments.json" "$root/comments-valid.json"
for mutation in '.[0][0].id=null' '.[0][0].updated_at="invalid"' '.[0][0].issue_url="https://api.github.com/repos/other/winget-pkgs/issues/42"'; do
  jq "$mutation" "$root/comments-valid.json" >"$root/comments.json"
  if bash "$script" >"$root/error" 2>&1; then exit 1; fi
  [[ ! -e "$root/writes" ]]
done
cp "$root/comments-valid.json" "$root/comments.json"
jq '.[0][0] | .body="changed"' "$root/comments.json" >"$root/comment-current.json"
if bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" ]]
rm "$root/comment-current.json"
jq '.author_association=null' "$root/policy.json" >"$root/policy-next.json"
mv "$root/policy-next.json" "$root/policy.json"
if bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" ]]
# A trusted green CLA check completes initial policy processing without a label.
jq -n '[{check_runs:[{id:1,name:"license/cla",
  head_sha:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",status:"completed",conclusion:"success",
  app:{id:95686,slug:"microsoft-github-policy-service"}}]}]' >"$root/checks-valid.json"
cp "$root/checks-valid.json" "$root/checks.json"
printf '[[]]\n' >"$root/comments.json"
printf '[[]]\n' >"$root/events.json"
for association in NONE FIRST_TIMER FIRST_TIME_CONTRIBUTOR; do
  rm -f "$root/writes" "$root/policy-read" "$root/sleeps"
  jq --arg association "$association" '.author_association=$association | .labels=[]' "$root/policy.json" >"$root/policy-next.json"
  mv "$root/policy-next.json" "$root/policy.json"
  bash "$script"
  [[ "$(cat "$root/writes")" == 42 && ! -e "$root/sleeps" ]]
done

# Green checks do not weaken the current-head guard or exact comment dedupe.
rm -f "$root/writes" "$root/viewed"
jq '.headRefOid="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' "$root/valid.json" >"$root/current-after.json"
if bash "$script" >"$root/error" 2>&1; then exit 1; fi
[[ ! -e "$root/writes" ]]
rm "$root/current-after.json"
jq -n --arg body "$agreement" '[[{id:123,user:{login:"jacobcxdev"},body:($body+"\n"),created_at:"2026-11-07T16:00:00Z",updated_at:"2026-11-07T16:00:00Z",issue_url:"https://api.github.com/repos/microsoft/winget-pkgs/issues/42"}]]' >"$root/comments.json"
bash "$script"
[[ ! -e "$root/writes" ]]

# A green check cannot suppress an explicit new Needs-CLA label generation.
jq '.labels=["Needs-CLA"]' "$root/policy.json" >"$root/policy-next.json"
mv "$root/policy-next.json" "$root/policy.json"
printf '[[{"id":99,"event":"labeled","label":{"name":"Needs-CLA"},"created_at":"2026-11-07T17:00:00Z"}]]\n' >"$root/events.json"
bash "$script"
[[ "$(cat "$root/writes")" == edit ]]

# Pending, failed, spoofed, stale and malformed checks cannot authorise progress.
jq '.labels=[]' "$root/policy.json" >"$root/policy-next.json"
mv "$root/policy-next.json" "$root/policy.json"
printf '[[]]\n' >"$root/comments.json"
printf '[[]]\n' >"$root/events.json"
for mutation in '.[0].check_runs[0].conclusion="failure"' \
  '.[0].check_runs[0].status="in_progress"' \
  '.[0].check_runs[0].app.id=1' \
  '.[0].check_runs[0].app.slug="other"' \
  '.[0].check_runs[0].name="other"' \
  '.[0].check_runs[0].head_sha="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' \
  '.[0].check_runs |= . + [(.[0] | .id=2 | .status="in_progress" | .conclusion=null)]' \
  '.[0].check_runs[0].id=null' \
  '.[0].check_runs=null'; do
  jq "$mutation" "$root/checks-valid.json" >"$root/checks.json"
  rm -f "$root/writes" "$root/sleeps"
  if bash "$script" >"$root/error" 2>&1; then exit 1; fi
  [[ ! -e "$root/writes" && "$(wc -l <"$root/sleeps" | tr -d ' ')" == 23 ]]
done
for mutation in '.isDraft=true' '.isPrerelease=true' '.tagName="v0.47.2"' 'del(.isDraft)'; do
  jq "$mutation" <<< '{"tagName":"v0.47.3","isDraft":false,"isPrerelease":false}' >"$root/release.json"
  rm -f "$root/writes"
  if bash "$script" >"$root/error" 2>&1; then exit 1; fi
  [[ ! -e "$root/writes" ]]
  grep -F 'release must be published and stable' "$root/error" >/dev/null
done
echo 'WinGet CLA checks passed: identity, head, exact dedupe, label ordering, trusted checks, replay and invalid metadata.'
