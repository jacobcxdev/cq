package dots

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRspackAdapterResolvedRegisteredExportsAndRejectedMissingModules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required for renderer adapter regression")
	}
	ref := NativeRspackExport{Module: "AccountModule", Export: "atom$"}
	b := &NativeRspackBindings{API: ref, Account: ref, User: ref, Ready: ref, Cache: ref, Executor: ref, ProfileQuery: ref, ProfileRecovery: ref, Bus: ref}
	expression := nativeRspackImport("Promise.resolve({__webpack_require__:requireModule})", b)
	script := `const assert=require('node:assert/strict');(async()=>{
const marker={owner:'expected'};let calls=0;function requireModule(id){assert.equal(id,'AccountModule');calls++;return {'atom$':marker};}requireModule.m={AccountModule:()=>{}};
const result=await (` + expression + `);assert.equal(result.Ymr,marker);assert.equal(result.CQBus,marker);assert.equal(result.CQNativeOwnedReads,true);assert.equal(calls,9);
delete requireModule.m.AccountModule;await assert.rejects(()=>` + expression + `,/not registered/);assert.equal(calls,9);
requireModule.m.AccountModule=()=>{};marker.owner='changed';const next=await (` + expression + `);assert.equal(next.GMr.owner,'changed');
})().catch(e=>{console.error(e);process.exit(1)});`
	if output, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("runtime adapter: %v\n%s", err, output)
	}
	s := &Settings{ModuleAsset: "123.abc.js", NativeBindings: &NativeBindings{Rspack: b}}
	if result := nativeAsset("import('app://-/assets/MODULE_ASSET_PLACEHOLDER')", s); !strings.Contains(result, "__webpack_require__") || !strings.Contains(result, "123.abc.js") {
		t.Fatal("runtime adapter not applied")
	}
}
