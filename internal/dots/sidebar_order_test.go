package dots

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProbeAppliedPersistedSidebarOrderWithoutChangingOwners(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	path := filepath.Join(filepath.Dir(p.settings.RuntimeRoot), "sidebar-order.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"handles":["two","one"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"items":[{"id":"profile-system","active_root_thread_id":"root-system","messaging_room_id":"room-system"}],"cursor":null}`)
	data, err := p.transform(probeRequest("GET", "/backend-api/tbo", ""), probeSuccess(""), body, nil, p.fixture, 0, false, true)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := probeObject(data)
	rows := value["items"].([]any)
	if rows[0].(map[string]any)["id"] != "profile-owner" || rows[1].(map[string]any)["id"] != "profile-system" {
		t.Fatalf("wrong sidebar order: %s", data)
	}
	if p.fixture.Profiles[0]["id"] != "profile-system" || p.fixture.Owners["profile-owner"].Alias != "bbbbbbbb" {
		t.Fatal("presentation order changed fixture or ownership")
	}
}

func TestSidebarOrderResolvedNamesAndPreservedUnlistedProfiles(t *testing.T) {
	c, s := controllerFixture(t)
	c.Native = nativeEvaluationFunc(func(context.Context, string) ([]any, error) { return nil, os.ErrNotExist })
	value, err := c.Execute(context.Background(), Request{Operation: "order", Target: `["null","Pingy"]`})
	if err != nil || value["saved"] != true {
		t.Fatalf("order save: %v %v", value, err)
	}
	var order sidebarOrder
	if err := coordinationRead(filepath.Join(c.Root, "sidebar-order.json"), &order); err != nil {
		t.Fatal(err)
	}
	want := []string{StableHandle("fixture-secondary", "profile-fixture-secondary"), StableHandle("fixture-owner", "profile-fixture-owner")}
	if !reflect.DeepEqual(order.Handles, want) {
		t.Fatalf("order %v", order.Handles)
	}
	before, _ := json.Marshal(s.Dots)
	profiles := []map[string]any{{"id": "unlisted"}, {"id": "profile-fixture-owner"}, {"id": "profile-fixture-secondary"}, {"id": "another-unlisted"}}
	got, err := orderedSidebarProfiles(s, profiles)
	if err != nil {
		t.Fatal(err)
	}
	if got[0]["id"] != "profile-fixture-secondary" || got[1]["id"] != "profile-fixture-owner" || got[2]["id"] != "unlisted" || got[3]["id"] != "another-unlisted" {
		t.Fatalf("unlisted ordering: %v", got)
	}
	after, _ := json.Marshal(s.Dots)
	if string(before) != string(after) {
		t.Fatal("order changed account bindings")
	}
}

func TestSidebarOrderRejectedInvalidOrAmbiguousInputsWithoutWriting(t *testing.T) {
	for _, refs := range []string{`[]`, `["missing"]`, `["Pingy","Pingy"]`, `["Pingy",17]`, `["duplicate"]`} {
		t.Run(refs, func(t *testing.T) {
			c, s := controllerFixture(t)
			if strings.Contains(refs, "duplicate") {
				for h, d := range s.Dots {
					d.DisplayName = "duplicate"
					s.Dots[h] = d
				}
				if err := SaveJSON(filepath.Join(c.Root, "settings.json"), s); err != nil {
					t.Fatal(err)
				}
			}
			c.Native = nativeEvaluationFunc(func(context.Context, string) ([]any, error) { return nil, os.ErrNotExist })
			if _, err := c.Execute(context.Background(), Request{Operation: "order", Target: refs}); err == nil {
				t.Fatal("invalid order accepted")
			}
			if _, err := os.Stat(filepath.Join(c.Root, "sidebar-order.json")); !os.IsNotExist(err) {
				t.Fatal("invalid order persisted")
			}
		})
	}
}

func TestSidebarOrderInvalidStateDidNotHideCatalogue(t *testing.T) {
	p, _, _ := probeFixtureForTest(t)
	path := filepath.Join(filepath.Dir(p.settings.RuntimeRoot), "sidebar-order.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"handles":["two"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"items":[{"id":"profile-system","active_root_thread_id":"root-system","messaging_room_id":"room-system"}],"cursor":null}`)
	data, err := p.transform(probeRequest("GET", "/backend-api/tbo", ""), probeSuccess(""), body, nil, p.fixture, 0, false, true)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := probeObject(data)
	if len(value["items"].([]any)) != 2 {
		t.Fatal("optional order error hid Dots")
	}
}

func TestNativeSidebarOrderRefreshScopedCurrentAccountWithoutRestart(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node required for native ordering regression")
	}
	source := nativeAsset(strings.Replace(nativeSidebarOrderJS, "ORDERED_PROFILE_IDS", `["profile-owner","profile-other"]`, 1), &Settings{ModuleAsset: QualifiedModuleAsset})
	script := `const assert=require('node:assert/strict');
const ns={GMr:'account',XMr:'user',JMr:'ready'};let current='owner',rows=[{id:'profile-other'},{id:'profile-owner'},{id:'unlisted'}],listener,subscriptions=0,writes=0,stops=0;
const key=['cloud-aeon-profiles','owner','viewer'];
const client={getQueryData:k=>{assert.deepEqual(k,key);return rows;},setQueryData(k,value){assert.deepEqual(k,key);writes++;rows=value;listener?.({query:{queryKey:key}});},getQueryCache:()=>({subscribe(callback){listener=callback;subscriptions++;return()=>{listener=null;stops++;};}})};
const scope={watch(){},get:atom=>atom==='account'?current:atom==='user'?'viewer':true,queryClient:client};
globalThis.document={getElementById:()=>({__reactContainer$test:{current:{memoizedProps:scope}}})};
const source=SOURCE,run=(s=source)=>eval(s.replace(/import\([^)]*\)/,'Promise.resolve(ns)'));
(async()=>{
assert.deepEqual(await run(),{refreshed:true,order_applied:true});assert.deepEqual(rows.map(p=>p.id),['profile-owner','profile-other','unlisted']);assert.equal(writes,1);
assert.deepEqual(await run(),{refreshed:true,order_applied:true});assert.equal(writes,1);assert.equal(subscriptions,1);
rows=[rows[1],rows[0],rows[2]];listener({query:{queryKey:['cloud-aeon-profiles','foreign','viewer']}});assert.equal(writes,1);
listener({query:{queryKey:key}});assert.equal(writes,2);assert.equal(rows[0].id,'profile-owner');
assert.deepEqual(await run(source.replace('["profile-owner","profile-other"]','["profile-other","profile-owner"]')),{refreshed:true,order_applied:true});assert.equal(rows[0].id,'profile-other');assert.equal(subscriptions,1);
current='changed';listener({query:{queryKey:key}});assert.equal(stops,1);assert.equal(writes,3);
globalThis.document={getElementById:()=>null};assert.deepEqual(await run(),{refreshed:false});
})().catch(e=>{console.error(e);process.exit(1)});`
	script = strings.Replace(script, "SOURCE", nativeJSON(source), 1)
	if output, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("native order refresh: %v\n%s", err, output)
	}
}
