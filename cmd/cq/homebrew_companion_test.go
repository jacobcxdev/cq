package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func reviewedHomebrewNativePin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../.github/native-network-release.json")
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		t.Fatal(err)
	}
	return compact.String()
}

func TestHomebrewCompanionReconciliation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native Homebrew test")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Fatal("native Homebrew test requires brew")
	}
	data, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	block := strings.SplitN(string(data), "    custom_block: |\n", 2)[1]
	var lines []string
	for _, line := range strings.Split(block, "\n") {
		if line != "" && !strings.HasPrefix(line, "      ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "      "))
	}
	block = strings.ReplaceAll(strings.Join(lines, "\n"), "{{ .Version }}", "0.50.0")
	pinData, err := os.ReadFile("../../.github/native-network-release.json")
	if err != nil {
		t.Fatal(err)
	}
	var pin map[string]any
	if err := json.Unmarshal(pinData, &pin); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"current", "stale-enabled", "stale-disabled", "rollback", "restore-failed", "pin-mismatch", "pinned", "newer", "unsafe-agent", "missing", "macos12", "unsupported-dependency-callback", "unsupported-upgrade-callback"} {
		t.Run(scenario, func(t *testing.T) {
			selected := "0.41.9"
			if scenario == "current" {
				selected = pin["version"].(string)
			}
			fixturePin := make(map[string]any, len(pin))
			for key, value := range pin {
				fixturePin[key] = value
			}
			fixturePin["version"] = selected
			encodedPin, err := json.Marshal(fixturePin)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			parent := "cask \"cq\" do\nversion \"0.50.0\"\nsha256 :no_check\nurl \"https://example.invalid/cq.zip\"\n" + strings.ReplaceAll(block, "{{ .Env.CQ_NATIVE_NETWORK_PIN }}", string(encodedPin)) + "\nend\n"
			parentPath := filepath.Join(root, "cq.rb")
			if err := os.WriteFile(parentPath, []byte(parent), 0o600); err != nil {
				t.Fatal(err)
			}
			script := "CQ_FIXTURE_ROOT=" + strconv.Quote(root) + "\nCQ_FIXTURE_SCENARIO=" + strconv.Quote(scenario) + "\nCQ_FIXTURE_PIN=JSON.parse(" + strconv.Quote(string(encodedPin)) + ")\n" + homebrewCompanionFixture
			path := filepath.Join(root, "companion.rb")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(brew, "ruby", "-r", "json", path)
			command.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("companion reconciliation failed: %v\n%s", err, output)
			}
		})
	}
}

// Only network/download boundaries are synthetic. Homebrew loads the old
// unconditional-uninstall Cask from disk and owns artifact movement, backup,
// rollback, and the installed receipt inside this fixture's private Caskroom.
const homebrewCompanionFixture = `
require "cask/cask_loader"
require "cask/installer"
require "cask/upgrade"
require "fileutils"
CQFixtureEvents=[]
CQFixtureHome=Pathname(CQ_FIXTURE_ROOT)/"home"
CQFixtureApplications=Pathname(CQ_FIXTURE_ROOT)/"Applications"
CQFixtureAgent=CQFixtureHome/"Library/LaunchAgents/dev.jacobcx.cq.network.plist"
Dir.singleton_class.define_method(:home) { CQFixtureHome.to_s }
Object.send(:remove_const,:HOMEBREW_PREFIX)
HOMEBREW_PREFIX=Pathname(CQ_FIXTURE_ROOT)/"prefix"
Object.send(:remove_const,:HOMEBREW_CASKROOM)
HOMEBREW_CASKROOM=Pathname(CQ_FIXTURE_ROOT)/"Caskroom"
Cask::Caskroom.singleton_class.define_method(:path) { HOMEBREW_CASKROOM }
Cask::Quarantine.singleton_class.define_method(:available?) { false }
Cask::Upgrade.singleton_class.define_method(:reopen_apps_after_upgrade) { |*args| raise "companion upgrade reopened applications" }
SystemCommand.singleton_class.define_method(:sudo_available?) { false }
Homebrew::Services::System.singleton_class.define_method(:launchctl_find_service) { |*args,**kwargs| ["",false,:launchctl_print] }
FileUtils.mkdir_p([CQFixtureApplications,CQFixtureAgent.dirname,HOMEBREW_PREFIX/"bin"])
CQFixtureCQ=HOMEBREW_PREFIX/"bin/cq"
File.write(CQFixtureCQ,"fixture executable")
File.chmod(0700,CQFixtureCQ)
CQFixtureAgentXML=<<~XML
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>dev.jacobcx.cq.network</string><key>ProgramArguments</key><array><string>/Applications/CQNetwork.app/Contents/MacOS/CQNetwork</string><string>run</string><string>--cq</string><string>#{CQFixtureCQ}</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/></dict></plist>
XML
unless %w[stale-disabled current missing macos12].include?(CQ_FIXTURE_SCENARIO)
  File.write(CQFixtureAgent,CQFixtureAgentXML)
  File.chmod(CQ_FIXTURE_SCENARIO=="unsafe-agent" ? 0644 : 0600,CQFixtureAgent)
end
class CQFixtureResult
  def stdout = ""
  def stderr = ""
  def exit_status = 0
  def success? = true
  def assert_success! = self
end
SystemCommand.singleton_class.prepend(Module.new do
  def run(executable,**kwargs)
    executable=executable.to_s
    if executable=="/bin/launchctl"
      CQFixtureEvents << "launchctl"
      return CQFixtureResult.new
    elsif executable=="/usr/bin/xargs"
      kwargs.fetch(:input).split("\0").each do |path|
        raise "fixture deletion escaped its private root" unless path.start_with?(CQ_FIXTURE_ROOT+"/")
        FileUtils.rm_rf(path)
      end
      return CQFixtureResult.new
    elsif executable.end_with?("/CQNetwork") && kwargs[:args]==["disable"]
      CQFixtureEvents << "disable"
      return CQFixtureResult.new
    elsif executable==CQFixtureCQ.to_s
      raise "unexpected CQ command" unless kwargs[:args]==["codex","network","enable"]
      CQFixtureEvents << "enable"
      raise "fixture restoration failed" if CQ_FIXTURE_SCENARIO=="restore-failed"
      File.write(CQFixtureAgent,CQFixtureAgentXML)
      File.chmod(0600,CQFixtureAgent)
      return CQFixtureResult.new
    end
    super
  end
end)
def native_cask(version,directory,sha=CQ_FIXTURE_PIN.fetch("sha256"))
  path=Pathname(CQ_FIXTURE_ROOT)/directory/"cq-network.rb"
  path.dirname.mkpath
  File.write(path,<<~RUBY)
    cask "cq-network" do
      version #{version.inspect}
      sha256 #{sha.inspect}
      url "https://github.com/jacobcxdev/cq/releases/download/cq-network-v\#{version}/CQNetwork_\#{version}_universal.zip"
      app "CQNetwork.app"
      uninstall launchctl: "dev.jacobcx.cq.network",
                script: {executable: "\#{appdir}/CQNetwork.app/Contents/MacOS/CQNetwork",args: ["disable"]},
                delete: "~/Library/LaunchAgents/dev.jacobcx.cq.network.plist"
    end
  RUBY
  config=Cask::Config.new(explicit:{appdir:CQFixtureApplications})
  Cask::CaskLoader::FromPathLoader.new(path).load(config:config)
end
def stage_native(cask)
  app=cask.staged_path/"CQNetwork.app/Contents/MacOS"
  app.mkpath
  File.write(app/"CQNetwork",cask.version.to_s)
  File.chmod(0700,app/"CQNetwork")
end
Cask::Installer.prepend(Module.new do
  def prelude
    return if cask.token=="cq-network"
    super
  end
  def fetch(**kwargs)
    return if cask.token=="cq-network"
    super
  end
  def stage
    return super unless cask.token=="cq-network"
    CQFixtureEvents << "stage"
    stage_native(cask)
    save_caskfile
    raise "fixture install failed" if CQ_FIXTURE_SCENARIO=="rollback"
  end
end)
old_version=case CQ_FIXTURE_SCENARIO
when "newer" then "0.42.0"
when "current" then CQ_FIXTURE_PIN.fetch("version")
else "0.41.8"
end
unless CQ_FIXTURE_SCENARIO=="missing"
  old=native_cask(old_version,"old")
  stage_native(old)
  installer=Cask::Installer.new(old)
  installer.save_caskfile
  installer.install_artifacts
  Cask::Tab.create(old).write
end
sha=CQ_FIXTURE_SCENARIO=="pin-mismatch" ? "0"*64 : CQ_FIXTURE_PIN.fetch("sha256")
selected=native_cask(CQ_FIXTURE_PIN.fetch("version"),"selected",sha)
selected.define_singleton_method(:pinned?) { true } if CQ_FIXTURE_SCENARIO=="pinned"
if CQ_FIXTURE_SCENARIO=="unsupported-dependency-callback"
  Cask::Installer.class_eval { def satisfy_cask_and_formula_dependencies(unexpected:); raise "unexpected callback"; end }
elsif CQ_FIXTURE_SCENARIO=="unsupported-upgrade-callback"
  Cask::Upgrade.singleton_class.class_eval { def upgrade_cask(unexpected:); raise "unexpected callback"; end }
end
if CQ_FIXTURE_SCENARIO.start_with?("unsupported-")
  begin
    Cask::CaskLoader::FromPathLoader.new(Pathname(CQ_FIXTURE_ROOT)/"cq.rb").load(config:nil)
    raise "unsupported Homebrew callback accepted"
  rescue => error
    raise unless error.to_s.include?("CQ companion reconciliation requires")
  end
  raise "unsupported Homebrew mutated companion" unless CQFixtureEvents.empty?
  exit(0)
end
parent=nil
if CQ_FIXTURE_SCENARIO=="macos12"
  Homebrew::SimulateSystem.with(os: :monterey) do
    parent=Cask::CaskLoader::FromPathLoader.new(Pathname(CQ_FIXTURE_ROOT)/"cq.rb").load(config:nil)
    raise "CQ gained a native companion dependency on macOS12" unless parent.depends_on.cask.empty?
  end
else
  parent=Cask::CaskLoader::FromPathLoader.new(Pathname(CQ_FIXTURE_ROOT)/"cq.rb").load(config:nil)
end
raise "loading metadata mutated the companion" unless CQFixtureEvents.empty?
installer=Cask::Installer.new(parent)
installer.define_singleton_method(:cask_and_formula_dependencies) { CQ_FIXTURE_SCENARIO=="macos12" ? [] : [selected] }
failure=nil
begin
  installer.satisfy_cask_and_formula_dependencies
rescue => error
  failure=error
end
if %w[rollback restore-failed pin-mismatch pinned newer unsafe-agent].include?(CQ_FIXTURE_SCENARIO)
  raise "expected visible failure" unless failure
  expected={"rollback"=>"fixture install failed","restore-failed"=>"restoration failed","pin-mismatch"=>"reviewed","pinned"=>"pinned","newer"=>"newer","unsafe-agent"=>"private"}.fetch(CQ_FIXTURE_SCENARIO)
  raise "wrong failure: #{failure}" unless failure.to_s.include?(expected)
else
  raise failure if failure
end
if %w[current macos12].include?(CQ_FIXTURE_SCENARIO)
  raise "current companion was touched" unless CQFixtureEvents.empty?
elsif %w[pin-mismatch pinned newer unsafe-agent].include?(CQ_FIXTURE_SCENARIO)
  raise "rejected companion was mutated: #{CQFixtureEvents}" unless CQFixtureEvents.empty?
else
  raise "stale/missing companion was skipped" unless CQFixtureEvents.include?("stage")
  should_restore=%w[stale-enabled rollback restore-failed].include?(CQ_FIXTURE_SCENARIO)
  raise "routing restoration policy differs: #{CQFixtureEvents}" unless CQFixtureEvents.include?("enable")==should_restore
  if should_restore
    raise "old unconditional disable was skipped" unless CQFixtureEvents.include?("disable")
    raise "restore preceded uninstall" unless CQFixtureEvents.index("disable")<CQFixtureEvents.index("enable")
    raise "restored agent missing" if CQ_FIXTURE_SCENARIO!="restore-failed" && !CQFixtureAgent.exist?
  end
  expected_version=CQ_FIXTURE_SCENARIO=="rollback" ? old_version : selected.version.to_s
  metadata=(HOMEBREW_CASKROOM/"cq-network/.metadata"/expected_version).glob("**/cq-network.*").select { |path| %w[.rb .json].include?(path.extname) }
  raise "Homebrew receipt was not retained: #{metadata}" unless metadata.length==1
  installed=Cask::CaskLoader.load_from_installed_caskfile(metadata.first,api_fallback:false)
  raise "installed metadata differs" unless installed.version.to_s==expected_version
end
raise "stable CQ target was changed" unless File.read(CQFixtureCQ)=="fixture executable"
puts "verified #{CQ_FIXTURE_SCENARIO}: #{CQFixtureEvents.join(',')}"
`
