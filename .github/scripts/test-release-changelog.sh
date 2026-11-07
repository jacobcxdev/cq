#!/usr/bin/env bash
set -euo pipefail

workflow="$(cd "$(dirname "$0")/../workflows" && pwd)/release.yml"
root=$(mktemp -d "${TMPDIR:-/tmp}/cq-release-changelog.XXXXXX")
trap 'rm -rf "$root"' EXIT

# Exercise the production step itself so a later workflow edit cannot silently
# revert the changelog baseline to the nearest unpublished Git tag.
python3 - "$workflow" "$root/baseline.sh" <<'PY'
from pathlib import Path
import sys
lines = Path(sys.argv[1]).read_text().splitlines()
name = "      - name: Bind published stable changelog baseline"
indices = [index for index, line in enumerate(lines) if line == name]
if len(indices) != 1:
    raise SystemExit("missing or ambiguous published changelog baseline step")
start = indices[0] + 1
while start < len(lines) and lines[start] != "        run: |":
    if lines[start].startswith("      - "):
        raise SystemExit("baseline step has no shell block")
    start += 1
if start == len(lines):
    raise SystemExit("baseline step has no shell block")
block = []
for line in lines[start + 1:]:
    if line and not line.startswith("          "):
        break
    block.append(line[10:])
if not block:
    raise SystemExit("baseline shell block is empty")
Path(sys.argv[2]).write_text("\n".join(block) + "\n")
PY

mkdir "$root/bin"
cat >"$root/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1 $2" == 'release list' ]] || exit 1
shift 2
query=''
repository=''
drafts=0
prereleases=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) repository=$2; shift 2 ;;
    --exclude-drafts) drafts=1; shift ;;
    --exclude-pre-releases) prereleases=1; shift ;;
    --json|--limit) shift 2 ;;
    --jq) query=$2; shift 2 ;;
    *) echo "unexpected release-list argument: $1" >&2; exit 1 ;;
  esac
done
[[ "$repository" == release-test/repository && "$drafts" == 1 && "$prereleases" == 1 ]]
filtered=$(jq '[.[] | select(.draft == false and .prerelease == false) | {tagName}]' "$RELEASE_FIXTURE_INVENTORY")
if [[ -n "$query" ]]; then
  jq -r "$query" <<<"$filtered"
else
  printf '%s\n' "$filtered"
fi
GH
chmod +x "$root/bin/gh"
export PATH="$root/bin:$PATH" GITHUB_REPOSITORY=release-test/repository
export RELEASE_FIXTURE_INVENTORY="$root/releases.json" GITHUB_ENV="$root/environment"

# Numeric ordering differs from lexical ordering, and newer non-public releases
# must never become the baseline even when they have a stable-looking version.
cat >"$RELEASE_FIXTURE_INVENTORY" <<'JSON'
[
  {"tagName":"v1.9.9","draft":false,"prerelease":false},
  {"tagName":"v1.10.0","draft":false,"prerelease":false},
  {"tagName":"v1.10.1","draft":true,"prerelease":false},
  {"tagName":"cq-native-stage-v1.11.0","draft":true,"prerelease":false},
  {"tagName":"cq-network-v1.99.0","draft":false,"prerelease":true}
]
JSON

git init --quiet -b main "$root/work"
cd "$root/work"
git config user.name 'Release test'
git config user.email 'release-test@example.invalid'
ssh-keygen -q -t ed25519 -N '' -f "$root/signing-key"
git config gpg.format ssh
git config user.signingkey "$root/signing-key"
git config commit.gpgsign true
git config tag.gpgsign true
printf 'base\n' > source
git add source
git commit --quiet -m 'test: recorded published base'
git tag -s v1.9.9 -m 'Published predecessor'
git tag -s v1.10.0 -m 'Published base'
printf 'voice fix\n' >> source
git commit --quiet -am 'fix: accepted composite identifiers'
git tag -s v1.10.1 -m 'Failed unpublished attempt'
printf 'second fix\n' >> source
git commit --quiet -am 'fix: refreshed idle state'
git tag -s cq-native-stage-v1.11.0 -m 'Unpublished native staging'
printf 'reviewed pin\n' >> source
git commit --quiet -am 'chore: pinned signed companion'
git tag -s v1.11.0 -m 'Parent candidate'
export GITHUB_REF_NAME=v1.11.0

# Prove this history reproduces the truncation before checking the fix.
nearest=$(git describe --tags --abbrev=0 'tags/v1.11.0^')
[[ "$nearest" == cq-native-stage-v1.11.0 ]]
[[ "$(git rev-list --count "$nearest..v1.11.0")" == 1 ]]

: >"$GITHUB_ENV"
bash "$root/baseline.sh"
[[ "$(cat "$GITHUB_ENV")" == GORELEASER_PREVIOUS_TAG=v1.10.0 ]]
baseline=$(sed -n 's/^GORELEASER_PREVIOUS_TAG=//p' "$GITHUB_ENV")
[[ "$(git rev-list --count "$baseline..v1.11.0")" == 3 ]]
git log --format=%s "$baseline..v1.11.0" >"$root/changelog"
grep -Fxq 'fix: accepted composite identifiers' "$root/changelog"
grep -Fxq 'fix: refreshed idle state' "$root/changelog"

# Missing local tags and tags outside the release ancestry must fail closed.
git checkout --quiet --orphan unrelated
printf 'unrelated\n' > source
git add source
git commit --quiet -m 'test: recorded unrelated source'
git tag -s v1.10.9 -m 'Unrelated release'
git checkout --quiet main
for inventory in '[]' '[{"tagName":"invalid","draft":false,"prerelease":false}]' '[{"tagName":"v9.0.0","draft":false,"prerelease":false}]' '[{"tagName":"v1.10.9","draft":false,"prerelease":false}]'; do
  printf '%s\n' "$inventory" >"$RELEASE_FIXTURE_INVENTORY"
  : >"$GITHUB_ENV"
  if bash "$root/baseline.sh" >"$root/failure.log" 2>&1; then
    echo 'missing published stable changelog baseline was accepted' >&2
    exit 1
  fi
  [[ ! -s "$GITHUB_ENV" ]]
done

echo 'Published stable changelog baseline ignored staging, failed and non-public tags; full fix range retained.'
