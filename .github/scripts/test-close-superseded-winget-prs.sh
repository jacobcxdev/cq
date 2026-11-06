#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/close-superseded-winget-prs.sh"
root=$(mktemp -d "${TMPDIR:-/tmp}/cq-winget-prs.XXXXXX")
trap 'rm -rf "$root"' EXIT
mkdir "$root/bin"
cat >"$root/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'pr list')
    if [[ " $* " == *' --head '* ]]; then
      if [[ -f "$CQ_WINGET_TEST_ROOT/current.json" ]]; then
        cat "$CQ_WINGET_TEST_ROOT/current.json"
      else
        echo '[]'
      fi
    else
      cat "$CQ_WINGET_TEST_ROOT/prs.json"
    fi ;;
  'api repos/microsoft/winget-pkgs/contents/manifests/j/jacobcxdev/cq/0.42.5') test -f "$CQ_WINGET_TEST_ROOT/published" ;;
  'pr close')
    [[ "$3" =~ ^[0-9]+$ && "$4" == --repo && "$5" == microsoft/winget-pkgs && $# == 5 ]]
    echo "$3" >>"$CQ_WINGET_TEST_ROOT/closed" ;;
  *) exit 2 ;;
esac
SH
chmod +x "$root/bin/gh"
export CQ_WINGET_TEST_ROOT="$root" GH_TOKEN=fixture RELEASE_TAG=v0.42.5
export PATH="$root/bin:$PATH"
jq -n '
  def pr($n;$v): {number:$n,title:("New version: jacobcxdev.cq version "+$v),headRefName:("cq-"+$v),headRepositoryOwner:{login:"jacobcxdev"},author:{login:"jacobcxdev"}};
  [pr(1;"0.42.5"), pr(2;"0.42.4"), pr(3;"0.43.0"), pr(4;"0.41.10"),
   (pr(5;"0.42.3")|.title="New version: Other.Package version 0.42.3"),
   (pr(6;"0.42.3")|.headRepositoryOwner.login="other"),
   (pr(7;"0.42.3")|.author.login="other"),
   (pr(8;"0.42.3")|.headRefName="other-0.42.3"),
   (pr(9;"0.42.2")|.title="Update: jacobcxdev.cq to 0.42.2"),
   (pr(10;"0.42.2")|.title="Update: Other.Package to 0.42.2"),
   (pr(11;"0.42.2")|.title="Update: jacobcxdev.cq to 0.43.0"),
   (pr(12;"0.42.2")|.title="Update: jacobcxdev.cq to 0.42.2"|.headRepositoryOwner.login="other"),
   (pr(13;"0.42.2")|.title="Update: jacobcxdev.cq to 0.42.2"|.author.login="other"),
   (pr(14;"0.42.2")|.title="Update: jacobcxdev.cq to 0.42.2"|.headRefName="other-0.42.2")]
' >"$root/prs.json"
bash "$script"
[[ "$(cat "$root/closed")" == $'2\n4\n9' ]] || {
  echo 'expected both accepted title forms, preserving package and identity guards' >&2
  exit 1
}

jq 'map(if .number == 1 then .title = "Update: jacobcxdev.cq to 0.42.5" else . end)' "$root/prs.json" >"$root/updated.json"
mv "$root/updated.json" "$root/prs.json"
rm "$root/closed"
bash "$script"
[[ "$(cat "$root/closed")" == $'2\n4\n9' ]]

jq 'map(select(.number != 1))' "$root/prs.json" >"$root/older.json"
mv "$root/older.json" "$root/prs.json"
rm "$root/closed"
# A newly created PR can be absent from GitHub's author search index while
# the repository's exact-head lookup already returns it.
jq -n '[{number:1,title:"New version: jacobcxdev.cq version 0.42.5",headRefName:"cq-0.42.5",headRepositoryOwner:{login:"jacobcxdev"},author:{login:"jacobcxdev"}}]' >"$root/current.json"
bash "$script"
[[ "$(cat "$root/closed")" == $'2\n4\n9' ]]
rm "$root/closed"
for mutation in '.title="Other.Package"' '.author.login="other"' '.headRepositoryOwner.login="other"' '.headRefName="cq-0.42.4"'; do
  jq "map($mutation)" "$root/current.json" >"$root/invalid.json"
  mv "$root/current.json" "$root/valid.json"
  mv "$root/invalid.json" "$root/current.json"
  if bash "$script" >"$root/log" 2>&1; then exit 1; fi
  [[ ! -e "$root/closed" ]]
  mv "$root/valid.json" "$root/current.json"
done
rm "$root/current.json"
if bash "$script" >"$root/log" 2>&1; then exit 1; fi
[[ ! -e "$root/closed" ]]

touch "$root/published"
bash "$script"
[[ "$(cat "$root/closed")" == $'2\n4\n9' ]]
rm "$root/closed"
if RELEASE_TAG=v0.42.5-rc bash "$script" >"$root/log" 2>&1; then exit 1; fi
[[ ! -e "$root/closed" ]]
echo 'Superseded WinGet PR checks passed: versions, identity, package, replacement and invalid tag.'
