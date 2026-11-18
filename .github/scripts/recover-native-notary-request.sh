#!/usr/bin/env bash
set -euo pipefail

repository=${GITHUB_REPOSITORY:?}
tag=${GITHUB_REF_NAME:?}
source=${GITHUB_SHA:?}
current_run=${GITHUB_RUN_ID:?}
current_attempt=${GITHUB_RUN_ATTEMPT:?}
output=${1:?}

[[ "$repository" == jacobcxdev/cq && "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ && "$source" =~ ^[0-9a-f]{40}$ ]]
[[ "$current_run" =~ ^[1-9][0-9]*$ && "$current_attempt" =~ ^[1-9][0-9]*$ ]]

# A fresh dispatch must also find an earlier submission for the same tag.
# Job history survives request expiry or interruption before artifact upload.
runs=$(gh api --paginate --slurp "repos/$repository/actions/workflows/release.yml/runs?event=workflow_dispatch&head_sha=$source&per_page=100" |
  jq -er --arg sha "$source" --arg tag "$tag" '
    map(.workflow_runs) | add | map(select(.head_sha == $sha and .head_branch == $tag))
    | sort_by(.run_number) | reverse | map(.id | tostring) | join("\n")')
if [[ $'\n'"$runs"$'\n' != *$'\n'"$current_run"$'\n'* ]]; then
  echo 'The current release run is missing from GitHub history; retry the check.' >&2
  exit 1
fi
while IFS= read -r run; do
  [[ "$run" =~ ^[1-9][0-9]*$ ]]
  exclude_attempt=0
  if [[ "$run" == "$current_run" ]]; then exclude_attempt=$current_attempt; fi
  attempt=$(gh api --paginate --slurp "repos/$repository/actions/runs/$run/jobs?filter=all&per_page=100" |
    jq -er --argjson exclude "$exclude_attempt" '
      map(.jobs) | add | map(select(.name == "native-network" and .run_attempt != $exclude
        and any(.steps[]?; (.name == "Submit the native companion for notarisation"
          or .name == "Build, sign and submit the native companion")
          and .started_at != null and .conclusion != "skipped")))
      | map(.run_attempt) | max // ""')
  [[ -n "$attempt" ]] || continue

  name=native-notary-request-$attempt
  if ! gh api --paginate --slurp "repos/$repository/actions/runs/$run/artifacts?per_page=100" |
    jq -e --arg name "$name" 'map(.artifacts) | add | any(.name == $name and .expired == false)' >/dev/null; then
    echo "Previous native submission attempt $run/$attempt has no retained request. Reconcile it with Apple before retrying." >&2
    exit 1
  fi
  mkdir -p "$output/native-request"
  gh run download "$run" --repo "$repository" --name "$name" --dir "$output/native-request"
  echo 'restored=true' >> "${GITHUB_OUTPUT:?}"
  exit 0
done <<< "$runs"
echo 'restored=false' >> "${GITHUB_OUTPUT:?}"
