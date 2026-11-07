#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 || "$(uname -s)" != Darwin ]]; then
  echo "usage (macOS): package-macos-bundles.sh VERSION DIST NATIVE_PIN VERIFIED_NATIVE_APP" >&2
  exit 64
fi
version=$1
distribution=$2
manifest=$3
app=$4
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "bundle version must be stable" >&2; exit 64; }
[[ -d "$distribution" && -f "$manifest" && -d "$app" ]]
[[ "$(jq -er .schema_version "$manifest")" == 2 && "$(jq -er .version "$manifest")" == "$version" ]] || { echo "CQ and Network must use the same parent version" >&2; exit 1; }
temporary=$(mktemp -d "${TMPDIR:-/tmp}/cq-bundled-release.XXXXXX")
trap 'rm -rf "$temporary"' EXIT
go build -o "$temporary/nativepackage" ./internal/tools/nativepackage

for architecture in arm64 amd64; do
  archive="$distribution/cq_${version}_darwin_${architecture}_bundle.tar.gz"
  .github/scripts/build-macos-bundle.sh "$distribution/cq_${version}_darwin_${architecture}.tar.gz" "$app" "$archive"
  extracted="$temporary/$architecture"
  mkdir "$extracted"
  /usr/bin/tar -xzf "$archive" -C "$extracted"
  python3 - "$extracted/cq" "$version" <<'PY'
import re, subprocess, sys
information = subprocess.check_output(["go", "version", "-m", sys.argv[1]], text=True, stderr=subprocess.PIPE)
if "\tpath\tgithub.com/jacobcxdev/cq/cmd/cq\n" not in information or not re.search(r"-X main\.version=" + re.escape(sys.argv[2]) + r'(?=[\s"\\]|$)', information):
    raise SystemExit("bundled CLI identity or version differs")
# Build flags contain release-time link data; never print or persist them.
PY
  host="$extracted/CQNetwork.app"
  provider="$host/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension"
  requirement='anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "K2648T24P4"'
  /usr/bin/codesign --verify --deep --strict --all-architectures "-R=$requirement and identifier \"dev.jacobcx.cq.network\"" "$host"
  /usr/bin/codesign --verify --strict --all-architectures "-R=$requirement and identifier \"dev.jacobcx.cq.network.proxy\"" "$provider"
  for bundle in "$host" "$provider"; do
    [[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$bundle/Contents/Info.plist")" == "$version" ]] || { echo "bundled Network version differs from CQ" >&2; exit 1; }
  done
  # A transient Apple ticket-service disconnect may be retried; invalid tickets fail.
  for attempt in 1 2 3; do
    if /usr/bin/xcrun stapler validate "$host" >"$temporary/stapler.log" 2>&1; then
      cat "$temporary/stapler.log"
      break
    else
      validation_status=$?
    fi
    cat "$temporary/stapler.log" >&2
    if [[ "$validation_status" -ne 68 || "$attempt" -eq 3 ]] || ! /usr/bin/grep -Fq 'NSURLErrorDomain Code=-1005' "$temporary/stapler.log"; then
      exit "$validation_status"
    fi
    sleep 2
  done
  /usr/sbin/spctl --assess --type execute "$host"
  native_architecture=$architecture
  if [[ "$architecture" == amd64 ]]; then native_architecture=x86_64; fi
  /usr/bin/lipo "$extracted/cq" -verify_arch "$native_architecture"
  for native_architecture in arm64 x86_64; do
    /usr/bin/lipo "$host/Contents/MacOS/CQNetwork" -verify_arch "$native_architecture"
    /usr/bin/lipo "$provider/Contents/MacOS/CQNetworkProxy" -verify_arch "$native_architecture"
  done
done

ruby - "$version" "$distribution" "$manifest" <<'RUBY'
require "digest"
require "json"
version, distribution, manifest = ARGV
pin = JSON.parse(File.read(manifest))
bundles = %w[arm64 amd64].to_h do |architecture|
  name = "cq_#{version}_darwin_#{architecture}_bundle.tar.gz"
  [architecture, {"url" => "https://github.com/jacobcxdev/cq/releases/download/v#{version}/#{name}",
                  "sha256" => Digest::SHA256.file(File.join(distribution, name)).hexdigest}]
end
pin["bundles"] = bundles
File.write(File.join(distribution, "macos-bundles.json"), JSON.pretty_generate(bundles) + "\n")
File.write(File.join(distribution, "bundled-native-network.json"), JSON.pretty_generate(pin) + "\n")
cask = File.join(distribution, "homebrew/Casks/cq.rb")
text = File.read(cask)
raise "CQ Cask version differs" unless text.match?(/^\s*version "#{Regexp.escape(version)}"$/)
pins = text.to_enum(:scan, /^([ \t]*)pin = JSON\.parse\(<<~(?:'JSON'|JSON)\)\n(.*?)^\s*JSON$/m).map { Regexp.last_match }
raise "missing or ambiguous CQ companion pin" unless pins.length == 1
old_pin = JSON.parse(pins.first[2])
raise "CQ Cask source pin differs" unless old_pin == JSON.parse(File.read(manifest)) || old_pin == pin
indent = pins.first[1]
text.sub!(pins.first[0], "#{indent}pin = JSON.parse(<<~'JSON')\n#{indent}  #{JSON.generate(pin)}\n#{indent}JSON")
lines = text.lines
seen = []
seen_linux = []
lines.each_with_index do |line, index|
  match = line.strip.match(/\Aurl "([^"]+)"\z/)
  next unless match
  old_checksum = lines[index - 1]&.match(/\A(\s*)sha256 "([0-9a-f]{64})"\s*\z/)
  raise "missing Cask archive checksum" unless old_checksum
  linux_architecture = %w[arm64 amd64].find do |candidate|
    [version, '#{version}'].any? do |token|
      match[1] == "https://github.com/jacobcxdev/cq/releases/download/v#{token}/cq_#{token}_linux_#{candidate}.tar.gz"
    end
  end
  if linux_architecture
    raise "duplicate Linux Cask archive" if seen_linux.include?(linux_architecture)
    archive = File.join(distribution, "cq_#{version}_linux_#{linux_architecture}.tar.gz")
    raise "Linux Cask input checksum differs" unless old_checksum[2] == Digest::SHA256.file(archive).hexdigest
    seen_linux << linux_architecture
    next
  end
  architecture = %w[arm64 amd64].find do |candidate|
    [version, '#{version}'].any? do |token|
      %w[.tar.gz _bundle.tar.gz].any? do |suffix|
        match[1] == "https://github.com/jacobcxdev/cq/releases/download/v#{token}/cq_#{token}_darwin_#{candidate}#{suffix}"
      end
    end
  end
  raise "foreign or unexpected CQ Cask archive" unless architecture
  raise "duplicate Darwin Cask archive" if seen.include?(architecture)
  cli = File.join(distribution, "cq_#{version}_darwin_#{architecture}.tar.gz")
  expected = bundles.fetch(architecture).fetch("sha256")
  input_checksum = match[1].end_with?("_bundle.tar.gz") ? expected : Digest::SHA256.file(cli).hexdigest
  raise "Darwin Cask input checksum differs" unless old_checksum[2] == input_checksum
  lines[index - 1] = "#{old_checksum[1]}sha256 \"#{expected}\"\n"
  prefix = line[/\A\s*/]
  url = bundles.fetch(architecture).fetch("url").gsub(version, '#{version}')
  lines[index] = "#{prefix}url \"#{url}\"\n"
  seen << architecture
end
raise "CQ Cask must contain both Darwin architectures" unless seen.sort == %w[amd64 arm64]
File.write(cask, lines.join)
checksums = File.join(distribution, "checksums.txt")
names = bundles.values.map { |bundle| bundle.fetch("url").split('/').last } + ["macos-bundles.json", "CQNetwork_#{version}_universal.zip"]
entries = File.readlines(checksums).reject { |line| names.include?(line.split.last) }
bundles.each_value { |bundle| entries << "#{bundle.fetch('sha256')}  #{bundle.fetch('url').split('/').last}\n" }
entries << "#{Digest::SHA256.file(File.join(distribution, 'macos-bundles.json')).hexdigest}  macos-bundles.json\n"
entries << "#{pin.fetch('sha256')}  CQNetwork_#{version}_universal.zip\n"
File.write(checksums, entries.join)
RUBY

"$temporary/nativepackage" cask "$distribution/bundled-native-network.json" "$distribution/homebrew/Casks/cq-network.rb"
echo "Prepared shared-version macOS bundles and paired Casks for CQ $version."
