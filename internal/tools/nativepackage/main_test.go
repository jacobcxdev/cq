package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testManifest() manifest {
	return manifest{1, "0.41.0", strings.Repeat("a", 40), 123, strings.Repeat("b", 64), "13.0", nil}
}

func TestNativeProviderDaemonReadScope(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "native", "macos-network", "Packaging", "Provider.entitlements"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var rules []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil {
			t.Fatal(err)
		}
		if key != "com.apple.security.temporary-exception.sbpl" {
			continue
		}
		var value struct {
			XMLName xml.Name `xml:"array"`
			Rules   []string `xml:"string"`
		}
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		rules = append(rules, value.Rules...)
	}
	if len(rules) != 1 {
		t.Fatalf("provider must contain one narrow daemon read rule, got %d", len(rules))
	}
	parts := regexp.MustCompile(`^\(allow file-read\* \(regex #"([^"]+)"\)\)$`).FindStringSubmatch(rules[0])
	if len(parts) != 2 {
		t.Fatalf("daemon exception must grant only file-read* for one regex: %q", rules[0])
	}
	scope, err := regexp.Compile(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		want bool
	}{
		{"/Users/alice/.codex/packages/app-server-daemon/releases/0.160.1-aarch64-apple-darwin/bin/codex", true},
		{"/Users/bob/.codex/packages/app-server-daemon/releases/0.166.0-x86_64-apple-darwin/bin/codex", true},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/v/bin/./codex", true},
		{"/Users/alice/.codex/auth.json", false},
		{"/Users/alice/.codex/packages/app-server-daemon/auth.json", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/v/bin/auth.json", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/v/bin/codex.sig", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/v/bin/codex/child", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/v/codex", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/v/nested/bin/codex", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases-other/v/bin/codex", false},
		{"/Users/alice/.codex/packages/other/releases/v/bin/codex", false},
		{"/Users/alice/custom-codex/packages/app-server-daemon/releases/v/bin/codex", false},
		{"/Volumes/Homes/alice/.codex/packages/app-server-daemon/releases/v/bin/codex", false},
		{"/var/root/.codex/packages/app-server-daemon/releases/v/bin/codex", false},
		{"/Users/nested/alice/.codex/packages/app-server-daemon/releases/v/bin/codex", false},
		{"/prefix/Users/alice/.codex/packages/app-server-daemon/releases/v/bin/codex", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/v/bin/codex\n", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/../bin/codex", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/./bin/codex", false},
		{"/Users/alice/.codex/packages/app-server-daemon/releases/v/bin/codex/../auth.json", false},
		{"/Users/../.codex/packages/app-server-daemon/releases/v/bin/codex", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			// Sandbox path filters see canonical paths, not unresolved traversal.
			if got := scope.MatchString(path.Clean(test.path)); got != test.want {
				t.Fatalf("daemon read scope matched %q = %t, want %t", test.path, got, test.want)
			}
		})
	}
}

func TestNativeManifestPinAndClosedSchema(t *testing.T) {
	m := testManifest()
	var output bytes.Buffer
	if err := run([]string{"pin", m.Version, m.SourceCommit, "123", m.SHA256, m.MinimumMacOS}, &output); err != nil {
		t.Fatal(err)
	}
	var got manifest
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != m {
		t.Fatalf("pin %+v", got)
	}
	file := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(file, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var info bytes.Buffer
	if err := run([]string{"info", file}, &info); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(info.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["source_commit"] != m.SourceCommit || fields["asset_id"] != float64(m.AssetID) {
		t.Fatalf("pin identity lost in info: %s", info.String())
	}
	for _, change := range []func(*manifest){
		func(m *manifest) { m.SchemaVersion = 3 }, func(m *manifest) { m.Version = "0.41.0/../bad" }, func(m *manifest) { m.SourceCommit = "main" },
		func(m *manifest) { m.AssetID = 0 }, func(m *manifest) { m.SHA256 = "TODO" }, func(m *manifest) { m.MinimumMacOS = `13.0"; system("bad")` },
		func(m *manifest) { m.MinimumMacOS = "14.0" }, func(m *manifest) { m.MinimumMacOS = "12.0" },
	} {
		bad := m
		change(&bad)
		if err := bad.validate(); err == nil {
			t.Fatalf("invalid manifest accepted %+v", bad)
		}
	}
	for _, data := range []string{strings.TrimSpace(output.String()) + ` {}`, strings.Replace(output.String(), `"schema_version": 1`, `"unknown": true, "schema_version": 1`, 1)} {
		file := filepath.Join(t.TempDir(), "manifest.json")
		if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run([]string{"info", file}, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid manifest JSON accepted")
		}
	}
}

func TestNativeParentPinRetainsNativeIdentityAndSharedVersion(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"pin-parent", "0.47.1", strings.Repeat("a", 40), "123", strings.Repeat("b", 64), "13.0"}, &output); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 6 || fields["schema_version"] != float64(2) || fields["version"] != "0.47.1" || fields["asset_id"] != float64(123) {
		t.Fatalf("parent pin lost its closed native identity: %s", output.String())
	}
	file := filepath.Join(t.TempDir(), "parent.json")
	if err := os.WriteFile(file, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run([]string{"info", file}, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["schema_version"] != float64(2) || fields["tag"] != "v0.47.1" || fields["staging_tag"] != "cq-native-stage-v0.47.1" || fields["url"] != "https://github.com/jacobcxdev/cq/releases/download/v0.47.1/CQNetwork_0.47.1_universal.zip" {
		t.Fatalf("parent release identity differs: %s", output.String())
	}
}

func mockNativeSourceGit(t *testing.T, source, target string) {
	t.Helper()
	directory := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  "merge-base --is-ancestor $NATIVE_SOURCE $NATIVE_TARGET") exit "${NATIVE_ANCESTRY_ERROR:-0}" ;;
  "rev-parse --verify $NATIVE_SOURCE:native/macos-network") echo cccccccccccccccccccccccccccccccccccccccc ;;
  "rev-parse --verify $NATIVE_TARGET:native/macos-network") echo "${NATIVE_TARGET_TREE:-cccccccccccccccccccccccccccccccccccccccc}" ;;
  "rev-parse --verify refs/tags/v0.41.0^{commit}") echo "${NATIVE_TAG_TARGET:-$NATIVE_TARGET}" ;;
  *) echo "unexpected git operation" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(directory, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NATIVE_SOURCE", source)
	t.Setenv("NATIVE_TARGET", target)
}

func TestNativeParentReleaseRequiresStableMatchingDescendant(t *testing.T) {
	m := testManifest()
	m.SchemaVersion = 2
	target := strings.Repeat("d", 40)
	mockNativeSourceGit(t, m.SourceCommit, target)
	fixture := func(change func(map[string]any)) string {
		return releaseFixture(t, m, func(r map[string]any) {
			r["target_commitish"], r["prerelease"] = target, false
			if change != nil {
				change(r)
			}
		})
	}
	if err := verifyRelease(m, fixture(nil), false); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]any){
		func(r map[string]any) { r["prerelease"] = true },
		func(r map[string]any) { r["target_commitish"] = "main" },
		func(r map[string]any) { r["tag_name"] = "cq-network-v0.41.0" },
		func(r map[string]any) { r["draft"] = true },
	} {
		if err := verifyRelease(m, fixture(change), false); err == nil {
			t.Fatal("invalid parent release accepted")
		}
	}
	if err := verifyRelease(m, fixture(func(r map[string]any) { r["draft"] = true }), true); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []struct{ key, value string }{
		{"NATIVE_ANCESTRY_ERROR", "1"},
		{"NATIVE_TARGET_TREE", strings.Repeat("e", 40)},
		{"NATIVE_TAG_TARGET", strings.Repeat("f", 40)},
	} {
		t.Run(failure.key, func(t *testing.T) {
			t.Setenv(failure.key, failure.value)
			if err := verifyRelease(m, fixture(nil), false); err == nil {
				t.Fatal("unproven signed source accepted")
			}
		})
	}
}

func TestNativeParentStagingDraftRequiresExactNativeSource(t *testing.T) {
	m := testManifest()
	m.SchemaVersion = 2
	mockNativeSourceGit(t, m.SourceCommit, m.SourceCommit)
	fixture := func(change func(map[string]any)) string {
		return releaseFixture(t, m, func(r map[string]any) {
			r["tag_name"], r["draft"], r["prerelease"] = "cq-native-stage-v0.41.0", true, false
			r["assets"].([]any)[0].(map[string]any)["browser_download_url"] = "https://github.com/jacobcxdev/cq/releases/download/untagged-123/" + m.asset()
			if change != nil {
				change(r)
			}
		})
	}
	if err := verifyRelease(m, fixture(nil), true); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(m, fixture(nil), false); err == nil {
		t.Fatal("staging draft passed publication gate")
	}
	for _, change := range []func(map[string]any){
		func(r map[string]any) { r["tag_name"] = "another-draft" },
		func(r map[string]any) { r["target_commitish"] = strings.Repeat("d", 40) },
		func(r map[string]any) { r["draft"] = false },
		func(r map[string]any) { r["prerelease"] = true },
	} {
		if err := verifyRelease(m, fixture(change), true); err == nil {
			t.Fatal("changed staging identity accepted")
		}
	}
}

func TestNativePreparationResolvesOnlyConfirmedAbsentParentStaging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS preparation requires Bash")
	}
	source, err := os.ReadFile("../../../.github/scripts/prepare-native-network.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, gate, ok := strings.Cut(string(source), "release_api=$(gh release view")
	if !ok {
		t.Fatal("release resolution missing")
	}
	gate = "release_api=$(gh release view" + gate
	gate, _, ok = strings.Cut(gate, `if [[ ! "$release_api"`)
	if !ok {
		t.Fatal("release API boundary missing")
	}
	directory := t.TempDir()
	fake := `#!/bin/sh
case "$1:$2:$3" in
  release:view:v0.41.0) [ "$RESOLVE_MODE" = canonical ] && { echo https://api.github.com/repos/jacobcxdev/cq/releases/123; exit 0; }; exit 1 ;;
  api:--include:repos/jacobcxdev/cq/releases/tags/v0.41.0) printf 'HTTP/2.0 %s\n' "$RESOLVE_CODE"; [ "$RESOLVE_CODE" = 200 ] && exit 0; exit 1 ;;
  release:view:cq-native-stage-v0.41.0) echo staging >> "$RESOLVE_CALLS"; echo https://api.github.com/repos/jacobcxdev/cq/releases/456 ;;
  *) exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(directory, "gh"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	shells := []string{"/bin/bash"}
	if bash, err := exec.LookPath("bash"); err == nil && bash != "/bin/bash" {
		shells = append(shells, bash)
	}
	for _, shell := range shells {
		for _, tc := range []struct {
			name, mode, code, schema, flag string
			want                           bool
			stage                          bool
		}{
			{"canonical", "canonical", "404", "2", "--allow-draft", true, false},
			{"fixed staging", "missing", "404", "2", "--allow-draft", true, true},
			{"forbidden", "missing", "403", "2", "--allow-draft", false, false},
			{"network failure", "missing", "", "2", "--allow-draft", false, false},
			{"canonical exists", "missing", "200", "2", "--allow-draft", false, false},
			{"published only", "missing", "404", "2", "", false, false},
			{"legacy pin", "missing", "404", "1", "--allow-draft", false, false},
		} {
			t.Run(shell+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				if err := os.WriteFile(filepath.Join(root, "info.json"), []byte(`{"staging_tag":"cq-native-stage-v0.41.0"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				calls := filepath.Join(root, "calls")
				command := exec.Command(shell, "-c", "set -euo pipefail\n"+gate+"\nprintf '%s' \"$release_api\"", "test", "pin", "out", tc.flag)
				command.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"), "temporary="+root, "tag=v0.41.0", "schema_version="+tc.schema, "RESOLVE_MODE="+tc.mode, "RESOLVE_CODE="+tc.code, "RESOLVE_CALLS="+calls)
				output, err := command.CombinedOutput()
				if (err == nil) != tc.want {
					t.Fatalf("resolution error=%v: %s", err, output)
				}
				_, statErr := os.Stat(calls)
				if (statErr == nil) != tc.stage {
					t.Fatalf("staging lookup=%t, want %t", statErr == nil, tc.stage)
				}
			})
		}
	}
}

func parentBundles(m manifest) *installBundles {
	return &installBundles{
		ARM64: installArchive{"https://github.com/jacobcxdev/cq/releases/download/v" + m.Version + "/cq_" + m.Version + "_darwin_arm64_bundle.tar.gz", strings.Repeat("c", 64)},
		AMD64: installArchive{"https://github.com/jacobcxdev/cq/releases/download/v" + m.Version + "/cq_" + m.Version + "_darwin_amd64_bundle.tar.gz", strings.Repeat("d", 64)},
	}
}

func writeNativeManifest(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "pin.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestNativeParentCaskAndInfoSelectBoundedInstallBundles(t *testing.T) {
	m := testManifest()
	m.SchemaVersion = 2
	m.Bundles = parentBundles(m)
	file := writeNativeManifest(t, m)
	var output bytes.Buffer
	if err := run([]string{"info", file}, &output); err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if err := json.Unmarshal(output.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	want := m.Bundles.ARM64
	if runtime.GOARCH == "amd64" {
		want = m.Bundles.AMD64
	}
	if info["install_url"] != want.URL || info["install_sha256"] != want.SHA256 || info["url"] != m.url() || info["asset_id"] != float64(m.AssetID) {
		t.Fatalf("mixed base and install identities: %s", output.String())
	}
	m.Bundles = nil
	cask := filepath.Join(t.TempDir(), "cq-network.rb")
	if err := run([]string{"cask", writeNativeManifest(t, m), cask, "--bundles", writeNativeManifest(t, parentBundles(m))}, &output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cask)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`arch arm: "arm64", intel: "amd64"`, strings.Repeat("c", 64), strings.Repeat("d", 64), `/v#{version}/cq_#{version}_darwin_#{arch}_bundle.tar.gz`, `app "CQNetwork.app"`} {
		if !strings.Contains(string(data), value) {
			t.Fatalf("bundled cask omitted %q", value)
		}
	}
	for _, change := range []func(*manifest){
		func(m *manifest) { m.SchemaVersion = 1 },
		func(m *manifest) { m.Bundles.ARM64.URL = "https://example.invalid/bundle.tar.gz" },
		func(m *manifest) {
			m.Bundles.AMD64.URL = strings.Replace(m.Bundles.AMD64.URL, "/v0.41.0/", "/v0.41.1/", 1)
		},
		func(m *manifest) { m.Bundles.ARM64.SHA256 = "TODO" },
		func(m *manifest) { m.Bundles.AMD64 = installArchive{} },
	} {
		bad := m
		bad.Bundles = parentBundles(m)
		change(&bad)
		if err := run([]string{"info", writeNativeManifest(t, bad)}, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid install bundle accepted")
		}
	}
	for _, bad := range []string{
		strings.Replace(string(mustNativeJSON(t, parentBundles(m))), `"arm64":`, `"extra":{},"arm64":`, 1),
		strings.Replace(string(mustNativeJSON(t, parentBundles(m))), `"arm64":`, `"ARM64":`, 1),
		`null`,
	} {
		badFile := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(badFile, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run([]string{"cask", writeNativeManifest(t, m), cask, "--bundles", badFile}, &bytes.Buffer{}); err == nil {
			t.Fatal("unknown or absent bundle architecture accepted")
		}
	}
}

func mustNativeJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func installTarFixture(t *testing.T, entries ...*tar.Header) []byte {
	return installTarChangedFixture(t, nil, entries...)
}

func installTarChangedFixture(t *testing.T, change func(*tar.Header), entries ...*tar.Header) []byte {
	t.Helper()
	var data bytes.Buffer
	compressed := gzip.NewWriter(&data)
	archive := tar.NewWriter(compressed)
	for _, name := range []string{"cq", "CQNetwork.app/Contents/Info.plist", "CQNetwork.app/Contents/MacOS/CQNetwork", "CQNetwork.app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/Info.plist", "CQNetwork.app/Contents/Library/SystemExtensions/dev.jacobcx.cq.network.proxy.systemextension/Contents/MacOS/CQNetworkProxy"} {
		header := &tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeReg, Size: 1}
		if change != nil {
			change(header)
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := archive.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, entry := range entries {
		if err := archive.WriteHeader(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func parentBundleReleaseFixture(t *testing.T, m manifest, target string, change func(map[string]any)) string {
	t.Helper()
	return releaseFixture(t, m, func(r map[string]any) {
		r["target_commitish"], r["prerelease"] = target, false
		for i, arch := range []string{"arm64", "amd64"} {
			bundle := m.Bundles.ARM64
			if arch == "amd64" {
				bundle = m.Bundles.AMD64
			}
			r["assets"] = append(r["assets"].([]any), map[string]any{"id": 234 + i, "name": "cq_" + m.Version + "_darwin_" + arch + "_bundle.tar.gz", "digest": "sha256:" + bundle.SHA256, "state": "uploaded", "browser_download_url": bundle.URL})
		}
		if change != nil {
			change(r)
		}
	})
}

func TestNativeInstallBundleVerifiesArchiveAndPublishedIdentity(t *testing.T) {
	m := testManifest()
	m.SchemaVersion = 2
	m.Bundles = parentBundles(m)
	target := strings.Repeat("e", 40)
	mockNativeSourceGit(t, m.SourceCommit, target)
	data := installTarFixture(t)
	digest := sha256.Sum256(data)
	if runtime.GOARCH == "arm64" {
		m.Bundles.ARM64.SHA256 = hex.EncodeToString(digest[:])
	} else {
		m.Bundles.AMD64.SHA256 = hex.EncodeToString(digest[:])
	}
	file := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	pin := writeNativeManifest(t, m)
	release := parentBundleReleaseFixture(t, m, target, nil)
	if err := run([]string{"verify-install", pin, file, release}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]any){
		func(r map[string]any) { r["assets"] = r["assets"].([]any)[:1] },
		func(r map[string]any) { r["assets"] = append(r["assets"].([]any), r["assets"].([]any)[1]) },
		func(r map[string]any) {
			r["assets"].([]any)[1].(map[string]any)["digest"] = "sha256:" + strings.Repeat("f", 64)
		},
		func(r map[string]any) {
			r["assets"].([]any)[1].(map[string]any)["browser_download_url"] = "https://example.invalid/bundle.tar.gz"
		},
		func(r map[string]any) { r["assets"].([]any)[1].(map[string]any)["state"] = "new" },
		func(r map[string]any) { r["draft"] = true },
	} {
		if err := run([]string{"verify-install", pin, file, parentBundleReleaseFixture(t, m, target, change)}, &bytes.Buffer{}); err == nil {
			t.Fatal("changed installation release accepted")
		}
	}
	for _, invalid := range [][]byte{
		append(append([]byte(nil), data...), data...),
		installTarFixture(t, &tar.Header{Name: "../outside", Mode: 0o600, Typeflag: tar.TypeReg}),
		installTarFixture(t, &tar.Header{Name: "CQNetwork.app/escape", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "../../outside"}),
		installTarFixture(t, &tar.Header{Name: "cq", Mode: 0o755, Typeflag: tar.TypeReg}),
		installTarFixture(t, &tar.Header{Name: "CQNetwork.app/hard", Typeflag: tar.TypeLink, Linkname: "cq"}),
		installTarFixture(t, &tar.Header{Name: "CQNetwork.app/empty", Typeflag: tar.TypeSymlink, Linkname: ""}),
		installTarChangedFixture(t, func(h *tar.Header) {
			if h.Name == "CQNetwork.app/Contents/Info.plist" {
				h.Typeflag, h.Size = tar.TypeDir, 0
			}
		}),
	} {
		digest := sha256.Sum256(invalid)
		if runtime.GOARCH == "arm64" {
			m.Bundles.ARM64.SHA256 = hex.EncodeToString(digest[:])
		} else {
			m.Bundles.AMD64.SHA256 = hex.EncodeToString(digest[:])
		}
		if err := os.WriteFile(file, invalid, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := run([]string{"verify-install", writeNativeManifest(t, m), file, parentBundleReleaseFixture(t, m, target, nil)}, &bytes.Buffer{}); err == nil {
			t.Fatal("unsafe or concatenated install archive accepted")
		}
	}
}

func releaseFixture(t *testing.T, m manifest, change func(map[string]any)) string {
	t.Helper()
	value := map[string]any{"tag_name": m.tag(), "target_commitish": m.SourceCommit, "draft": false, "prerelease": true, "assets": []any{map[string]any{"id": m.AssetID, "name": m.asset(), "digest": "sha256:" + m.SHA256, "state": "uploaded", "browser_download_url": m.url()}}, "additional_github_field": "ignored"}
	if change != nil {
		change(value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestNativeReleaseRequiresExactPublishedAsset(t *testing.T) {
	m := testManifest()
	if err := verifyRelease(m, releaseFixture(t, m, nil), false); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]any){
		func(r map[string]any) { r["target_commitish"] = "main" }, func(r map[string]any) { r["tag_name"] = "v0.41.0" }, func(r map[string]any) { r["draft"] = true }, func(r map[string]any) { r["prerelease"] = false },
		func(r map[string]any) { r["assets"] = []any{} }, func(r map[string]any) { r["assets"] = append(r["assets"].([]any), r["assets"].([]any)[0]) },
		func(r map[string]any) { r["assets"].([]any)[0].(map[string]any)["id"] = 124 }, func(r map[string]any) {
			r["assets"].([]any)[0].(map[string]any)["digest"] = "sha256:" + strings.Repeat("c", 64)
		},
		func(r map[string]any) {
			r["assets"].([]any)[0].(map[string]any)["browser_download_url"] = "https://example.invalid/asset.zip"
		},
	} {
		if err := verifyRelease(m, releaseFixture(t, m, change), false); err == nil {
			t.Fatal("changed native release accepted")
		}
	}
}

func TestNativeReleaseDraftAllowedOnlyForStaging(t *testing.T) {
	m := testManifest()
	draftURL := "https://github.com/" + repository + "/releases/download/untagged-de565141bccfa7e0646a/" + m.asset()
	file := releaseFixture(t, m, func(release map[string]any) {
		release["draft"] = true
		release["assets"].([]any)[0].(map[string]any)["browser_download_url"] = draftURL
	})
	if err := verifyRelease(m, file, true); err != nil {
		t.Fatal(err)
	}
	if err := verifyRelease(m, file, false); err == nil {
		t.Fatal("draft native asset passed publication gate")
	}
	file = releaseFixture(t, m, func(release map[string]any) {
		release["assets"].([]any)[0].(map[string]any)["browser_download_url"] = draftURL
	})
	if err := verifyRelease(m, file, true); err == nil {
		t.Fatal("untagged URL accepted for a published release")
	}
	for _, change := range []func(map[string]any){
		func(release map[string]any) { release["target_commitish"] = "main" },
		func(release map[string]any) { release["tag_name"] = "another-tag" },
		func(release map[string]any) { release["assets"].([]any)[0].(map[string]any)["id"] = 124 },
		func(release map[string]any) {
			release["assets"].([]any)[0].(map[string]any)["digest"] = "sha256:" + strings.Repeat("c", 64)
		},
		func(release map[string]any) {
			release["assets"].([]any)[0].(map[string]any)["browser_download_url"] = "https://example.invalid/" + m.asset()
		},
	} {
		file = releaseFixture(t, m, func(release map[string]any) {
			release["draft"] = true
			release["assets"].([]any)[0].(map[string]any)["browser_download_url"] = draftURL
			change(release)
		})
		if err := verifyRelease(m, file, true); err == nil {
			t.Fatal("changed draft asset identity accepted")
		}
	}
}

type archiveEntry struct {
	name, body string
	mode       os.FileMode
}

func archiveFixture(t *testing.T, extra ...archiveEntry) (manifest, string) {
	t.Helper()
	return archiveFixtureWithProvider(t, "dev.jacobcx.cq.network.proxy", extra...)
}

func archiveFixtureWithProvider(t *testing.T, provider string, extra ...archiveEntry) (manifest, string) {
	t.Helper()
	providerRoot := "CQNetwork.app/Contents/Library/SystemExtensions/" + provider + ".systemextension"
	entries := []archiveEntry{
		{"CQNetwork.app/Contents/Info.plist", "plist", 0o644},
		{"CQNetwork.app/Contents/MacOS/CQNetwork", "host", 0o755},
		{providerRoot + "/Contents/Info.plist", "plist", 0o644},
		{providerRoot + "/Contents/MacOS/CQNetworkProxy", "provider", 0o755},
	}
	entries = append(entries, extra...)
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name}
		header.SetMode(entry.mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "native.zip")
	if err := os.WriteFile(file, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testManifest()
	digest := sha256.Sum256(data.Bytes())
	m.SHA256 = hex.EncodeToString(digest[:])
	return m, file
}

func TestNativeArchiveRejectsUnmatchedProviderFilename(t *testing.T) {
	m, file := archiveFixtureWithProvider(t, "CQNetworkProxy")
	if err := verifyArchive(m, file); err == nil {
		t.Fatal("provider filename that differs from its identifier was accepted")
	}
}

func TestNativeArchiveValidatesDigestAndExtractionBoundary(t *testing.T) {
	m, file := archiveFixture(t, archiveEntry{"CQNetwork.app/Contents/host-link", "MacOS/CQNetwork", os.ModeSymlink | 0o777})
	if err := verifyArchive(m, file); err != nil {
		t.Fatal(err)
	}
	m.SHA256 = strings.Repeat("d", 64)
	if err := verifyArchive(m, file); err == nil {
		t.Fatal("wrong digest accepted")
	}
	for _, entry := range []archiveEntry{
		{"../outside", "bad", 0o644}, {"CQNetwork.app/../../outside", "bad", 0o644}, {"/outside", "bad", 0o644}, {"other.app/Contents/Info.plist", "bad", 0o644},
		{"CQNetwork.app/Contents/Info.plist", "duplicate", 0o644}, {"CQNetwork.app/Contents/escape", "../../../outside", os.ModeSymlink | 0o777},
		{"CQNetwork.app/Contents/escape", "/tmp/outside", os.ModeSymlink | 0o777}, {"CQNetwork.app/Contents/escape", strings.Repeat("x", 1025), os.ModeSymlink | 0o777},
	} {
		m, file := archiveFixture(t, entry)
		if err := verifyArchive(m, file); err == nil {
			t.Fatalf("unsafe entry accepted %s", entry.name)
		}
	}
}

func TestNativeArchiveRejectsEntriesBeneathSymlinks(t *testing.T) {
	entries := []archiveEntry{
		{"CQNetwork.app/Contents/subdir/x", "../../A", os.ModeSymlink | 0o777},
		{"CQNetwork.app/Contents/subdir/y", "x/../../../outside", os.ModeSymlink | 0o777},
		{"CQNetwork.app/Contents/subdir/y/file", "must not escape", 0o644},
	}
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "ancestor first", true: "descendant first"}[reverse], func(t *testing.T) {
			ordered := append([]archiveEntry(nil), entries...)
			if reverse {
				ordered[0], ordered[2] = ordered[2], ordered[0]
			}
			m, file := archiveFixture(t, ordered...)
			if err := verifyArchive(m, file); err == nil {
				t.Fatal("entry beneath composed symlink accepted")
			}
		})
	}
}

func TestNativeCaskPinsIndependentVersionWithoutActivation(t *testing.T) {
	m := testManifest()
	text := renderCask(m)
	for _, value := range []string{`cask "cq-network" do`, `version "0.41.0"`, m.SHA256, `url "https://github.com/jacobcxdev/cq/releases/download/cq-network-v#{version}/CQNetwork_#{version}_universal.zip"`, `app "CQNetwork.app"`, `depends_on macos: :ventura`, `args:       ["disable"]`, `launchctl: "dev.jacobcx.cq.network"`} {
		if !strings.Contains(text, value) {
			t.Fatalf("cask missing %q", value)
		}
	}
	for _, value := range []string{"installer script:", "postflight", "codesign --sign", "notarytool submit"} {
		if strings.Contains(text, value) {
			t.Fatalf("cask activates or signs during installation: %s", value)
		}
	}
}

func TestNativeCaskParsesWithHomebrew(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native companion cask is validated on macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is not installed")
	}
	// Retain the fixture filename during evaluation: Homebrew's URL audit reads
	// its source line to detect version interpolation. No tap or install is needed.
	script := `file = ARGV.fetch(0)
Homebrew::SimulateSystem.with(arch: ARGV.fetch(2).to_sym) do
loader = Cask::CaskLoader::FromContentLoader.new(File.read(file))
cask = loader.instance_eval(File.read(file), file)
abort "resolved URL differs from pinned release" unless cask.url.to_s == ARGV.fetch(1)
abort "resolved SHA differs from pinned release" unless cask.sha256.to_s == ARGV.fetch(3)
requirement = cask.depends_on.macos
abort "wrong macOS requirement" unless requirement.comparator == ">=" && requirement.minimum_version == MacOSVersion.new("13.0")
uninstall = cask.artifacts.grep(Cask::Artifact::Uninstall).fetch(0)
abort "wrong uninstall directive order" unless uninstall.directives.keys.take(3) == [:launchctl, :script, :delete]
errors = Cask::Auditor.audit(cask, audit_strict: true, any_named_args: true)
abort "strict cask audit failed" unless errors.empty?
end`
	for _, kind := range []string{"legacy", "parent ZIP", "parent bundle"} {
		for _, arch := range []string{"arm", "intel"} {
			t.Run(kind+"/"+arch, func(t *testing.T) {
				m := testManifest()
				if kind != "legacy" {
					m.SchemaVersion = 2
				}
				wantURL := m.url()
				wantSHA := m.SHA256
				if kind == "parent bundle" {
					m.Bundles = parentBundles(m)
					wantURL = m.Bundles.ARM64.URL
					wantSHA = m.Bundles.ARM64.SHA256
					if arch == "intel" {
						wantURL = m.Bundles.AMD64.URL
						wantSHA = m.Bundles.AMD64.SHA256
					}
				}
				file := filepath.Join(t.TempDir(), "cq-network.rb")
				if err := os.WriteFile(file, []byte(renderCask(m)), 0o600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, brew, "ruby", "-r", "cask/cask_loader", "-r", "cask/auditor", "-e", script, file, wantURL, arch, wantSHA)
				command.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1")
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("Homebrew rejected generated cask: %v\n%s", err, output)
				}
			})
		}
	}
}

func TestNativeArchiveRequiresHostAndProvider(t *testing.T) {
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "empty.zip")
	if err := os.WriteFile(file, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	m := testManifest()
	digest := sha256.Sum256(data.Bytes())
	m.SHA256 = hex.EncodeToString(digest[:])
	if err := verifyArchive(m, file); err == nil {
		t.Fatal("archive without host and provider accepted")
	}
}
