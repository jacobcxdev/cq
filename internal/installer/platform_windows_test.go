package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/installstate"
)

const testWindowsInstallRoot = `C:\Users\Test\AppData\Local\Programs\cq`

func TestWindowsMetadataInstallAddsPATHAndARP(t *testing.T) {
	metadata, registry, files := newWindowsMetadataHarness()
	registry.values[windowsEnvironmentKey] = map[string]windowsRegistryValue{
		"Path": {Kind: windowsRegistryExpandString, String: `C:\Windows\System32`},
	}
	installation := testWindowsInstallation()

	if err := metadata.Install(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	path := registry.values[windowsEnvironmentKey]["Path"]
	if path.Kind != windowsRegistryExpandString || path.String != `C:\Windows\System32;`+testWindowsInstallRoot {
		t.Fatalf("PATH = %#v", path)
	}
	assertWindowsARP(t, registry, installation, 1)
	if string(files.files[windowsJoin(testWindowsInstallRoot, "cq-install.exe")]) != "installer" {
		t.Fatal("durable installer copy missing")
	}
	wantScript := renderWindowsUninstallScript(testWindowsInstallRoot)
	if string(files.files[windowsJoin(testWindowsInstallRoot, "uninstall.cmd")]) != wantScript {
		t.Fatalf("uninstall script = %q", files.files[windowsJoin(testWindowsInstallRoot, "uninstall.cmd")])
	}
	if registry.broadcasts != 1 {
		t.Fatalf("PATH broadcasts = %d", registry.broadcasts)
	}
	if err := metadata.Inspect(context.Background(), installation); err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
}

func TestWindowsMetadataPreservesPreExistingPATHEntry(t *testing.T) {
	metadata, registry, _ := newWindowsMetadataHarness()
	registry.values[windowsEnvironmentKey] = map[string]windowsRegistryValue{
		"Path": {Kind: windowsRegistryString, String: strings.ToLower(testWindowsInstallRoot) + `;C:\Windows`},
	}
	installation := testWindowsInstallation()

	if err := metadata.Install(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	assertWindowsARP(t, registry, installation, 0)
	if registry.broadcasts != 0 {
		t.Fatalf("unexpected PATH broadcast count = %d", registry.broadcasts)
	}
	if err := metadata.Remove(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	if got := registry.values[windowsEnvironmentKey]["Path"].String; got != strings.ToLower(testWindowsInstallRoot)+`;C:\Windows` {
		t.Fatalf("pre-existing PATH changed to %q", got)
	}
}

func TestWindowsMetadataRemoveDeletesOnlyInstallerAddedPATHEntry(t *testing.T) {
	metadata, registry, files := newWindowsMetadataHarness()
	registry.values[windowsEnvironmentKey] = map[string]windowsRegistryValue{
		"Path": {Kind: windowsRegistryString, String: `C:\Windows`},
	}
	installation := testWindowsInstallation()
	if err := metadata.Install(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	registry.values[windowsEnvironmentKey]["Path"] = windowsRegistryValue{
		Kind:   windowsRegistryString,
		String: `C:\Windows;` + testWindowsInstallRoot + `;C:\UserAdded`,
	}

	if err := metadata.Remove(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	if got := registry.values[windowsEnvironmentKey]["Path"].String; got != `C:\Windows;C:\UserAdded` {
		t.Fatalf("PATH after remove = %q", got)
	}
	if _, exists := registry.values[windowsUninstallKey]; exists {
		t.Fatal("ARP key remains")
	}
	if string(files.files[windowsJoin(testWindowsInstallRoot, "cq-install.exe")]) != "installer" || len(files.files[windowsJoin(testWindowsInstallRoot, "uninstall.cmd")]) == 0 {
		t.Fatal("metadata removal deleted durable cleanup files")
	}
	if registry.broadcasts != 2 {
		t.Fatalf("PATH broadcasts = %d", registry.broadcasts)
	}
	if err := metadata.Remove(context.Background(), installation); err != nil {
		t.Fatalf("repeated Remove() error = %v", err)
	}
}

func TestWindowsMetadataUpgradeKeepsPATHOwnership(t *testing.T) {
	metadata, registry, _ := newWindowsMetadataHarness()
	registry.values[windowsEnvironmentKey] = map[string]windowsRegistryValue{
		"Path": {Kind: windowsRegistryString, String: `C:\Windows`},
	}
	installation := testWindowsInstallation()
	if err := metadata.Install(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	if err := metadata.Install(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	assertWindowsARP(t, registry, installation, 1)
	if strings.Count(registry.values[windowsEnvironmentKey]["Path"].String, testWindowsInstallRoot) != 1 {
		t.Fatalf("duplicate PATH = %q", registry.values[windowsEnvironmentKey]["Path"].String)
	}
}

func TestWindowsMetadataRejectsWrongOwnerBeforeMutation(t *testing.T) {
	metadata, registry, files := newWindowsMetadataHarness()
	registry.values[windowsUninstallKey] = map[string]windowsRegistryValue{
		"Publisher":       {Kind: windowsRegistryString, String: "other"},
		"InstallLocation": {Kind: windowsRegistryString, String: testWindowsInstallRoot},
	}
	beforeRegistry := registry.cloneValues()
	beforeFiles := files.clone()

	err := metadata.Install(context.Background(), testWindowsInstallation())
	if !errors.Is(err, installstate.ErrOwnershipConflict) {
		t.Fatalf("Install() error = %v", err)
	}
	if !reflect.DeepEqual(registry.values, beforeRegistry) || !reflect.DeepEqual(files.files, beforeFiles) {
		t.Fatal("wrong-owner install mutated metadata")
	}
}

func TestWindowsMetadataRollsBackPATHFilesAndARP(t *testing.T) {
	metadata, registry, files := newWindowsMetadataHarness()
	registry.values[windowsEnvironmentKey] = map[string]windowsRegistryValue{
		"Path": {Kind: windowsRegistryString, String: `C:\Windows`},
	}
	registry.failSetKey = windowsUninstallKey
	registry.failSetName = "DisplayVersion"

	err := metadata.Install(context.Background(), testWindowsInstallation())
	if err == nil {
		t.Fatal("Install() succeeded")
	}
	if got := registry.values[windowsEnvironmentKey]["Path"].String; got != `C:\Windows` {
		t.Fatalf("PATH rollback = %q", got)
	}
	if _, exists := registry.values[windowsUninstallKey]; exists {
		t.Fatal("failed ARP key remains")
	}
	if _, exists := files.files[windowsJoin(testWindowsInstallRoot, "cq-install.exe")]; exists {
		t.Fatal("failed durable installer remains")
	}
	if _, exists := files.files[windowsJoin(testWindowsInstallRoot, "uninstall.cmd")]; exists {
		t.Fatal("failed uninstall script remains")
	}
}

func TestWindowsUninstallScriptMatchesGoldenFile(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "uninstall.cmd.golden"))
	if err != nil {
		t.Fatal(err)
	}
	want = []byte(strings.ReplaceAll(strings.ReplaceAll(string(want), "\r\n", "\n"), "\n", "\r\n"))
	if got := renderWindowsUninstallScript(testWindowsInstallRoot); got != string(want) {
		t.Fatalf("uninstall script differs\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestWindowsInstallRootUsesLocalAppData(t *testing.T) {
	root, err := WindowsInstallRoot(`C:\Users\Test\AppData\Local`)
	if err != nil {
		t.Fatal(err)
	}
	if root != testWindowsInstallRoot {
		t.Fatalf("root = %q", root)
	}
	for _, invalid := range []string{"", "relative", `C:\Users\Test\AppData\Local\..\Roaming`, `C:\Users\Test\Bad"Name`} {
		if _, err := WindowsInstallRoot(invalid); err == nil {
			t.Fatalf("WindowsInstallRoot(%q) succeeded", invalid)
		}
	}
}

func testWindowsInstallation() Installation {
	return Installation{
		Owner:        installstate.OwnerWinGet,
		Version:      "0.27.0",
		Executable:   windowsJoin(testWindowsInstallRoot, "cq.exe"),
		BinaryDigest: strings.Repeat("0", 64),
		Services:     []string{`\cq\Proxy`, `\cq\Refresh`},
	}
}

func newWindowsMetadataHarness() (*WindowsMetadata, *fakeWindowsRegistry, *fakeWindowsMetadataFiles) {
	registry := &fakeWindowsRegistry{values: map[string]map[string]windowsRegistryValue{}}
	files := &fakeWindowsMetadataFiles{files: map[string][]byte{`C:\Temp\cq-install.exe`: []byte("installer")}}
	metadata := &WindowsMetadata{
		Root:            testWindowsInstallRoot,
		SourceInstaller: `C:\Temp\cq-install.exe`,
		Registry:        registry,
		Files:           files,
		Broadcast:       registry.BroadcastEnvironment,
	}
	return metadata, registry, files
}

func assertWindowsARP(t *testing.T, registry *fakeWindowsRegistry, installation Installation, pathAdded uint32) {
	t.Helper()
	values := registry.values[windowsUninstallKey]
	wantStrings := map[string]string{
		"DisplayName":          "CQ",
		"DisplayVersion":       installation.Version,
		"Publisher":            "jacobcxdev",
		"InstallLocation":      testWindowsInstallRoot,
		"UninstallString":      `"` + windowsJoin(testWindowsInstallRoot, "uninstall.cmd") + `"`,
		"QuietUninstallString": `"` + windowsJoin(testWindowsInstallRoot, "uninstall.cmd") + `"`,
	}
	for name, want := range wantStrings {
		if got := values[name]; got.Kind != windowsRegistryString || got.String != want {
			t.Fatalf("ARP %s = %#v, want %q", name, got, want)
		}
	}
	for name, want := range map[string]uint32{"NoModify": 1, "NoRepair": 1, windowsPathAddedValue: pathAdded} {
		if got := values[name]; got.Kind != windowsRegistryDWORD || got.DWORD != want {
			t.Fatalf("ARP %s = %#v, want %d", name, got, want)
		}
	}
}

type fakeWindowsRegistry struct {
	values      map[string]map[string]windowsRegistryValue
	failSetKey  string
	failSetName string
	broadcasts  int
}

func (registry *fakeWindowsRegistry) Get(key, name string) (windowsRegistryValue, bool, error) {
	value, exists := registry.values[key][name]
	return value, exists, nil
}

func (registry *fakeWindowsRegistry) Values(key string) (map[string]windowsRegistryValue, bool, error) {
	values, exists := registry.values[key]
	if !exists {
		return nil, false, nil
	}
	clone := make(map[string]windowsRegistryValue, len(values))
	for name, value := range values {
		clone[name] = value
	}
	return clone, true, nil
}

func (registry *fakeWindowsRegistry) Set(key, name string, value windowsRegistryValue) error {
	if key == registry.failSetKey && name == registry.failSetName {
		registry.failSetKey = ""
		registry.failSetName = ""
		return errors.New("injected registry failure")
	}
	if registry.values[key] == nil {
		registry.values[key] = map[string]windowsRegistryValue{}
	}
	registry.values[key][name] = value
	return nil
}

func (registry *fakeWindowsRegistry) DeleteValue(key, name string) error {
	delete(registry.values[key], name)
	return nil
}

func (registry *fakeWindowsRegistry) DeleteKey(key string) error {
	delete(registry.values, key)
	return nil
}

func (registry *fakeWindowsRegistry) BroadcastEnvironment() error {
	registry.broadcasts++
	return nil
}

func (registry *fakeWindowsRegistry) cloneValues() map[string]map[string]windowsRegistryValue {
	clone := make(map[string]map[string]windowsRegistryValue, len(registry.values))
	for key, values := range registry.values {
		clone[key] = make(map[string]windowsRegistryValue, len(values))
		for name, value := range values {
			clone[key][name] = value
		}
	}
	return clone
}

type fakeWindowsMetadataFiles struct {
	files map[string][]byte
}

func (files *fakeWindowsMetadataFiles) Read(path string, limit int64) ([]byte, error) {
	body, exists := files.files[path]
	if !exists {
		return nil, os.ErrNotExist
	}
	if int64(len(body)) > limit {
		return nil, errors.New("file too large")
	}
	return append([]byte(nil), body...), nil
}

func (files *fakeWindowsMetadataFiles) Write(path string, body []byte, _ os.FileMode) error {
	files.files[path] = append([]byte(nil), body...)
	return nil
}

func (files *fakeWindowsMetadataFiles) Remove(path string) error {
	if _, exists := files.files[path]; !exists {
		return os.ErrNotExist
	}
	delete(files.files, path)
	return nil
}

func (files *fakeWindowsMetadataFiles) MkdirAll(string, os.FileMode) error { return nil }

func (files *fakeWindowsMetadataFiles) Exists(path string) (bool, error) {
	_, exists := files.files[path]
	return exists, nil
}

func (files *fakeWindowsMetadataFiles) clone() map[string][]byte {
	clone := make(map[string][]byte, len(files.files))
	for path, body := range files.files {
		clone[path] = append([]byte(nil), body...)
	}
	return clone
}
