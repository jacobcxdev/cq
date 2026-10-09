#!/usr/bin/env bash

set -euo pipefail

root=$(mktemp -d "${TMPDIR:-/tmp}/cq-homebrew-status.XXXXXX")
trap 'chmod -R u+rwX "$root"; rm -rf "$root"' EXIT
root=$(cd "$root" && pwd -P)
mkdir -p "$root/home"
go build -o "$root/previous" -ldflags '-X main.version=0.34.98' ./cmd/cq
go build -o "$root/current" -ldflags '-X main.version=0.34.99' ./cmd/cq
ruby - .github/scripts/validate-homebrew-install.sh "$root/status-validator.rb" <<'RUBY'
source, destination = ARGV
text = File.read(source)
validator = text.match(/validate_runtime_status\(\) \{\n.*?<<'RUBY'\n(.*?)^RUBY$/m)
abort "missing release status validator" unless validator
File.write(destination, validator.captures.first)
RUBY
ruby - "$root" "$(go env GOHOSTARCH)" "$(go env GOROOT)/bin/go" <<'RUBY'
require "digest"
require "fileutils"
require "json"
require "open3"
require "rbconfig"

root, architecture, go_executable = ARGV
runtime_root = File.join(root, "state", "runtime-artifacts")
FileUtils.mkdir_p(runtime_root, mode: 0o700)
File.chmod(0o700, File.dirname(runtime_root), runtime_root)
retained = %w[previous current].map do |name|
  source = File.join(root, name)
  digest = Digest::SHA256.file(source).hexdigest
  directory = File.join(runtime_root, digest)
  Dir.mkdir(directory, 0o700)
  path = File.join(directory, "cq")
  FileUtils.cp(source, path)
  File.chmod(0o500, path)
  path
end
package = File.join(root, "package-cq")
File.symlink(File.join(root, "current"), package)
status = {"owner" => "homebrew", "executable" => package,
          "proxy" => {"registered" => true, "running" => true, "healthy" => true, "pid" => 42, "listener" => "127.0.0.1:19280", "configured_executable" => retained[0], "live_executable" => retained[1]},
          "refresh" => {"registered" => true, "healthy" => true, "configured_executable" => retained[1]}, "active_runtime_version" => "0.34.99"}
verify = lambda do |value, mode, expected|
  output, error, result = Open3.capture3({"HOME" => File.join(root, "home"), "XDG_CONFIG_HOME" => "", "XDG_CACHE_HOME" => ""}, RbConfig.ruby, File.join(root, "status-validator.rb"), JSON.generate(value), package, runtime_root, "0.34.99", mode, architecture, go_executable)
  abort "status gate result differed: expected #{expected}, got #{result.success?}\n#{output}#{error}" unless result.success? == expected
end
copy = lambda { JSON.parse(JSON.generate(status)) }
verify.call(status, "retained", true)
legacy = copy.call
legacy.delete("active_runtime_version")
legacy["proxy"]["configured_executable"] = package
legacy["proxy"]["live_executable"] = package
legacy["refresh"]["configured_executable"] = package
verify.call(legacy, "legacy-or-retained", true)
verify.call(legacy, "retained", false)
%w[active_runtime_version pending_runtime_version].each do |key|
  invalid = copy.call
  invalid[key] = "0.34.98"
  verify.call(invalid, "retained", false)
end
invalid = copy.call
invalid["refresh"]["configured_executable"] = retained[0]
verify.call(invalid, "retained", false)
invalid = copy.call
invalid["proxy"]["live_executable"] = retained[0]
verify.call(invalid, "retained", false)
File.chmod(0o755, retained[0])
verify.call(status, "retained", false)
File.chmod(0o500, retained[0])
foreign = File.join(runtime_root, "a" * 64)
Dir.mkdir(foreign, 0o700)
FileUtils.cp(retained[0], File.join(foreign, "cq"))
File.chmod(0o500, File.join(foreign, "cq"))
invalid = copy.call
invalid["proxy"]["configured_executable"] = File.join(foreign, "cq")
verify.call(invalid, "retained", false)
puts "Homebrew release status gate legacy, retained, bootstrap, and rejection tests passed"
RUBY
