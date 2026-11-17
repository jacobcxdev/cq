#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/recover-native-notary-request.sh"
root=$(mktemp -d "${TMPDIR:-/tmp}/cq-native-recovery-test.XXXXXX")
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/bin"
export GITHUB_REPOSITORY=jacobcxdev/cq GITHUB_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export GITHUB_REF_NAME=v1.2.3 GITHUB_RUN_ID=22 GITHUB_RUN_ATTEMPT=1
export FIXTURE="$root" GITHUB_OUTPUT="$root/outputs" PATH="$root/bin:$PATH"

cat > "$root/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'api --paginate --slurp repos/jacobcxdev/cq/actions/workflows/release.yml/runs?'*) cat "$FIXTURE/runs" ;;
  'api --paginate --slurp repos/jacobcxdev/cq/actions/runs/'*'/jobs?'*)
    endpoint=${4%/jobs\?*}
    cat "$FIXTURE/jobs-${endpoint##*/}" ;;
  'api --paginate --slurp repos/jacobcxdev/cq/actions/runs/'*'/artifacts?'*)
    endpoint=${4%/artifacts\?*}
    cat "$FIXTURE/artifacts-${endpoint##*/}" ;;
  'run download '*) printf '%s\n' "$3 $7" > "$FIXTURE/download" ;;
  *) echo "Unexpected GitHub command: $*" >&2; exit 1 ;;
esac
FAKE
chmod +x "$root/bin/gh"

jq -n --arg sha "$GITHUB_SHA" '[{workflow_runs:[
    {id:20,run_number:1,head_branch:"v1.2.4",head_sha:$sha},
    {id:21,run_number:2,head_branch:"v1.2.3",head_sha:$sha},
    {id:22,run_number:3,head_branch:"v1.2.3",head_sha:$sha}
  ]}]' > "$root/runs"
job() {
  jq -n --argjson attempt "$1" --arg conclusion "$2" '[{jobs:[{
    name:"native-network",run_attempt:$attempt,steps:[{
      name:"Build, sign and submit the native companion",started_at:"2026-11-17T18:00:00Z",conclusion:$conclusion
    }]}]}]'
}
artifact() {
  jq -n --arg name "native-notary-request-$1" --argjson expired "$2" '[{artifacts:[{name:$name,expired:$expired}]}]'
}
run() {
  : > "$GITHUB_OUTPUT"
  bash "$script" "$root/output"
}

job 1 success > "$root/jobs-22" # The current job must not count as an earlier attempt.
job 1 skipped > "$root/jobs-21"
run
[[ "$(cat "$GITHUB_OUTPUT")" == 'restored=false' && ! -e "$root/download" ]]

# A fresh dispatch recovers a request from a prior run at the same tag.
job 3 failure > "$root/jobs-21"
artifact 3 false > "$root/artifacts-21"
run
[[ "$(cat "$GITHUB_OUTPUT")" == 'restored=true' ]]
[[ "$(cat "$root/download")" == '21 native-notary-request-3' ]]

# A rerun uses the exact attempt which started signing.
export GITHUB_RUN_ATTEMPT=2
artifact 1 false > "$root/artifacts-22"
run
[[ "$(cat "$root/download")" == '22 native-notary-request-1' ]]

# A missing, unrelated or expired artefact cannot authorise a new upload.
for expiry in true false; do
  if [[ "$expiry" == true ]]; then artifact 1 true; else artifact 2 false; fi > "$root/artifacts-22"
  if run > "$root/error" 2>&1; then
    echo 'A previous attempt without a retained request was accepted.' >&2
    exit 1
  fi
  grep -q 'Reconcile it with Apple before retrying' "$root/error"
  [[ ! -s "$GITHUB_OUTPUT" ]]
done

echo 'Native notarisation recovery checks passed.'
