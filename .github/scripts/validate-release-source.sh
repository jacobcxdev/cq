#!/usr/bin/env bash
set -euo pipefail

if [[ ! "${GITHUB_REF:-}" =~ ^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo 'release must run against a stable version tag' >&2
  exit 1
fi

git fetch --no-tags origin main
main_commit=$(git rev-parse --verify 'FETCH_HEAD^{commit}')
release_commit=$(git rev-parse --verify "$GITHUB_REF^{commit}")
if [[ "$release_commit" != "$(git rev-parse HEAD)" ]]; then
  echo 'release tag does not identify the checked-out commit' >&2
  exit 1
fi
if [[ "$release_commit" != "$main_commit" ]]; then
  echo 'release tag must identify current reviewed main; update main and tag its final commit' >&2
  exit 1
fi
latest=$(gh release list --repo "${GITHUB_REPOSITORY:?}" --exclude-drafts --exclude-pre-releases --limit 1000 --json tagName \
  --jq 'map(.tagName | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) | ltrimstr("v") | split(".") | map(tonumber)) | max // empty | map(tostring) | join(".")')
if [[ ! "$latest" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo 'cannot determine the highest published stable release' >&2
  exit 1
fi
IFS=. read -r major minor patch <<<"$latest"
version=${GITHUB_REF#refs/tags/v}
if [[ "$version" != "$major.$minor.$((patch + 1))" && \
      "$version" != "$major.$((minor + 1)).0" && \
      "$version" != "$((major + 1)).0.0" ]]; then
  echo "release must follow published v$latest with the next patch, minor or major version" >&2
  exit 1
fi
if ! jq -e --arg version "$version" '.schema_version == 2 and .version == $version and (has("bundles") | not)' .github/native-network-release.json >/dev/null; then
  echo 'CQ and Network must share the selected parent release version and a reviewed base pin' >&2
  exit 1
fi
echo "Verified release source: $GITHUB_REF at reviewed main $main_commit"
