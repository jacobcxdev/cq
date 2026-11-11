#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/publish-homebrew-casks.sh"
root=$(mktemp -d /tmp/cq-homebrew-publication-test.XXXXXX)
trap 'rm -rf "$root"' EXIT
mkdir "$root/bin" "$root/casks"
for token in cq-network cq; do printf 'cask "%s" do\n  version "0.48.0"\nend\n' "$token" >"$root/casks/$token.rb"; done
cat >"$root/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root=$CQ_CASK_TEST_ROOT
case "$1 $2" in
  'api repos/jacobcxdev/homebrew-tap') echo main ;;
  'api repos/jacobcxdev/homebrew-tap/git/ref/heads/main') echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  api\ repos/jacobcxdev/homebrew-tap/contents/Casks/*)
    token=${2##*/};token=${token%%.rb*}
    if [[ -f "$root/invalid-response" ]]; then echo '{}';
    elif [[ -f "$root/unchanged" || ( "$token" == cq && -f "$root/one-change" ) ]]; then
      jq -n --rawfile body "$root/casks/$token.rb" '{content:($body|@base64)}'
    else echo '{"content":"b2xkCg=="}'; fi ;;
  'api graphql')
    jq -e '.variables.input.branch | keys == ["branchName","repositoryNameWithOwner"]
      and .branchName == "main"' "$4" >/dev/null
    cp "$4" "$root/request.json"; echo commit >>"$root/writes"
    [[ ! -e "$root/race" ]] || exit 1
    echo '{"data":{"createCommitOnBranch":{"commit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"ref":{"target":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}}}}' ;;
  'api repos/jacobcxdev/homebrew-tap/commits/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb')
    jq --argjson verified "$([[ -e "$root/unsigned" ]] && echo false || echo true)" '{sha:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",parents:[{sha:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
      commit:{verification:{verified:$verified,reason:(if $verified then "valid" else "unsigned" end)}},
      files:(.variables.input.fileChanges.additions|map({filename:.path,status:"modified"}))}' "$root/request.json" ;;
  *) echo 'unexpected gh call' >&2; exit 2 ;;
esac
SH
chmod +x "$root/bin/gh"
export CQ_CASK_TEST_ROOT="$root" PATH="$root/bin:$PATH" GH_TOKEN=fixture RELEASE_TAG=v0.48.0
bash "$script" "$root/casks" >"$root/output"
[[ "$(cat "$root/writes")" == commit ]]
jq -e '.variables.input.branch.branchName == "main" and .variables.input.expectedHeadOid == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  and (.variables.input.fileChanges.additions|map(.path)|sort) == ["Casks/cq-network.rb","Casks/cq.rb"]' "$root/request.json" >/dev/null
rm "$root/writes"
touch "$root/unchanged"
bash "$script" "$root/casks" >"$root/output"
[[ ! -e "$root/writes" ]]
rm "$root/unchanged"
touch "$root/one-change"
bash "$script" "$root/casks" >"$root/output"
jq -e '.variables.input.fileChanges.additions|map(.path) == ["Casks/cq-network.rb"]' "$root/request.json" >/dev/null
rm "$root/one-change"
for failure in unsigned race; do
  touch "$root/$failure"
  if bash "$script" "$root/casks" >"$root/output" 2>"$root/error"; then exit 1; fi
  [[ ! -s "$root/output" ]]
  rm "$root/$failure"
done
rm -f "$root/writes"
touch "$root/invalid-response"
if bash "$script" "$root/casks" >"$root/output" 2>"$root/error"; then exit 1; fi
[[ ! -e "$root/writes" ]]
echo 'Homebrew publication regression checks passed.'
