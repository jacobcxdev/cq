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
  'pr list') cat "$CQ_WINGET_TEST_ROOT/prs.json" ;;
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
   (pr(8;"0.42.3")|.headRefName="other-0.42.3")]
' >"$root/prs.json"
bash "$script"
[[ "$(cat "$root/closed")" == $'2\n4' ]]

jq 'map(select(.number != 1))' "$root/prs.json" >"$root/older.json"
mv "$root/older.json" "$root/prs.json"
rm "$root/closed"
if bash "$script" >"$root/log" 2>&1; then exit 1; fi
[[ ! -e "$root/closed" ]]

touch "$root/published"
bash "$script"
[[ "$(cat "$root/closed")" == $'2\n4' ]]
rm "$root/closed"
if RELEASE_TAG=v0.42.5-rc bash "$script" >"$root/log" 2>&1; then exit 1; fi
[[ ! -e "$root/closed" ]]
echo 'Superseded WinGet PR checks passed: versions, identity, package, replacement and invalid tag.'
