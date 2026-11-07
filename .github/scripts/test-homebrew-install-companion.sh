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
import hashlib, io, json, pathlib, sys, tarfile, zipfile
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
for version, asset in [('0.47.1', 471), ('0.47.2', 472)]:
    archive = root / f'{version}.tar.gz'
    with tarfile.open(archive, 'w:gz') as tar:
        for path in ['cq', 'CQNetwork.app/Contents/Info.plist', 'CQNetwork.app/Contents/MacOS/CQNetwork', 'CQNetwork.app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/Info.plist', 'CQNetwork.app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/MacOS/CQNetworkProxy']:
            payload = (version + path).encode()
            entry = tarfile.TarInfo(path)
            entry.size, entry.mode = len(payload), 0o755
            tar.addfile(entry, io.BytesIO(payload))
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    # The bundle digest deliberately differs from the retained native ZIP pin.
    pin = dict(schema_version=2, version=version, source_commit='a'*40, asset_id=asset, sha256='d'*64, minimum_macos='13.0', bundles={})
    assets = [dict(id=asset, name=f'CQNetwork_{version}_universal.zip', digest='sha256:' + pin['sha256'], state='uploaded', browser_download_url=f'https://github.com/jacobcxdev/cq/releases/download/v{version}/CQNetwork_{version}_universal.zip')]
    for index, architecture in enumerate(['arm64', 'amd64']):
        name = f'cq_{version}_darwin_{architecture}_bundle.tar.gz'
        url = f'https://github.com/jacobcxdev/cq/releases/download/v{version}/{name}'
        pin['bundles'][architecture] = dict(url=url, sha256=digest)
        assets.append(dict(id=asset+1000+index, name=name, digest='sha256:'+digest, state='uploaded', browser_download_url=url))
    (root / f'{version}.json').write_text(json.dumps(pin))
    (root / f'{version}.rb').write_text("pin = JSON.parse(<<~JSON)\n" + json.dumps(pin) + '\nJSON\n')
    (root / f'release-{asset}.json').write_text(json.dumps(dict(tag_name=f'v{version}', target_commitish='b'*40, draft=False, prerelease=False, assets=assets)))
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
  '--cache --cask jacobcxdev/tap/cq-network') grep -q "version \"$CQ_COMPANION_TEST_CURRENT\"" "$selected"; echo "$root/cache/candidate.zip" ;;
  'uninstall --cask --force jacobcxdev/tap/cq-network') echo uninstall >>"$root/events"; rm -f "$root/installed" ;;
  'install --cask jacobcxdev/tap/cq-network')
    grep -q "version \"$CQ_COMPANION_TEST_PREVIOUS\"" "$selected"
    [[ ! -f "$root/reject-install" ]] || exit 1
    echo install-previous >>"$root/events"; echo "$CQ_COMPANION_TEST_PREVIOUS" >"$root/installed" ;;
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
  'release view v0.47.1 --repo jacobcxdev/cq --json apiUrl --jq .apiUrl') echo https://api.github.com/repos/jacobcxdev/cq/releases/471 ;;
  'api repos/jacobcxdev/cq/releases/471') cat "$root/release-471.json" ;;
  'api repos/jacobcxdev/cq/releases/assets/1471 --header Accept: application/octet-stream'|'api repos/jacobcxdev/cq/releases/assets/1472 --header Accept: application/octet-stream') cat "$root/0.47.1.tar.gz" ;;
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
# Mock only the read-only Git proofs, within this isolated fixture process.
cat >"$root/bin/git" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
a=$(printf '%040d' 0 | tr 0 a)
b=$(printf '%040d' 0 | tr 0 b)
c=$(printf '%040d' 0 | tr 0 c)
case "$*" in
  "merge-base --is-ancestor $a $b") exit 0 ;;
  "rev-parse --verify $a:native/macos-network"|"rev-parse --verify $b:native/macos-network") echo "$c" ;;
  'rev-parse --verify refs/tags/v0.47.1^{commit}') echo "$b" ;;
  *) echo "unexpected git proof call: $*" >&2; exit 1 ;;
esac
SH
chmod +x "$root/bin/"*
export PATH="$root/bin:$PATH"
export CI=true
export CQ_COMPANION_TEST_PREVIOUS=0.41.8 CQ_COMPANION_TEST_CURRENT=0.41.9
temporary_root="$root/work"
nativepackage_executable="$root/nativepackage"
previous_cask="$root/0.41.8.rb"
current_cask="$root/0.41.9.rb"
current_native_cask="$root/current-native.rb"
current_native_archive="$root/0.41.9.zip"
install_previous_cq() {
  [[ "$(cat "$root/installed")" == "$CQ_COMPANION_TEST_PREVIOUS" ]]
  grep -q "version \"$CQ_COMPANION_TEST_PREVIOUS\"" "$root/tap/Casks/cq-network.rb"
  [[ ! -f "$root/reject-cq" ]] || return 1
  echo previous-cq >>"$root/events"
}
install_unchanged_cq() {
  [[ "$(cat "$root/installed")" == "$CQ_COMPANION_TEST_CURRENT" ]]
  cmp "$root/current-native.rb" "$root/tap/Casks/cq-network.rb"
}
reset_case() {
  cp "$root/current-native.rb" "$root/tap/Casks/cq-network.rb"
  echo "$CQ_COMPANION_TEST_CURRENT" >"$root/installed"
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
# A bundled candidate must seed its install archive, not its native ZIP pin.
current_cask="$root/0.47.2.rb"
current_native_archive="$root/0.47.2.tar.gz"
export CQ_COMPANION_TEST_CURRENT=0.47.2
"$root/nativepackage" cask "$root/0.47.2.json" "$root/current-native.rb"
reset_case
run_setup
assert_restored
cmp "$current_native_archive" "$root/cache/candidate.zip"
[[ "$(cat "$root/events")" == $'uninstall\ninstall-previous\nprevious-cq' ]]
# A published bundled predecessor selects its exact archive from release assets.
previous_cask="$root/0.47.1.rb"
export CQ_COMPANION_TEST_PREVIOUS=0.47.1
reset_case
run_setup
assert_restored
cmp "$current_native_archive" "$root/cache/candidate.zip"
[[ -f "$root/work/previous-companion/unpacked/cq" ]]
[[ "$(cat "$root/events")" == $'uninstall\ninstall-previous\nprevious-cq' ]]
reset_case
previous_cask=$current_cask
run_setup install_unchanged_cq
assert_restored
[[ ! -f "$root/events" ]]
previous_cask="$root/0.47.1.rb"
for failure in install cq signature; do
  reset_case
  touch "$root/reject-$failure"
  if run_setup >"$root/error" 2>&1; then
    echo "unexpected bundled predecessor setup success: $failure" >&2; exit 1
  fi
  assert_restored
  if [[ "$failure" == signature ]]; then [[ ! -f "$root/events" ]]; fi
done
reset_case
cp "$root/release-471.json" "$root/release-parent-original.json"
for failure in duplicate wrong-digest foreign-url missing-other-architecture wrong-source; do
  cp "$root/release-parent-original.json" "$root/release-471.json"
  python3 - "$root/release-471.json" "$failure" <<'PY'
import json, pathlib, sys
path, failure = pathlib.Path(sys.argv[1]), sys.argv[2]
release = json.loads(path.read_text())
if failure == 'duplicate': release['assets'].append(release['assets'][1])
elif failure == 'wrong-digest': release['assets'][1]['digest'] = 'sha256:' + 'e'*64
elif failure == 'foreign-url': release['assets'][1]['browser_download_url'] = release['assets'][1]['browser_download_url'].replace('jacobcxdev/cq', 'other/cq')
elif failure == 'missing-other-architecture': release['assets'].pop()
elif failure == 'wrong-source': release['target_commitish'] = 'e'*40
path.write_text(json.dumps(release))
PY
  if run_setup >"$root/error" 2>&1; then
    echo "accepted invalid bundled predecessor metadata: $failure" >&2; exit 1
  fi
  assert_restored
  [[ ! -f "$root/events" ]]
done
cp "$root/release-parent-original.json" "$root/release-471.json"
printf corrupt >>"$root/0.47.1.tar.gz"
if run_setup >"$root/error" 2>&1; then
  echo "accepted bundled predecessor archive differing from pin" >&2; exit 1
fi
assert_restored
[[ ! -f "$root/events" ]]
printf corrupt >>"$current_native_archive"
if run_setup >"$root/error" 2>&1; then
  echo "accepted bundled candidate archive differing from pin" >&2; exit 1
fi
assert_restored
[[ ! -f "$root/events" ]]
echo "Homebrew legacy and bundled predecessor, candidate cache, restoration and rejection tests passed"
