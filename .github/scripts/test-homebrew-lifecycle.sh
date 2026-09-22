#!/usr/bin/env bash
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
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == -d ]]; then
  [[ ! -e "$CQ_HOOK_TEST_ROOT/reject-xattr" ]]
  rm -f "$CQ_HOOK_TEST_ROOT/quarantine"
elif [[ -e "$CQ_HOOK_TEST_ROOT/quarantine" ]]; then
  echo com.apple.quarantine
fi
MOCK
cat > "$root/launchctl" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$CQ_HOOK_TEST_ROOT/launchctl.log"
MOCK
cat > "$root/staged/cq" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[[ ! -e "$CQ_HOOK_TEST_ROOT/quarantine" ]]
printf '%s\n' "$*" >> "$CQ_HOOK_TEST_ROOT/service.log"
[[ ! -e "$CQ_HOOK_TEST_ROOT/reject-service" ]]
MOCK
chmod +x "$root/xattr" "$root/launchctl" "$root/staged/cq"
hook="$root/staged/cq-homebrew-lifecycle.sh"
target="$root/bin/cq"
run_hook() { HOME="$root/home" bash "$hook" "$1" "$root/staged/cq" "$target"; }
expect_failure() { if run_hook "$1" >"$root/error" 2>&1; then echo "unexpected lifecycle success" >&2; exit 1; fi; }
touch "$root/quarantine"
run_hook install
[[ -L "$target" && "$target" -ef "$root/staged/cq" && ! -e "$root/quarantine" ]]
run_hook install
run_hook uninstall
[[ $(wc -l < "$root/service.log") -eq 3 ]]
rm "$target"
printf foreign > "$target"
expect_failure install
expect_failure uninstall
[[ $(cat "$target") == foreign ]]
rm "$target"
ln -s "$root/xattr" "$target"
expect_failure install
expect_failure uninstall
[[ $(readlink "$target") == "$root/xattr" ]]
rm "$target"
touch "$root/reject-service"
expect_failure install
[[ ! -e "$target" && ! -L "$target" ]]
rm "$root/reject-service"
touch "$root/quarantine" "$root/reject-xattr"
expect_failure install
[[ ! -e "$target" && ! -L "$target" ]]
rm "$root/quarantine" "$root/reject-xattr"
run_hook install
rm "$root/staged/cq"
touch "$root/home/Library/LaunchAgents/dev.jacobcx.cq.proxy.plist" "$root/home/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist"
run_hook uninstall
[[ ! -e "$root/home/Library/LaunchAgents/dev.jacobcx.cq.proxy.plist" && ! -e "$root/home/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist" ]]
[[ $(wc -l < "$root/launchctl.log") -eq 4 ]]
echo "Homebrew lifecycle ownership, quarantine, rollback, and missing-binary tests passed"
