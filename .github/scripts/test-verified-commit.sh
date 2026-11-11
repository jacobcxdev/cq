#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/verified-commit.sh"
root=$(mktemp -d /tmp/cq-verified-commit-test.XXXXXX)
trap 'rm -rf "$root"' EXIT
mkdir "$root/bin"
cat >"$root/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'api graphql')
    [[ "$3" == --input && $# == 4 ]]
    cp "$4" "$CQ_COMMIT_TEST_ROOT/request.json"
    jq -e '.variables.input.branch | keys == ["branchName","repositoryNameWithOwner"]
      and .branchName == "cq-0.48.0"' "$4" >/dev/null
    echo mutation >>"$CQ_COMMIT_TEST_ROOT/writes"
    [[ ! -e "$CQ_COMMIT_TEST_ROOT/race" ]] || exit 1
    cat "$CQ_COMMIT_TEST_ROOT/response.json" ;;
  'api repos/jacobcxdev/winget-pkgs/commits/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb')
    cat "$CQ_COMMIT_TEST_ROOT/commit.json" ;;
  *) echo 'unexpected gh call' >&2; exit 2 ;;
esac
SH
chmod +x "$root/bin/gh"
export CQ_COMMIT_TEST_ROOT="$root" PATH="$root/bin:$PATH" GH_TOKEN=fixture
jq -n '{branch:{repositoryNameWithOwner:"jacobcxdev/winget-pkgs",branchName:"cq-0.48.0"},
  expectedHeadOid:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  message:{headline:"chore: updated package manifests",body:"- Recorded the published installer checksums"},
  fileChanges:{additions:[{path:"manifests/j/jacobcxdev/cq/0.48.0/package.yaml",contents:"Zml4dHVyZQo="}]}}' >"$root/input.json"
jq -n '{data:{createCommitOnBranch:{commit:{oid:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
  ref:{target:{oid:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}}}' >"$root/response.json"
jq -n '{sha:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",parents:[{sha:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
  commit:{verification:{verified:true,reason:"valid"}},files:[{filename:"manifests/j/jacobcxdev/cq/0.48.0/package.yaml",status:"added"}]}' >"$root/commit.json"

[[ "$(bash "$script" jacobcxdev/winget-pkgs "$root/input.json")" == bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ]]
jq -e --slurpfile input "$root/input.json" '.variables.input == $input[0]
  and (.query|contains("createCommitOnBranch"))' "$root/request.json" >/dev/null
cp "$root/commit.json" "$root/valid.json"
for mutation in '.commit.verification.verified=false' '.commit.verification.reason="unsigned"' \
  '.parents[0].sha="cccccccccccccccccccccccccccccccccccccccc"' \
  '.files[0].filename="unrelated.yaml"' '.files[0].status="removed"'; do
  jq "$mutation" "$root/valid.json" >"$root/commit.json"
  if bash "$script" jacobcxdev/winget-pkgs "$root/input.json" >"$root/output" 2>"$root/error"; then exit 1; fi
  [[ ! -s "$root/output" ]]
done
cp "$root/valid.json" "$root/commit.json"
touch "$root/race"
if bash "$script" jacobcxdev/winget-pkgs "$root/input.json" >"$root/output" 2>"$root/error"; then exit 1; fi
[[ ! -s "$root/output" ]]
rm "$root/race"
cp "$root/input.json" "$root/valid-input.json"
for mutation in '.branch.repositoryNameWithOwner="other/repository"' '.branch.branchName=""' \
  '.branch |= (.refName=.branchName | del(.branchName))' '.branch.refName="extra"' '.expectedHeadOid="invalid"' \
  '.fileChanges.additions[0].path="../outside"' '.fileChanges.additions += .fileChanges.additions' \
  '.fileChanges.deletions=[{path:"other"}]' '.message.body=""'; do
  jq "$mutation" "$root/valid-input.json" >"$root/input.json"
  rm -f "$root/writes"
  if bash "$script" jacobcxdev/winget-pkgs "$root/input.json" >"$root/output" 2>"$root/error"; then exit 1; fi
  [[ ! -e "$root/writes" && ! -s "$root/output" ]]
done
echo 'Verified commit regression checks passed.'
