#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: validate-homebrew-cask.sh CASK ARCHIVE" >&2
  exit 64
fi
if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "Homebrew Cask validation requires macOS" >&2
  exit 69
fi

cask_source=$1
archive=$2
if [[ ! -f "$cask_source" || ! -f "$archive" ]]; then
  echo "Cask and archive must be regular files" >&2
  exit 66
fi

validation_token=cq-cask-validation
validation_tap=cq-validation/local
validation_binary="$(brew --prefix)/bin/$validation_token"
validation_caskroom="$(brew --caskroom)/$validation_token"
installed_casks=$(brew list --cask)
if awk -v token="$validation_token" '$1 == token { found = 1 } END { exit !found }' <<<"$installed_casks"; then
  echo "refusing to replace installed $validation_token Cask" >&2
  exit 73
fi
if [[ -e "$validation_binary" || -L "$validation_binary" ]]; then
  echo "refusing to replace existing $validation_binary" >&2
  exit 73
fi
if [[ -e "$validation_caskroom" || -L "$validation_caskroom" ]]; then
  echo "refusing to replace existing $validation_caskroom" >&2
  exit 73
fi
installed_taps=$(brew tap)
if grep -Fxq "$validation_tap" <<<"$installed_taps"; then
  echo "refusing to replace existing $validation_tap tap" >&2
  exit 73
fi

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/cq-cask-validation.XXXXXX")
validation_cask="$temporary_root/$validation_token.rb"
cleanup() {
  result=$?
  trap - EXIT
  HOMEBREW_NO_AUTO_UPDATE=1 brew uninstall --cask --force "$validation_token" >/dev/null 2>&1 || true
  HOMEBREW_NO_AUTO_UPDATE=1 brew untap "$validation_tap" >/dev/null 2>&1 || true
  if installed_casks=$(brew list --cask 2>/dev/null); then
    if awk -v token="$validation_token" '$1 == token { found = 1 } END { exit !found }' <<<"$installed_casks"; then
      echo "validation Cask cleanup failed: $validation_token remains installed" >&2
      result=1
    fi
  else
    echo "validation Cask cleanup failed: cannot inspect installed Casks" >&2
    result=1
  fi
  if [[ -e "$validation_caskroom" || -L "$validation_caskroom" ]]; then
    echo "validation Caskroom cleanup failed: $validation_caskroom" >&2
    result=1
  fi
  if [[ -e "$validation_binary" || -L "$validation_binary" ]]; then
    echo "validation binary cleanup failed: $validation_binary" >&2
    result=1
  fi
  if installed_taps=$(brew tap 2>/dev/null); then
    if grep -Fxq "$validation_tap" <<<"$installed_taps"; then
      echo "validation tap cleanup failed: $validation_tap" >&2
      result=1
    fi
  else
    echo "validation tap cleanup failed: cannot inspect installed taps" >&2
    result=1
  fi
  chmod -R u+w "$temporary_root" 2>/dev/null || true
  find "$temporary_root" -depth -delete 2>/dev/null || result=1
  if [[ -e "$temporary_root" ]]; then
    echo "validation temporary directory cleanup failed: $temporary_root" >&2
    result=1
  fi
  exit "$result"
}
trap cleanup EXIT

ruby - "$cask_source" "$archive" "$validation_cask" <<'RUBY'
cask_source, archive, validation_cask = ARGV
text = File.read(cask_source)
text.sub!(/^cask "cq" do$/, 'cask "cq-cask-validation" do') or abort "missing cq Cask token"
text.gsub!(/^(\s*)url ".*"$/) { "#{Regexp.last_match(1)}url \"file://#{archive}\"" }
text.sub!(/^\s*binary "cq"$/, '  binary "cq", target: "cq-cask-validation"') or abort "missing cq binary artifact"
text.gsub!('#{HOMEBREW_PREFIX}/bin/cq', '#{HOMEBREW_PREFIX}/bin/cq-cask-validation')
[
  'dev.jacobcx.cq.proxy',
  'dev.jacobcx.cq.refresh',
  '#{Dir.home}/Library/LaunchAgents/dev.jacobcx.cq.proxy.plist',
  '#{Dir.home}/Library/LaunchAgents/dev.jacobcx.cq.refresh.plist',
].each do |backstop|
  abort "missing Homebrew uninstall backstop for #{backstop}" unless text.include?(backstop)
end
[
  'dev.jacobcx.cq.proxy',
  'dev.jacobcx.cq.refresh',
].each do |label|
  pattern = /args:\s*\["bootout",\s*"gui\/#\{Process\.uid\}\/#{Regexp.escape(label)}"\],\s*must_succeed:\s*false/m
  abort "Homebrew launchd uninstall backstop can fail uninstall: #{label}" unless text.match?(pattern)
end
{
  'dev.jacobcx.cq.proxy' => 'dev.jacobcx.cq.cask-validation.proxy',
  'dev.jacobcx.cq.refresh' => 'dev.jacobcx.cq.cask-validation.refresh',
}.each { |production, validation| text.gsub!(production, validation) }
lifecycle_pattern = /^([ \t]*)system_command "#\{HOMEBREW_PREFIX\}\/bin\/cq-cask-validation",\s+args:\s*\[\s*"service",\s*"(install|uninstall)".*?\]\n/m
lifecycle_commands = text.scan(lifecycle_pattern).map { |match| match.fetch(1) }.sort
abort "expected one CQ service install and uninstall command" unless lifecycle_commands == %w[install uninstall]
text.gsub!(lifecycle_pattern) do
  "#{Regexp.last_match(1)}system_command \"/usr/bin/true\"\n"
end
abort "CQ lifecycle command survived validation isolation" if text.match?(/args: \["service", "(?:install|uninstall)"/)
abort "production CQ binary path survived validation isolation" if text.include?('#{HOMEBREW_PREFIX}/bin/cq"')
abort "production CQ launchd label survived validation isolation" if text.match?(/dev\.jacobcx\.cq\.(?:proxy|refresh)/)
preflight = <<~'BLOCK'
  preflight do
    system_command "/usr/bin/xattr",
                   args: ["-w", "com.apple.quarantine", "0081;00000000;CQValidation;", "#{staged_path}/cq"]
  end

BLOCK
text.sub!(/^  postflight do$/, preflight + "  postflight do") or abort "missing postflight hook"
File.write(validation_cask, text)
RUBY

HOMEBREW_NO_AUTO_UPDATE=1 brew tap-new --no-git "$validation_tap" >/dev/null
validation_tap_root=$(brew --repository "$validation_tap")
mkdir -p "$validation_tap_root/Casks"
cp "$validation_cask" "$validation_tap_root/Casks/$validation_token.rb"
HOMEBREW_NO_AUTO_UPDATE=1 brew install --cask "$validation_tap/$validation_token"
if xattr -p com.apple.quarantine "$validation_binary" >/dev/null 2>&1; then
  echo "Homebrew Cask left cq quarantined" >&2
  exit 1
fi
find "$validation_binary" -depth -delete

echo "Homebrew Cask quarantine and missing-binary uninstall validation passed"
