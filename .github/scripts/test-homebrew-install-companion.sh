#!/usr/bin/env bash
set -euo pipefail

root=$(mktemp -d "${TMPDIR:-/tmp}/cq-homebrew-companion.XXXXXX")
trap 'rm -rf "$root"' EXIT
export CQ_COMPANION_TEST_ROOT="$root"
mkdir -p "$root/bin" "$root/tap/Casks" "$root/work"
go build -o "$root/nativepackage" ./internal/tools/nativepackage
ruby - .github/scripts/validate-homebrew-install.sh "$root/function.sh" <<'RUBY'
source, destination = ARGV
text = File.read(source)
function = text.match(/^with_previous_companion\(\) \(\n.*?^\)$/m)
abort "missing predecessor companion lifecycle setup" unless function
text = function[0]
%w[/usr/bin/ditto /usr/bin/codesign /usr/bin/xcrun /usr/sbin/spctl].each do |command|
  text.gsub!(command, File.join(ENV.fetch("CQ_COMPANION_TEST_ROOT"), "bin", File.basename(command)))
end
File.write(destination, text)
RUBY
python3 - "$root" <<'PY'
import hashlib, json, pathlib, sys, zipfile
root = pathlib.Path(sys.argv[1])
for version, asset in [('0.41.8', 18), ('0.41.9', 19)]:
    archive = root / f'{version}.zip'
    with zipfile.ZipFile(archive, 'w') as z:
        for path in ['Info.plist', 'MacOS/CQNetwork', 'Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/Info.plist', 'Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/MacOS/CQNetworkProxy']:
            z.writestr('CQNetwork.app/Contents/' + path, version)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    pin = dict(schema_version=1, version=version, source_commit='a'*40, asset_id=asset, sha256=digest, minimum_macos='13.0')
    (root / f'{version}.json').write_text(json.dumps(pin))
    # Published Casks have Homebrew's unquoted heredoc; templates retain quotes.
    delimiter = 'JSON' if version == '0.41.8' else "'JSON'"
    (root / f'{version}.rb').write_text(f"pin = JSON.parse(<<~{delimiter})\n" + json.dumps(pin) + '\nJSON\n')
    url = f'https://github.com/jacobcxdev/cq/releases/download/cq-network-v{version}/CQNetwork_{version}_universal.zip'
    release = dict(tag_name=f'cq-network-v{version}', target_commitish='a'*40, draft=False, prerelease=True, assets=[dict(id=asset, name=f'CQNetwork_{version}_universal.zip', digest='sha256:'+digest, state='uploaded', browser_download_url=url)])
    (root / f'release-{asset}.json').write_text(json.dumps(release))
PY
"$root/nativepackage" cask "$root/0.41.9.json" "$root/current-native.rb"
cp "$root/current-native.rb" "$root/tap/Casks/cq-network.rb"
cat >"$root/bin/brew" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root=$CQ_COMPANION_TEST_ROOT
selected="$root/tap/Casks/cq-network.rb"
case "$*" in
  '--repository jacobcxdev/tap') echo "$root/tap" ;;
  '--cache --cask jacobcxdev/tap/cq-network') grep -q 'version "0.41.9"' "$selected"; echo "$root/cache/candidate.zip" ;;
  'uninstall --cask --force jacobcxdev/tap/cq-network') echo uninstall >>"$root/events"; rm -f "$root/installed" ;;
  'install --cask jacobcxdev/tap/cq-network')
    grep -q 'version "0.41.8"' "$selected"
    [[ ! -f "$root/reject-install" ]] || exit 1
    echo install-previous >>"$root/events"; echo 0.41.8 >"$root/installed" ;;
  *) echo "unexpected brew call: $*" >&2; exit 1 ;;
esac
SH
cat >"$root/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root=$CQ_COMPANION_TEST_ROOT
case "$*" in
  'release view cq-network-v0.41.8 --repo jacobcxdev/cq --json apiUrl --jq .apiUrl') echo "${CQ_COMPANION_TEST_RELEASE_API:-https://api.github.com/repos/jacobcxdev/cq/releases/18}" ;;
  'api repos/jacobcxdev/cq/releases/18'|'api repos/other/cq/releases/18') cat "$root/release-18.json" ;;
  'api repos/jacobcxdev/cq/releases/assets/18 --header Accept: application/octet-stream') cat "$root/0.41.8.zip" ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
SH
for command in ditto codesign spctl; do
  cat >"$root/bin/$command" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$(basename "$0")" != codesign || ! -f "$CQ_COMPANION_TEST_ROOT/reject-signature" ]]
SH
done
cat >"$root/bin/xcrun" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
root=$CQ_COMPANION_TEST_ROOT
count=0
[[ ! -f "$root/ticket-count" ]] || count=$(cat "$root/ticket-count")
count=$((count + 1))
echo "$count" >"$root/ticket-count"
case "${CQ_COMPANION_TEST_TICKET:-ok}" in
  transient)
    if [[ "$count" -le 2 ]]; then echo 'NSURLErrorDomain Code=-1005' >&2; exit 68; fi ;;
  persistent) echo 'NSURLErrorDomain Code=-1005' >&2; exit 68 ;;
  unrelated) echo 'unrelated ticket lookup failure' >&2; exit 68 ;;
  invalid) echo 'invalid stapled ticket' >&2; exit 65 ;;
esac
SH
chmod +x "$root/bin/"*
export PATH="$root/bin:$PATH"
export CI=true
temporary_root="$root/work"
nativepackage_executable="$root/nativepackage"
previous_cask="$root/0.41.8.rb"
current_cask="$root/0.41.9.rb"
current_native_cask="$root/current-native.rb"
current_native_archive="$root/0.41.9.zip"
install_previous_cq() {
  [[ "$(cat "$root/installed")" == 0.41.8 ]]
  grep -q 'version "0.41.8"' "$root/tap/Casks/cq-network.rb"
  [[ ! -f "$root/reject-cq" ]] || return 1
  echo previous-cq >>"$root/events"
}
install_unchanged_cq() {
  [[ "$(cat "$root/installed")" == 0.41.9 ]]
  cmp "$root/current-native.rb" "$root/tap/Casks/cq-network.rb"
}
reset_case() {
  cp "$root/current-native.rb" "$root/tap/Casks/cq-network.rb"
  echo 0.41.9 >"$root/installed"
  rm -f "$root/events" "$root/reject-install" "$root/reject-cq" "$root/reject-signature" "$root/ticket-count"
}
assert_restored() { cmp "$root/current-native.rb" "$root/tap/Casks/cq-network.rb"; }
export root temporary_root nativepackage_executable previous_cask current_cask current_native_cask current_native_archive
export -f install_previous_cq install_unchanged_cq
# Use the shell under test; expansion belongs to that child.
# shellcheck disable=SC2016
run_setup() { "$BASH" -e -c 'source "$CQ_COMPANION_TEST_ROOT/function.sh"; with_previous_companion "$1"' fixture "${1:-install_previous_cq}"; }
reset_case
run_setup
assert_restored
cmp "$root/0.41.9.zip" "$root/cache/candidate.zip"
[[ "$(cat "$root/events")" == $'uninstall\ninstall-previous\nprevious-cq' ]]
reset_case
previous_cask=$current_cask
run_setup install_unchanged_cq
assert_restored
[[ ! -f "$root/events" ]]
previous_cask="$root/0.41.8.rb"
reset_case
CQ_COMPANION_TEST_TICKET=transient run_setup >"$root/error" 2>&1
assert_restored
[[ "$(cat "$root/ticket-count")" == 3 ]]
for failure in persistent unrelated invalid; do
  reset_case
  if CQ_COMPANION_TEST_TICKET=$failure run_setup >"$root/error" 2>&1; then
    echo "accepted failed ticket validation: $failure" >&2; exit 1
  fi
  assert_restored
  expected=1
  if [[ "$failure" == persistent ]]; then expected=3; fi
  [[ "$(cat "$root/ticket-count")" == "$expected" ]]
  [[ ! -f "$root/events" ]]
done
for failure in install cq signature; do
  reset_case
  touch "$root/reject-$failure"
  if run_setup >"$root/error" 2>&1; then
    echo "unexpected predecessor setup success: $failure" >&2; exit 1
  fi
  assert_restored
  if [[ "$failure" == signature ]]; then [[ ! -f "$root/events" ]]; fi
done
reset_case
cp "$root/release-18.json" "$root/release-original.json"
python3 - "$root/release-18.json" <<'PY'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
release = json.loads(path.read_text())
release['target_commitish'] = 'b' * 40
path.write_text(json.dumps(release))
PY
if run_setup >"$root/error" 2>&1; then
  echo "accepted predecessor release differing from pin" >&2; exit 1
fi
assert_restored
[[ ! -f "$root/events" ]]
cp "$root/release-original.json" "$root/release-18.json"
if CQ_COMPANION_TEST_RELEASE_API=https://api.github.com/repos/other/cq/releases/18 run_setup >"$root/error" 2>&1; then
  echo "accepted predecessor API outside expected repository" >&2; exit 1
fi
assert_restored
[[ ! -f "$root/events" ]]
if CI=false run_setup >"$root/error" 2>&1; then
  echo "accepted companion setup outside disposable CI" >&2; exit 1
fi
[[ ! -f "$root/events" ]]
printf corrupt >>"$current_native_archive"
if run_setup >"$root/error" 2>&1; then
  echo "accepted candidate archive differing from pin" >&2; exit 1
fi
assert_restored
[[ ! -f "$root/events" ]]
echo "Homebrew predecessor companion, candidate cache, restoration and rejection tests passed"
