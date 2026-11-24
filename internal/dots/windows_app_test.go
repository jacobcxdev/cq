//go:build windows

package dots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func windowsAppFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "OpenAI.Codex_26.1120.5083.0_x64__2p2nqsd0c76g0")
	resources := filepath.Join(root, "app", "resources")
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `<Package xmlns:uap="http://schemas.microsoft.com/appx/manifest/uap/windows10"><Identity Name="OpenAI.Codex" ProcessorArchitecture="x64" Version="26.1120.5083.0" Publisher="CN=50BDFD77-8903-4850-9FFE-6E8522F64D5B"/><Applications><Application Id="App" Executable="app/ChatGPT.exe" EntryPoint="Windows.FullTrustApplication"><uap:VisualElements Square44x44Logo="assets/Square44x44Logo.png"/></Application></Applications></Package>`
	if err := os.WriteFile(filepath.Join(root, "AppxManifest.xml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "ChatGPT.exe"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeNativeArchiveFixture(t, filepath.Join(resources, "app.asar"), nativeDiscoveryFixture(t))
	return root
}

func TestWindowsAppPinAndChangeDetection(t *testing.T) {
	root := windowsAppFixture(t)
	pin, executable, err := QualifyWindowsApp(root)
	if err != nil {
		t.Fatal(err)
	}
	if executable != filepath.Join(root, "app", "ChatGPT.exe") || pin.AppPath != root || pin.AppVersion != "26.1120.5083.0" || pin.AppBuild != "5083" || pin.NativeBindings == nil || pin.NativeBindings.Bus == "" {
		t.Fatalf("unexpected Windows package pin: executable=%q pin=%#v", executable, pin)
	}
	if err := CheckWindowsApp(&pin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "resources", "app.asar"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckWindowsApp(&pin); err == nil || !strings.Contains(err.Error(), "renderer") {
		t.Fatalf("changed renderer accepted: %v", err)
	}
}

func TestWindowsNativeEvaluatorChecksQualifiedPackage(t *testing.T) {
	root := windowsAppFixture(t)
	pin, _, err := QualifyWindowsApp(root)
	if err != nil {
		t.Fatal(err)
	}
	evaluator := &nativeEvaluator{settings: &pin}
	if err := evaluator.check(); err != nil {
		t.Fatalf("qualified Windows renderer rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "resources", "app.asar"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := evaluator.check(); err == nil {
		t.Fatal("changed Windows renderer accepted by evaluator")
	}
}

func TestWindowsAppRejectsWrongIdentityAndTarget(t *testing.T) {
	for _, replace := range []struct{ old, new string }{
		{"OpenAI.Codex\"", "Other.Codex\""},
		{"ProcessorArchitecture=\"x64\"", "ProcessorArchitecture=\"x86\""},
		{"CN=50BDFD77-8903-4850-9FFE-6E8522F64D5B", "CN=SomeoneElse"},
		{"app/ChatGPT.exe", "app/Other.exe"},
		{"Windows.FullTrustApplication", "Windows.App"},
	} {
		root := windowsAppFixture(t)
		path := filepath.Join(root, "AppxManifest.xml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(data), replace.old, replace.new, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := QualifyWindowsApp(root); err == nil {
			t.Fatalf("invalid package accepted after replacing %q", replace.old)
		}
	}
}

func TestInstalledWindowsAppDiscovery(t *testing.T) {
	root := os.Getenv("CQ_TEST_WINDOWS_DOTS_PACKAGE")
	if root == "" {
		t.Skip("set CQ_TEST_WINDOWS_DOTS_PACKAGE to the registered package path for read-only qualification")
	}
	pin, executable, err := QualifyWindowsApp(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckWindowsApp(&pin); err != nil {
		t.Fatal(err)
	}
	t.Logf("qualified installed Windows renderer: executable=%s version=%s asset=%s", filepath.Base(executable), pin.AppVersion, pin.ModuleAsset)
}
