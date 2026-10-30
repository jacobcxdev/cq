package dots

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func nativeDiscoveryFixture(t *testing.T) map[string]string {
	t.Helper()
	var f struct {
		Account          string            `json:"account_atoms"`
		Ready            string            `json:"ready_atom"`
		Executor         string            `json:"executor_getter"`
		Cache            string            `json:"cache_factory"`
		Classes          string            `json:"cache_classes"`
		BusClass         string            `json:"bus_class"`
		RecoveryKey      string            `json:"profile_recovery_key"`
		RecoverySnapshot string            `json:"profile_recovery_snapshot"`
		RecoveryReader   string            `json:"profile_recovery_reader"`
		ProfileQuery     string            `json:"profile_query"`
		Exports          map[string]string `json:"exports"`
		BusExport        map[string]string `json:"bus_export"`
	}
	if err := json.Unmarshal([]byte(nativeCurrentContractJSON), &f); err != nil {
		t.Fatal(err)
	}
	exports := func(values map[string]string) string {
		var entries []string
		for alias, local := range values {
			entries = append(entries, local+" as "+alias)
		}
		sort.Strings(entries)
		return "export{" + strings.Join(entries, ",") + "};"
	}
	initial := strings.Join([]string{f.Account, f.Ready, f.Executor, f.Cache, f.Classes, f.RecoveryKey, f.RecoverySnapshot, f.RecoveryReader, f.ProfileQuery, "aht=class{safeGet(){}safePost(){}safePatch(){}};M_=new aht({getAdditionalHeaders:A_,getRequestSignal:jmt})", exports(f.Exports)}, ";")
	shared := f.BusClass + ";Vg=Bg.getInstance();" + exports(f.BusExport)
	return map[string]string{"app-initial-future.js": initial, "app-shared-future.js": shared}
}

func writeNativeArchiveFixture(t *testing.T, path string, modules map[string]string) {
	t.Helper()
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	assets := map[string]nativeArchiveEntry{}
	var content []byte
	for _, name := range names {
		assets[name] = nativeArchiveEntry{Offset: strconv.Itoa(len(content)), Size: int64(len(modules[name]))}
		content = append(content, modules[name]...)
	}
	root := nativeArchiveEntry{Files: map[string]nativeArchiveEntry{"webview": {Files: map[string]nativeArchiveEntry{"assets": {Files: assets}}}}}
	header, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	// Chromium Pickle: outer size, header buffer, string length, JSON, padding.
	padded := (len(header) + 3) &^ 3
	prefix := make([]byte, 16+padded)
	binary.LittleEndian.PutUint32(prefix, 4)
	binary.LittleEndian.PutUint32(prefix[4:], uint32(8+padded))
	binary.LittleEndian.PutUint32(prefix[8:], uint32(4+padded))
	binary.LittleEndian.PutUint32(prefix[12:], uint32(len(header)))
	copy(prefix[16:], header)
	if err := os.WriteFile(path, append(prefix, content...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeDiscoveryAcceptedRebuiltArchiveAndRenamedExports(t *testing.T) {
	options, _ := installationFixture(t)
	modules := nativeDiscoveryFixture(t)
	// Change every private local name and exported alias used by CQ. Semantic
	// fields and method contracts remain unchanged, as in a minifier rebuild.
	replacer := strings.NewReplacer("M_", "apiNext", "S_", "accountNext", "XWt", "cacheNext", "az()", "executorNext()", "az as", "executorNext as", "gWt", "cacheClassNext", "Bg", "busClassNext", "Vg", "busNext", "_lr", "apiExport", "Hlr", "accountExport", "Ulr", "userExport", "mcr", "readyExport", "g2n", "cacheExport", "Nen", "executorExport", "zRt", "busExport", "HC", "profileNext", "H_", "recoveryNext", "K0n", "profileExport", "Rcr", "recoveryExport", "jUt", "recoveryKeyNext", "xC", "recoverySnapshotNext")
	for name, source := range modules {
		modules[name] = replacer.Replace(source)
	}
	archive := filepath.Join(options.App, "Contents", "Resources", "app.asar")
	writeNativeArchiveFixture(t, archive, modules)
	if err := os.WriteFile(filepath.Join(options.App, "Contents", "Info.plist"), launcherPlist(map[string]string{"CFBundleShortVersionString": "future-version", "CFBundleVersion": "999999"}), 0o600); err != nil {
		t.Fatal(err)
	}
	pin, err := qualifiedAppPin(options.App, qualifiedAppPins)
	if err != nil {
		t.Fatal(err)
	}
	if pin.AppBuild != "999999" || pin.ModuleAsset != "app-initial-future.js" || pin.NativeBindings == nil || pin.NativeBindings.API != "apiExport" || pin.NativeBindings.Bus != "busExport" {
		t.Fatalf("discovered %#v", pin)
	}
	if err := AppCheck(&pin); err != nil {
		t.Fatal(err)
	}
	source := nativeAsset("import('app://-/assets/MODULE_ASSET_PLACEHOLDER')", &pin)
	if !strings.Contains(source, "Ymr:m.apiExport") || !strings.Contains(source, "CQNativeOwnedReads:true") {
		t.Fatalf("dynamic bindings not used: %s", source)
	}
	if !strings.Contains(source, "CQProfileQuery:m.profileExport") || !strings.Contains(source, "CQProfileRecovery:m.recoveryExport") {
		t.Fatal("renamed profile recovery bindings not used")
	}
	pin.NativeBindings.API = "malicious()"
	if err := pin.NativeBindings.validate(); err == nil {
		t.Fatal("non-identifier binding accepted")
	}
}

func TestNativeDiscoveryRejectedChangedAndAmbiguousContracts(t *testing.T) {
	for _, variant := range []string{"account", "API", "cache", "executor", "bus", "ambiguous initial", "ambiguous bus", "missing export", "recovery identity", "recovery key", "recovery snapshot", "profile query", "profile export"} {
		t.Run(variant, func(t *testing.T) {
			modules := nativeDiscoveryFixture(t)
			switch variant {
			case "account":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], ".authenticatedAccountId", ".otherAccount")
			case "API":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], "safePatch()", "otherPatch()")
			case "cache":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], "read(e,t,n,r,i,a)", "read(e,t,n,r,i)")
			case "executor":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], ".tppLocalExecutor", ".otherExecutor")
			case "bus":
				modules["app-shared-future.js"] = strings.ReplaceAll(modules["app-shared-future.js"], "dispatchMessage(", "otherDispatch(")
			case "ambiguous initial":
				modules["app-initial-other.js"] = modules["app-initial-future.js"]
			case "ambiguous bus":
				modules["app-shared-other.js"] = modules["app-shared-future.js"]
			case "missing export":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], "M_ as _lr", "other as _lr")
			case "recovery identity":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], "let t=e(S_),n=e($)", "let t=e(S_),n=e(S_)")
			case "recovery key":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], "JSON.stringify([t,n])", "JSON.stringify([n,t])")
			case "recovery snapshot":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], "entries:H_(t,{})", "entries:H_(other,{})")
			case "profile query":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], "`/tbo/{tbo_id}`", "`/other/{tbo_id}`")
			case "profile export":
				modules["app-initial-future.js"] = strings.ReplaceAll(modules["app-initial-future.js"], "HC as K0n", "other as K0n")
			}
			if _, _, err := resolveNativeBindings(modules); err == nil {
				t.Fatal("incompatible renderer accepted")
			}
		})
	}
}

func TestNativeProfileRecoveryBindingsValidated(t *testing.T) {
	_, bindings, err := resolveNativeBindings(nativeDiscoveryFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"", ""}, {"query", "reader"}, {"query", ""}, {"", "reader"}, {"query()", "reader"}, {"query", "reader()"}} {
		bindings.ProfileQuery, bindings.ProfileRecovery = pair[0], pair[1]
		valid := pair[0] == "" && pair[1] == "" || pair[0] == "query" && pair[1] == "reader"
		if (bindings.validate() == nil) != valid {
			t.Fatalf("unexpected validation for %q", pair)
		}
	}
}

func TestNativeArchiveRejectedInvalidBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.asar")
	writeNativeArchiveFixture(t, path, nativeDiscoveryFixture(t))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(data[12:], 32<<20)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readNativeModules(path); err == nil {
		t.Fatal("unbounded index accepted")
	}
}

func TestInstalledNativeDiscovery(t *testing.T) {
	app := os.Getenv("CQ_TEST_NATIVE_DISCOVERY_APP")
	if app == "" {
		t.Skip("set CQ_TEST_NATIVE_DISCOVERY_APP for read-only installed archive qualification")
	}
	pin, err := qualifiedAppPin(app, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pin.NativeBindings == nil {
		t.Fatal("installed app reused fixed bindings")
	}
	if err := AppCheck(&pin); err != nil {
		t.Fatal(err)
	}
	t.Logf("source contracts accepted: version=%s build=%s initial=%s shared=%s", pin.AppVersion, pin.AppBuild, pin.ModuleAsset, pin.NativeBindings.BusAsset)
}

func TestNativeDiscoveryRefreshPreservedAccountsAndBindings(t *testing.T) {
	c, s := controllerFixture(t)
	before, err := json.Marshal(s.Dots)
	if err != nil {
		t.Fatal(err)
	}
	writeNativeArchiveFixture(t, filepath.Join(s.AppPath, "Contents", "Resources", "app.asar"), nativeDiscoveryFixture(t))
	if err := c.refreshAppPin(s, qualifiedAppPins); err != nil {
		t.Fatal(err)
	}
	loaded, err := c.settings()
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(loaded.Dots)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || loaded.NativeBindings == nil || loaded.NativeBindings.BusAsset != "app-shared-future.js" || loaded.NativeBindings.ProfileQuery == "" || loaded.NativeBindings.ProfileRecovery == "" {
		t.Fatalf("refresh lost accounts or bindings: %#v", loaded)
	}
	// Previously discovered settings must gain recovery without an app update.
	loaded.NativeBindings.ProfileQuery, loaded.NativeBindings.ProfileRecovery = "", ""
	if err := SaveJSON(filepath.Join(c.Root, "settings.json"), loaded); err != nil {
		t.Fatal(err)
	}
	if err := AppCheck(loaded); err != nil {
		t.Fatal(err)
	}
	if err := c.refreshAppPin(loaded, qualifiedAppPins); err != nil {
		t.Fatal(err)
	}
	loaded, err = c.settings()
	if err != nil || loaded.NativeBindings == nil || loaded.NativeBindings.ProfileQuery == "" || loaded.NativeBindings.ProfileRecovery == "" {
		t.Fatalf("old discovery bindings were not upgraded: %v", err)
	}
	after, err = json.Marshal(loaded.Dots)
	if err != nil || string(before) != string(after) {
		t.Fatal("binding migration changed configured Dots")
	}
	var expression string
	e := nativeEvaluationFunc(func(_ context.Context, value string) ([]any, error) {
		expression = value
		return boardValue(map[string]any{"thread": map[string]any{"id": testBoardThread}})
	})
	if _, err := NewBoard(loaded, nil, e).metadata(context.Background(), testBoardThread); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expression, "app-shared-future.js") || !strings.Contains(expression, ").zRt") {
		t.Fatal("board ignored discovered bindings")
	}
}
