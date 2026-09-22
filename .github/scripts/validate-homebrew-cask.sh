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
abort "missing supported installer artifact" unless text.include?('installer script:')
abort "missing supported uninstall artifact" unless text.include?('uninstall script:')
%w[install uninstall].each do |action|
  command = '"$source" service ' + action + ' --owner=homebrew "--service-executable=$target"'
  abort "missing CQ lifecycle command: #{action}" unless text.scan(command).length == 1
  text.sub!(command, '/usr/bin/true')
end
{
  'dev.jacobcx.cq.proxy' => 'dev.jacobcx.cq.cask-validation.proxy',
  'dev.jacobcx.cq.refresh' => 'dev.jacobcx.cq.cask-validation.refresh',
}.each { |production, validation| text.gsub!(production, validation) }
abort "CQ lifecycle command survived validation isolation" if text.include?('"$source" service ')
abort "production CQ binary path survived validation isolation" if text.include?('#{HOMEBREW_PREFIX}/bin/cq"')
abort "production CQ launchd label survived validation isolation" if text.match?(/dev\.jacobcx\.cq\.(?:proxy|refresh)/)
preflight = <<~'BLOCK'
  preflight do
    system_command "/usr/bin/xattr",
                   args: ["-w", "com.apple.quarantine", "0081;00000000;CQValidation;", "#{staged_path}/cq"]
  end

BLOCK
text.sub!(/^  installer script:/) { preflight + "  installer script:" } or abort "missing installer artifact"
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
