#!/usr/bin/env bash
set -euo pipefail

resolver="$(cd "$(dirname "$0")" && pwd)/resolve-native-network.sh"
root=$(mktemp -d "${TMPDIR:-/tmp}/cq-native-resolve-test.XXXXXX")
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/bin"
export RESOLVER_RELEASE_JSON="$root/release.json" RESOLVER_GO_ARGS="$root/go-args"
source_commit=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
target_commit=cccccccccccccccccccccccccccccccccccccccc
digest=$(printf 'b%.0s' {1..64})
export RESOLVER_SOURCE_COMMIT="$source_commit" RESOLVER_TARGET_COMMIT="$target_commit" RESOLVER_TAG_VERIFIED=true
export RESOLVER_RELEASE_API=https://api.github.com/repos/jacobcxdev/cq/releases/123

cat >"$root/bin/gh" <<'SH'
#!/usr/bin/env bash
case "$*" in
  'release view v1.2.3 --repo jacobcxdev/cq --json apiUrl --jq .apiUrl'|'release view cq-native-stage-v1.2.3 --repo jacobcxdev/cq --json apiUrl --jq .apiUrl') printf '%s\n' "$RESOLVER_RELEASE_API" ;;
  'api repos/jacobcxdev/cq/releases/123') cat "$RESOLVER_RELEASE_JSON" ;;
  'api repos/jacobcxdev/cq/git/ref/tags/cq-native-stage-v1.2.3') printf '{"object":{"type":"tag","sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}}\n' ;;
  'api repos/jacobcxdev/cq/git/ref/tags/v1.2.3') printf '{"object":{"type":"tag","sha":"ffffffffffffffffffffffffffffffffffffffff"}}\n' ;;
  'api repos/jacobcxdev/cq/git/tags/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee') printf '{"tag":"cq-native-stage-v1.2.3","object":{"type":"commit","sha":"%s"},"verification":{"verified":%s}}\n' "$RESOLVER_SOURCE_COMMIT" "$RESOLVER_TAG_VERIFIED" ;;
  'api repos/jacobcxdev/cq/git/tags/ffffffffffffffffffffffffffffffffffffffff') printf '{"tag":"v1.2.3","object":{"type":"commit","sha":"%s"},"verification":{"verified":%s}}\n' "$RESOLVER_TARGET_COMMIT" "$RESOLVER_TAG_VERIFIED" ;;
  *) exit 1 ;;
esac
SH
cat >"$root/bin/git" <<'SH'
#!/usr/bin/env bash
case "$*" in
  'rev-parse --verify refs/tags/cq-native-stage-v1.2.3^{commit}') printf '%s\n' "$RESOLVER_SOURCE_COMMIT" ;;
  'rev-parse --verify refs/tags/v1.2.3^{commit}') printf '%s\n' "$RESOLVER_TARGET_COMMIT" ;;
  *) exit 1 ;;
esac
SH
cat >"$root/bin/go" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$RESOLVER_GO_ARGS"
printf '{"schema_version":2}\n'
SH
chmod +x "$root/bin/gh" "$root/bin/git" "$root/bin/go"
export PATH="$root/bin:$PATH"

jq -n --arg target "$target_commit" --arg digest "sha256:$digest" '{
  tag_name: "v1.2.3", name: "v1.2.3", target_commitish: $target,
  draft: true, prerelease: false,
  assets: [{id: 123, name: "CQNetwork_1.2.3_universal.zip", state: "uploaded", digest: $digest}]
}' >"$root/good.json"

cp "$root/good.json" "$RESOLVER_RELEASE_JSON"
"$resolver" v1.2.3 "$root/manifest.json"
test -f "$root/manifest.json"
test "$(sed -n '3p' "$RESOLVER_GO_ARGS")" = pin-parent
test "$(sed -n '4p' "$RESOLVER_GO_ARGS")" = 1.2.3
test "$(sed -n '5p' "$RESOLVER_GO_ARGS")" = "$source_commit"
test "$(sed -n '6p' "$RESOLVER_GO_ARGS")" = 123
test "$(sed -n '7p' "$RESOLVER_GO_ARGS")" = "$digest"
jq '.draft = false' "$root/good.json" >"$RESOLVER_RELEASE_JSON"
"$resolver" v1.2.3 "$root/manifest.json" --published-fixture
test "$(sed -n '5p' "$RESOLVER_GO_ARGS")" = "$target_commit"
jq --arg source "$source_commit" '.tag_name = "cq-native-stage-v1.2.3" | .target_commitish = $source' "$root/good.json" >"$RESOLVER_RELEASE_JSON"
"$resolver" v1.2.3 "$root/manifest.json" --staging-draft
jq '.draft = false' "$RESOLVER_RELEASE_JSON" >"$root/not-draft.json"
cp "$root/not-draft.json" "$RESOLVER_RELEASE_JSON"
if "$resolver" v1.2.3 "$root/manifest.json" --staging-draft >"$root/result" 2>&1; then
  echo 'native resolver accepted published staging release' >&2
  exit 1
fi
cp "$root/good.json" "$RESOLVER_RELEASE_JSON"
RESOLVER_TAG_VERIFIED=false
export RESOLVER_TAG_VERIFIED
if "$resolver" v1.2.3 "$root/manifest.json" >"$root/result" 2>&1; then
  echo 'native resolver accepted unverified tag' >&2
  exit 1
fi
RESOLVER_TAG_VERIFIED=true
export RESOLVER_TAG_VERIFIED

for change in \
  '.tag_name = "v9.9.9"' \
  '.name = "other"' \
  '.prerelease = true' \
  '.target_commitish = "dddddddddddddddddddddddddddddddddddddddd"' \
  '.assets = []' \
  '.assets += [.assets[0]]' \
  '.assets[0].state = "starter"' \
  '.assets[0].digest = "sha256:bad"'; do
  jq "$change" "$root/good.json" >"$RESOLVER_RELEASE_JSON"
  if "$resolver" v1.2.3 "$root/manifest.json" >"$root/result" 2>&1; then
    echo "native resolver accepted invalid release: $change" >&2
    exit 1
  fi
done
if "$resolver" bad-tag "$root/manifest.json" >"$root/result" 2>&1; then
  echo 'native resolver accepted invalid tag' >&2
  exit 1
fi
for api in https://example.com/releases/123 https://api.github.com/repos/other/cq/releases/123 https://api.github.com/repos/jacobcxdev/cq/releases/0; do
  if RESOLVER_RELEASE_API="$api" "$resolver" v1.2.3 "$root/manifest.json" >"$root/result" 2>&1; then
    echo "native resolver accepted invalid API identity: $api" >&2
    exit 1
  fi
done
echo 'Native release resolution checks passed.'
