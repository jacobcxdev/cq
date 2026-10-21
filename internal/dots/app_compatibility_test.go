package dots

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestQualifiedAppPinRejectsUnknownBuildAndArchive(t *testing.T) {
	options, pin := installationFixture(t)
	got, err := qualifiedAppPin(options.App, []Settings{pin})
	if err != nil || got.ModuleAsset != pin.ModuleAsset || got.AppPath != options.App {
		t.Fatalf("qualified pin: %#v %v", got, err)
	}
	unknown := pin
	unknown.AppBuild = "unknown"
	if _, err := qualifiedAppPin(options.App, []Settings{unknown}); err == nil {
		t.Fatal("unknown build accepted")
	}
	if err := os.WriteFile(filepath.Join(options.App, "Contents", "Resources", "app.asar"), []byte("unreviewed archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := qualifiedAppPin(options.App, []Settings{pin}); err == nil {
		t.Fatal("changed archive accepted")
	}
}

func TestStatusRejectsUnqualifiedUpdateBeforeRendererAccess(t *testing.T) {
	c, s := controllerFixture(t)
	c.Native = nativeEvaluationFunc(func(context.Context, string) ([]any, error) {
		t.Fatal("unqualified update accessed renderer")
		return nil, nil
	})
	if err := os.WriteFile(filepath.Join(s.AppPath, "Contents", "Info.plist"), launcherPlist(map[string]string{"CFBundleShortVersionString": "unreviewed", "CFBundleVersion": "99999"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "build 99999") || !strings.Contains(err.Error(), "update CQ") {
		t.Fatalf("missing compatibility diagnostic: %v", err)
	}
}

func TestAppPinRefreshPreservesInstallationAndRequiresRelaunch(t *testing.T) {
	c, s := controllerFixture(t)
	updated := *s
	updated.ModuleAsset = "app-initial-updated.js"
	s.AppBuild = "previous"
	s.ModuleAsset = "app-initial-previous.js"
	if err := SaveJSON(filepath.Join(c.Root, "settings.json"), s); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(s.RuntimeRoot, "awareness", "installation.json")
	if err := SaveJSON(ledger, map[string]any{"installation_id": "existing-installation"}); err != nil {
		t.Fatal(err)
	}
	ledgerBefore, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.refreshAppPin(s, []Settings{updated}); err != nil {
		t.Fatal(err)
	}
	loaded, err := c.settings()
	want, _ := json.Marshal(updated)
	got, _ := json.Marshal(loaded)
	if err != nil || string(got) != string(want) {
		t.Fatalf("refreshed installation: %#v %v", loaded, err)
	}
	ledgerAfter, err := os.ReadFile(ledger)
	if err != nil || string(ledgerAfter) != string(ledgerBefore) {
		t.Fatalf("app update changed setup identity: %v", err)
	}
	launch := filepath.Join(s.RuntimeRoot, "launch.json")
	if err := SaveJSON(launch, map[string]any{"catalogue_revision": s.CatalogueRevision(), "asar_sha256": "previous archive"}); err != nil {
		t.Fatal(err)
	}
	if !c.restartRequired(s) {
		t.Fatal("previous renderer reused")
	}
	if err := SaveJSON(launch, map[string]any{"catalogue_revision": s.CatalogueRevision(), "asar_sha256": s.ASARSHA256}); err != nil {
		t.Fatal(err)
	}
	if c.restartRequired(s) {
		t.Fatal("matching renderer required another restart")
	}
	before, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
	if err := os.WriteFile(filepath.Join(s.AppPath, "Contents", "Resources", "app.asar"), []byte("unreviewed archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.refreshAppPin(s, []Settings{updated}); err == nil {
		t.Fatal("unreviewed app accepted")
	}
	after, _ := os.ReadFile(filepath.Join(c.Root, "settings.json"))
	if string(before) != string(after) {
		t.Fatal("failed qualification changed settings")
	}
}

func TestUpdatedNativeCacheUsesOwnerAtomsAndAPI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required for native JavaScript regression")
	}
	s := &Settings{IntegrationMode: "native", ModuleAsset: "app-initial-f3e281fb9a71.js", Dots: map[string]Dot{"one": {ProfileID: "profile-one"}}}
	var expression string
	e := nativeEvaluationFunc(func(_ context.Context, value string) ([]any, error) {
		expression = value
		return []any{map[string]any{"installed": true}}, nil
	})
	if _, err := InstallNativeRoomCache(context.Background(), s, e); err != nil {
		t.Fatal(err)
	}
	script := `const assert=require('node:assert/strict');
const account=Symbol(),user=Symbol(),ready=Symbol();
const scope={get:key=>new Map([[account,'account-one'],[user,'user-one'],[ready,true]]).get(key),watch:()=>{}};
class Cache {read() {}}
const cache=new Cache();cache.accountId='account-one';cache.userId='user-one';
const api={safeGet:async path=>{assert.equal(path,'/tbo');return {items:[{id:'profile-one',messaging_room_id:'room-one'}]};}};
const wrong=()=>{throw Error('old export used');};
const module={Ymr:wrong,GMr:wrong,XMr:wrong,JMr:wrong,Lpr:wrong,iRr:api,zRr:account,BRr:user,yLr:ready,B_r:value=>{assert.equal(value,scope);return cache;}};
globalThis.document={getElementById:id=>{assert.equal(id,'root');return {__reactContainer$fixture:{current:{memoizedProps:scope}}};}};
const expression=EXPRESSION.replace(/import\([^)]*\)/,'Promise.resolve(module)');
(async()=>{
const original=cache.read;
const result=await eval(expression);
assert.equal(result.installed,true);assert.notEqual(cache.read,original);
globalThis[Symbol.for('cq.dot-read-ownership.v2')].restore();
assert.equal(cache.read,original);
})().catch(error=>{console.error(error);process.exitCode=1;});`
	script = strings.Replace(script, "EXPRESSION", nativeJSON(expression), 1)
	if output, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("updated native cache: %v\n%s", err, output)
	}
}

func TestNativeAssetMapsUpdatedExportsWithoutChangingText(t *testing.T) {
	s := &Settings{ModuleAsset: "app-initial-f3e281fb9a71.js"}
	source := `const m=await import('app://-/assets/MODULE_ASSET_PLACEHOLDER');const text="m.Ymr m.GMr m.XMr m.JMr m.Lpr";`
	got := nativeAsset(source, s)
	for _, binding := range []string{"Ymr:m.iRr", "GMr:m.zRr", "XMr:m.BRr", "JMr:m.yLr", "Lpr:m.B_r"} {
		if !strings.Contains(got, binding) {
			t.Fatalf("updated renderer binding missing: %s", binding)
		}
	}
	if !strings.Contains(got, `const text="m.Ymr m.GMr m.XMr m.JMr m.Lpr"`) {
		t.Fatal("message text changed while adapting exports")
	}
	s.ModuleAsset = QualifiedModuleAsset
	if got := nativeAsset(source, s); strings.Contains(got, ".then(") {
		t.Fatal("legacy renderer exports changed")
	}
}

func TestUpdatedNativeExecutorAndBoardBindings(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required for native JavaScript regression")
	}
	s := &Settings{ModuleAsset: "app-initial-f3e281fb9a71.js", SystemAlias: "primary", Dots: map[string]Dot{"one": {OwnerAlias: "primary", ProfileID: "profile-one"}}}
	sources := map[string]string{}
	e := nativeEvaluationFunc(func(_ context.Context, expression string) ([]any, error) {
		if strings.Contains(expression, "const bus=") {
			sources["board"] = expression
			return boardValue(map[string]any{"thread": map[string]any{"id": testBoardThread}})
		}
		sources["executor"] = expression
		return []any{map[string]any{"ready": true, "enabled": true, "environment_id": "environment-one"}}, nil
	})
	if _, err := primaryExecutorEnvironment(context.Background(), s, e); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBoard(s, nil, e).metadata(context.Background(), testBoardThread); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sources["board"], "app-shared-60f4bb85435f.js") {
		t.Fatal("updated board used previous asset")
	}
	data, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	script := `const assert=require('node:assert/strict');
const sources=JSON.parse(require('node:fs').readFileSync(0,'utf8'));
const wrong=()=>{throw Error('old export used');};
const api={safeGet:async path=>{assert.equal(path,'/tbo/primary');return {profile:{id:'profile-one'}};}};
const executor={getState:async()=>({ready:true,enabled:true,environmentId:'environment-one'})};
let listener,unsubscribed=false;
const bus={subscribe:(kind,callback)=>{assert.equal(kind,'mcp-response');listener=callback;return()=>{unsubscribed=true;};},dispatchMessage:(kind,value)=>{assert.equal(kind,'mcp-request');assert.equal(value.hostId,'local');assert.equal(value.request.method,'thread/read');assert.equal(value.request.params.includeTurns,false);queueMicrotask(()=>listener({hostId:'local',message:{id:value.request.id,result:{thread:{id:value.request.params.threadId,status:{type:'idle'}}}}}));}};
globalThis.window={electronBridge:{getSharedObjectSnapshotValue:()=>({id:'local'})}};
const initial={Ymr:wrong,VAn:wrong,iRr:api,lPn:()=>executor};
const shared={gPt:wrong,MFt:bus};
(async()=>{
const state=await eval(sources.executor.replace(/import\([^)]*\)/,'Promise.resolve(initial)'));
assert.deepEqual(state,{ready:true,enabled:true,environment_id:'environment-one'});
const result=await eval(sources.board.replace(/import\([^)]*\)/,'Promise.resolve(shared)'));
assert.equal(result.result.thread.status.type,'idle');assert.equal(unsubscribed,true);
})().catch(error=>{console.error(error);process.exitCode=1;});`
	command := exec.Command(node, "-e", script)
	command.Stdin = strings.NewReader(string(data))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("updated executor and board: %v\n%s", err, output)
	}
}
