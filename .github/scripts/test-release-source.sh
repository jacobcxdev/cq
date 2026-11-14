#!/usr/bin/env bash
set -euo pipefail

validator="$(cd "$(dirname "$0")" && pwd)/validate-release-source.sh"
root=$(mktemp -d "${TMPDIR:-/tmp}/cq-release-source.XXXXXX")
trap 'rm -rf "$root"' EXIT
git init --bare --quiet "$root/origin.git"
git init --quiet -b main "$root/work"
cd "$root/work"
git config user.name 'Release test'
git config user.email 'release-test@example.invalid'
ssh-keygen -q -t ed25519 -N '' -f "$root/signing-key"
git config gpg.format ssh
git config user.signingkey "$root/signing-key"
git config commit.gpgsign true
git remote add origin "$root/origin.git"
printf 'base\n' > source
git add source
git commit --quiet -m base
base=$(git rev-parse HEAD)
printf 'merged fix\n' >> source
git commit --quiet -am 'merged fix'
git push --quiet origin main
git tag -s v1.0.1 -m release
mkdir "$root/bin"
cat >"$root/bin/gh" <<'SH'
#!/usr/bin/env bash
[[ "$*" == *'release list --repo release-test/repository --exclude-drafts --exclude-pre-releases'* ]] || exit 1
printf '%s\n' "${PUBLISHED_VERSION:?}"
SH
chmod +x "$root/bin/gh"
export PATH="$root/bin:$PATH" GITHUB_REPOSITORY=release-test/repository PUBLISHED_VERSION=1.0.0

expect() {
  local expected=$1 ref=$2
  if GITHUB_REF="$ref" bash "$validator" >"$root/result" 2>&1; then
    actual=pass
  else
    actual=fail
  fi
  if [[ "$actual" != "$expected" ]]; then
    cat "$root/result" >&2
    echo "release source check: expected $expected for $ref, got $actual" >&2
    exit 1
  fi
}

# The reviewed main commit, including an annotated release tag, is accepted.
expect pass refs/tags/v1.0.1
expect fail refs/heads/main
expect fail refs/tags/v1.0.1-rc
expect fail refs/tags/v9.9.9

# Only the next patch, minor or major may follow the highest public release.
for version in 1.0.2 1.1.0 2.0.0 1.1.1 1.2.0 3.0.0 0.9.9; do
  git tag "v$version"
done
expect fail refs/tags/v1.0.2
expect pass refs/tags/v1.1.0
expect pass refs/tags/v2.0.0
expect fail refs/tags/v1.1.1
expect fail refs/tags/v1.2.0
expect fail refs/tags/v3.0.0
expect fail refs/tags/v0.9.9
PUBLISHED_VERSION=1.0.1 expect pass refs/tags/v1.0.2
PUBLISHED_VERSION=1.0.1 expect fail refs/tags/v1.0.1
PUBLISHED_VERSION= expect fail refs/tags/v1.0.1
PUBLISHED_VERSION=invalid expect fail refs/tags/v1.0.1
git tag -d v1.0.2 >/dev/null

# A correctly named tag must identify the checked-out commit.
git tag v1.0.0 "$base"
expect fail refs/tags/v1.0.0

# Reproduce a pre-merge release branch which omitted a fix already on main.
git checkout --quiet -b stale "$base"
printf 'other change\n' > other
git add other
git commit --quiet -m 'other change'
git tag v1.0.2
expect fail refs/tags/v1.0.2

# Even an older main commit cannot be published over its current tip.
git checkout --quiet --detach "$base"
expect fail refs/tags/v1.0.0

echo 'Release source checks passed: main, tags, checkout, stale branches and sequential versions.'
