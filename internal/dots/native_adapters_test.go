package dots

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

//go:embed native_room_cache_test.js
var nativeRoomCacheRegressionJS string

//go:embed native_profile_test.js
var nativeProfileRegressionJS string

type nativeEvaluationFunc func(context.Context, string) ([]any, error)

func (f nativeEvaluationFunc) Evaluate(ctx context.Context, s string) (any, error) {
	values, err := f(ctx, s)
	if err != nil {
		return nil, err
	}
	return values[0], nil
}
func (f nativeEvaluationFunc) EvaluateAll(ctx context.Context, s string) ([]any, error) {
	return f(ctx, s)
}
func TestNativeRoomCacheSharedReadLifetimeRegression(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required for native JavaScript regression")
	}
	script := strings.ReplaceAll(nativeRoomCacheRegressionJS, "GUARD", nativeRoomCacheGuardJS)
	if data, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("native JS regression: %v\n%s", err, data)
	}
}
func TestNativeRoomCachePinsProfilesAndAvoidsWrites(t *testing.T) {
	settings := &Settings{IntegrationMode: "native", ModuleAsset: "app-shared-fixture.js", Dots: map[string]Dot{"one": {ProfileID: "profile-one"}, "two": {ProfileID: "profile-two"}}}
	calls := 0
	e := nativeEvaluationFunc(func(_ context.Context, expr string) ([]any, error) {
		calls++
		if !strings.Contains(expr, settings.ModuleAsset) || !strings.Contains(expr, `["profile-one","profile-two"]`) || strings.Contains(expr, "safePost") || strings.Contains(expr, "safePatch") || strings.Contains(expr, "GUARD") {
			t.Fatal("expression did not retain pinned read-only scope")
		}
		return []any{map[string]any{"installed": true, "ownedReads": float64(2)}, map[string]any{"installed": false}}, nil
	})
	result, err := InstallNativeRoomCache(context.Background(), settings, e)
	if err != nil || result["windows"] != 1 || result["ownedReads"] != float64(2) {
		t.Fatalf("install: %#v %v", result, err)
	}
	settings.IntegrationMode = "isolated"
	result, err = InstallNativeRoomCache(context.Background(), settings, e)
	if err != nil || result["installed"] != false || calls != 1 {
		t.Fatal("isolated renderer injected")
	}
}
func TestNativeDisplayNamesBindProfilesAndRejectDuplicateRows(t *testing.T) {
	s := &Settings{Dots: map[string]Dot{"one": {ProfileID: "a"}, "two": {ProfileID: "b"}}}
	rows := []any{map[string]any{"id": "a", "name": "Renamed"}, map[string]any{"id": "b", "name": "Renamed"}}
	e := nativeEvaluationFunc(func(context.Context, string) ([]any, error) { return []any{rows}, nil })
	names, err := NativeDisplayNames(context.Background(), s, e)
	if err != nil || names["one"] != "Renamed" || names["two"] != "Renamed" {
		t.Fatal("duplicate names lost stable handles")
	}
	rows = []any{rows[0], rows[0]}
	if _, err = NativeDisplayNames(context.Background(), s, e); err == nil {
		t.Fatal("duplicate profile accepted")
	}
	rows = []any{map[string]any{"id": "a", "name": "bad\nname"}, map[string]any{"id": "b", "name": "ok"}}
	if _, err = NativeDisplayNames(context.Background(), s, e); err == nil {
		t.Fatal("control in name accepted")
	}
}
func TestNativeSendUsesBoundTargetAndPrivateLedger(t *testing.T) {
	s := &Settings{SystemAlias: "system", ModuleAsset: "app-shared-fixture.js", Dots: map[string]Dot{"one": {OwnerAlias: "system", ProfileID: "profile-one", RootThreadID: "root-one"}, "two": {OwnerAlias: "secondary", ProfileID: "profile-two", RootThreadID: "root-two"}}}
	calls := 0
	e := nativeEvaluationFunc(func(_ context.Context, expression string) ([]any, error) {
		calls++
		for _, part := range []string{`"profile":"profile-two"`, `"text":"synthetic"`, `primary.profile?.id!=="profile-one"`} {
			if !strings.Contains(expression, part) {
				t.Fatalf("expression missing %s", part)
			}
		}
		return []any{map[string]any{"accepted": true, "message_id": "synthetic-message"}}, nil
	})
	ledgerRoot := t.TempDir()
	if err := os.Chmod(ledgerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(ledgerRoot, "ledger.json")
	result, err := NativeSend(context.Background(), s, e, "two", "synthetic", ledger)
	if err != nil || result["outcome"] != "accepted" || result["target"] != "two" {
		t.Fatalf("send: %#v %v", result, err)
	}
	info, err := os.Stat(ledger)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("ledger not private")
	}
	data, _ := os.ReadFile(ledger)
	var stored map[string]any
	if json.Unmarshal(data, &stored) != nil || stored["request_id"] != result["request_id"] {
		t.Fatal("ledger not updated")
	}
	if _, err = NativeSend(context.Background(), s, e, "unknown", "synthetic", ledger+"unknown"); err == nil || calls != 1 {
		t.Fatal("unknown target evaluated")
	}
	delete(s.Dots, "one")
	if _, err = NativeSend(context.Background(), s, e, "two", "synthetic", ledger+"nosystem"); err == nil || calls != 1 {
		t.Fatal("missing native identity evaluated")
	}
}

func TestNativeAdaptersFollowStableProfilesAcrossRootRotation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required for native JavaScript regression")
	}
	for _, asset := range []string{QualifiedModuleAsset, "app-initial-f3e281fb9a71.js"} {
		t.Run(asset, func(t *testing.T) {
			testNativeAdaptersRootRotation(t, node, asset)
		})
	}
}

func testNativeAdaptersRootRotation(t *testing.T, node, asset string) {
	t.Helper()
	s := &Settings{ModuleAsset: asset}
	sources := map[string]string{
		"send":  fmt.Sprintf(nativeSendJS, nativeJSON(map[string]any{"profile": "profile-two", "text": "synthetic", "request": "request-id"}), nativeJSON("profile-one")),
		"relay": strings.NewReplacer("PROFILE_ID", nativeJSON("profile-two"), "RELAY_PATTERN", nativeJSON(`^\[CQ:directory:[a-f0-9]{32}\]\ndirectory\n\[/CQ\]$`)).Replace(relayReadJS),
		"reply": fmt.Sprintf(nativeMatchingReplyJS, nativeJSON("profile-two"), nativeJSON("excluded"), nativeJSON("directory")),
	}
	for key, source := range sources {
		sources[key] = nativeAsset(source, s)
	}
	data, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "-e", nativeProfileRegressionJS)
	command.Stdin = strings.NewReader(string(data))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native profile regression: %v\n%s", err, output)
	}
}

func TestNativeEvaluatorRechecksChangedArchiveBeforeRendererAccess(t *testing.T) {
	_, s := installationFixture(t)
	e := &nativeEvaluator{settings: &s}
	if err := e.check(); err != nil {
		t.Fatal(err)
	}
	if err := e.check(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(s.AppPath, "Contents", "Resources", "app.asar")
	if err := os.WriteFile(archive, []byte("changed renderer fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.check(); err == nil {
		t.Fatal("changed renderer accepted")
	}
}

func TestNativeStatusWorkspaceWarningIgnoresConversationText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required for native DOM regression")
	}
	environment := os.Environ()
	c, _ := controllerFixture(t)
	var expression string
	c.Native = nativeEvaluationFunc(func(_ context.Context, value string) ([]any, error) {
		expression = value
		return []any{controllerReadyStatus()}, nil
	})
	if _, err = c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	script := `const assert=require('node:assert/strict');
const original=EXPRESSION;
const expression=original.replace(/await import\([^)]*\)/,"({Ymr:{safeGet:async()=>({items:[],profile:{id:'synthetic'},selection:{available:true}})}})");
assert.notEqual(expression,original);
let headings=[];
globalThis.document={body:{innerText:"Activity summary: couldn't load workspace settings"},querySelector:()=>null,querySelectorAll:selector=>{assert.equal(selector,'aside[role="status"][aria-live="polite"] h3');return headings.map(textContent=>({textContent}));}};
(async()=>{
assert.equal((await eval(expression)).workspaceError,false,'conversation text must not block readiness');
for(const title of ["Couldn’t load workspace settings","Couldn't load workspace settings","  Couldn’t load workspace settings  "]){headings=[title];assert.equal((await eval(expression)).workspaceError,true,'native warning must block readiness');}
headings=["Activity summary: Couldn't load workspace settings"];
assert.equal((await eval(expression)).workspaceError,false,'title must match the native warning exactly');
})().catch(e=>{console.error(e);process.exitCode=1;});`
	script = strings.Replace(script, "EXPRESSION", nativeJSON(expression), 1)
	command := exec.Command(node, "-e", script)
	command.Env = environment
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native DOM regression: %v\n%s", err, data)
	}
}
