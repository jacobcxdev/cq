#!/bin/bash
set -euo pipefail
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/staged" "$root/bin" "$root/home/Library/LaunchAgents"
export CQ_HOOK_TEST_ROOT="$root"
ruby -ryaml - .goreleaser.yml "$root/staged/cq-homebrew-lifecycle.sh" <<'RUBY'
config, destination = ARGV
block = YAML.load_file(config).fetch("homebrew_casks").first.fetch("custom_block")
script = block.match(/cq_lifecycle_script = <<~'SH'\n(.*?)^SH$/m).captures.first
script = script.lines.map { |line| line.delete_prefix("  ") }.join
script.gsub!("/usr/bin/xattr", "\"$CQ_HOOK_TEST_ROOT/xattr\"")
script.gsub!("/bin/launchctl", "\"$CQ_HOOK_TEST_ROOT/launchctl\"")
File.write(destination, script)
RUBY
cat > "$root/xattr" <<'MOCK'
#!/bin/bash
set -euo pipefail
if [[ "${1:-}" == -d ]]; then
  [[ ! -e "$CQ_HOOK_TEST_ROOT/reject-xattr" ]] || exit 1
  rm -f "$CQ_HOOK_TEST_ROOT/quarantine"
elif [[ -e "$CQ_HOOK_TEST_ROOT/quarantine" ]]; then
  echo com.apple.quarantine
fi
MOCK
cat > "$root/launchctl" <<'MOCK'
#!/bin/bash
printf '%s\n' "$*" >> "$CQ_HOOK_TEST_ROOT/launchctl.log"
if [[ "${1:-}" == print ]]; then
  remaining=0
  if [[ -f "$CQ_HOOK_TEST_ROOT/pending-bootout" ]]; then
    remaining=$(cat "$CQ_HOOK_TEST_ROOT/pending-bootout")
  fi
  if (( remaining > 0 )); then
    printf '%s\n' "$((remaining - 1))" > "$CQ_HOOK_TEST_ROOT/pending-bootout"
    exit 0
  fi
  if [[ "${2:-}" == */dev.jacobcx.cq.proxy && -f "$CQ_HOOK_TEST_ROOT/flap" ]]; then
    phase=$(cat "$CQ_HOOK_TEST_ROOT/flap")
    if [[ "$phase" == 2 ]]; then
      printf '1\n' > "$CQ_HOOK_TEST_ROOT/flap"
      exit 113
    fi
    rm "$CQ_HOOK_TEST_ROOT/flap"
    exit 0
  fi
  exit 113
fi
MOCK
cat > "$root/staged/cq" <<'MOCK'
#!/bin/bash
set -euo pipefail
[[ ! -e "$CQ_HOOK_TEST_ROOT/quarantine" ]] || exit 1
printf '%s\n' "$*" >> "$CQ_HOOK_TEST_ROOT/service.log"
[[ ! -e "$CQ_HOOK_TEST_ROOT/reject-service" ]] || exit 1
MOCK
chmod +x "$root/xattr" "$root/launchctl" "$root/staged/cq"
hook="$root/staged/cq-homebrew-lifecycle.sh"
target="$root/bin/cq"
run_hook() { HOME="$root/home" /bin/bash "$hook" "$1" "$root/staged/cq" "$target"; }
expect_failure() { if run_hook "$1" >"$root/error" 2>&1; then echo "unexpected lifecycle success" >&2; exit 1; fi; }
touch "$root/quarantine"
run_hook install
[[ -L "$target" && "$target" -ef "$root/staged/cq" && ! -e "$root/quarantine" ]] || exit 1
run_hook install
run_hook uninstall
[[ $(wc -l < "$root/service.log") -eq 3 ]] || exit 1
printf '3\n' > "$root/pending-bootout"
printf '2\n' > "$root/flap"
run_hook install
[[ $(cat "$root/pending-bootout") == 0 && ! -e "$root/flap" ]] || exit 1
run_hook uninstall
[[ $(wc -l < "$root/service.log") -eq 5 ]] || exit 1
rm "$target"
printf foreign > "$target"
expect_failure install
expect_failure uninstall
[[ $(cat "$target") == foreign ]] || exit 1
rm "$target"
ln -s "$root/xattr" "$target"
expect_failure install
expect_failure uninstall
[[ $(readlink "$target") == "$root/xattr" ]] || exit 1
rm "$target"
touch "$root/reject-service"
expect_failure install
[[ ! -e "$target" && ! -L "$target" ]] || exit 1
rm "$root/reject-service"
touch "$root/quarantine" "$root/reject-xattr"
expect_failure install
[[ ! -e "$target" && ! -L "$target" ]] || exit 1
rm "$root/quarantine" "$root/reject-xattr"
run_hook install
rm "$root/staged/cq"
touch "$root/home/Library/LaunchAgents/dev.jacobcx.cq.proxy.plist" "$root/home/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist"
run_hook uninstall
[[ ! -e "$root/home/Library/LaunchAgents/dev.jacobcx.cq.proxy.plist" && ! -e "$root/home/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist" ]] || exit 1
[[ $(grep -c '^bootout ' "$root/launchctl.log") -eq 2 ]] || exit 1
echo "Homebrew lifecycle ownership, quarantine, rollback, and missing-binary tests passed"
