#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/publish-homebrew-casks.sh"
generator="$(dirname "$script")/generate-scoop-manifest.sh"
root=$(mktemp -d /tmp/cq-homebrew-publication-test.XXXXXX)
trap 'rm -rf "$root"' EXIT
mkdir "$root/bin" "$root/casks"
for token in cq-network cq; do printf 'cask "%s" do\n  version "0.48.0"\nend\n' "$token" >"$root/casks/$token.rb"; done
amd64=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
arm64=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
printf '%s  cq_0.48.0_windows_amd64.zip\n%s  cq_0.48.0_windows_arm64.zip\n' "$amd64" "$arm64" >"$root/checksums.txt"
checksum=$(openssl dgst -sha256 -r "$root/checksums.txt" | cut -d ' ' -f 1)
jq -n --arg amd64 "$amd64" --arg arm64 "$arm64" --arg checksum "$checksum" '
  {id:123, url:"https://api.github.com/repos/jacobcxdev/cq/releases/123",
    assets_url:"https://api.github.com/repos/jacobcxdev/cq/releases/123/assets",
    html_url:"https://github.com/jacobcxdev/cq/releases/tag/v0.48.0",
    tag_name:"v0.48.0", draft:false, assets:([
    {name:"cq_0.48.0_windows_amd64.zip",hash:$amd64},
    {name:"cq_0.48.0_windows_arm64.zip",hash:$arm64},
    {name:"checksums.txt",hash:$checksum}] | map({
      name, state:"uploaded", digest:("sha256:" + .hash),
      browser_download_url:("https://github.com/jacobcxdev/cq/releases/download/v0.48.0/" + .name)
    }))}' >"$root/release.json"
bash "$generator" v0.48.0 "$root/release.json" "$root/checksums.txt" >"$root/cq.json"
jq -e --arg amd64 "$amd64" --arg arm64 "$arm64" '
  .version == "0.48.0" and .bin == "cq.exe" and (.architecture | keys) == ["64bit","arm64"]
  and .architecture["64bit"].hash == $amd64 and .architecture.arm64.hash == $arm64
  and (.architecture["64bit"].url | endswith("_amd64.zip")) and (.architecture.arm64.url | endswith("_arm64.zip"))
' "$root/cq.json" >/dev/null
draft_slug=untagged-cc22319b1b64249aae99
jq --arg slug "$draft_slug" '
  .draft = true
  | .html_url |= sub("v0\\.48\\.0$"; $slug)
  | .assets[].browser_download_url |= sub("/v0\\.48\\.0/"; "/" + $slug + "/")
' "$root/release.json" >"$root/draft.json"
bash "$generator" v0.48.0 "$root/draft.json" "$root/checksums.txt" >"$root/draft-cq.json"
cmp "$root/cq.json" "$root/draft-cq.json"
for invalid in '.assets[1].digest="sha256:bad"' '.assets[1].digest=.assets[0].digest' \
  'del(.assets[1])' '.assets[1].browser_download_url=.assets[0].browser_download_url' \
  '.assets += [.assets[1]]' '.draft=false' '.id=124' '.assets_url += "/extra"' \
  '.html_url |= sub("github.com"; "example.com")' '.html_url += "?extra"' \
  '.html_url |= sub("untagged-.*"; "untagged-z")' \
  '.assets[0].browser_download_url |= sub("github.com"; "example.com")' \
  '.assets[0].browser_download_url |= sub("untagged-[a-f0-9]+"; "untagged-deadbeef")'; do
  jq "$invalid" "$root/draft.json" >"$root/invalid.json"
  if bash "$generator" v0.48.0 "$root/invalid.json" "$root/checksums.txt" >"$root/output" 2>"$root/error"; then exit 1; fi
  [[ ! -s "$root/output" ]]
done
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
  api\ repos/jacobcxdev/homebrew-tap/contents/bucket/cq.json*)
    if [[ -f "$root/unchanged" || -f "$root/one-change" ]]; then
      jq -n --rawfile body "$root/cq.json" '{content:($body|@base64)}'
    else echo '{"status":"404"}'; exit 1; fi ;;
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
bash "$script" "$root/casks" "$root/cq.json" >"$root/output"
[[ "$(cat "$root/writes")" == commit ]]
jq -e '.variables.input.branch.branchName == "main" and .variables.input.expectedHeadOid == "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  and (.variables.input.fileChanges.additions|map(.path)|sort) == ["Casks/cq-network.rb","Casks/cq.rb","bucket/cq.json"]' "$root/request.json" >/dev/null
rm "$root/writes"
touch "$root/unchanged"
bash "$script" "$root/casks" "$root/cq.json" >"$root/output"
[[ ! -e "$root/writes" ]]
rm "$root/unchanged"
touch "$root/one-change"
bash "$script" "$root/casks" "$root/cq.json" >"$root/output"
jq -e '.variables.input.fileChanges.additions|map(.path) == ["Casks/cq-network.rb"]' "$root/request.json" >/dev/null
rm "$root/one-change"
for failure in unsigned race; do
  touch "$root/$failure"
  if bash "$script" "$root/casks" "$root/cq.json" >"$root/output" 2>"$root/error"; then exit 1; fi
  [[ ! -s "$root/output" ]]
  rm "$root/$failure"
done
rm -f "$root/writes"
touch "$root/invalid-response"
if bash "$script" "$root/casks" "$root/cq.json" >"$root/output" 2>"$root/error"; then exit 1; fi
[[ ! -e "$root/writes" ]]
echo 'Homebrew publication regression checks passed.'
