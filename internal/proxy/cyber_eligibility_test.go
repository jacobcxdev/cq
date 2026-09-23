package proxy

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider/codex"
)

func TestCyberEligibilityStoreKeepsExactEvidenceAndDenial(t *testing.T) {
	store := NewCyberEligibilityStore(nil)
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }
	models := map[string]map[string]CyberAccessState{
		"gpt-6-sol": {"daybreak_blue": CyberAccessEligible, "daybreak_red": CyberAccessIneligible},
	}
	store.ReplaceAccount("account-a", models)
	models["gpt-6-sol"]["daybreak_blue"] = CyberAccessIneligible
	if got := store.Status("account-a", "gpt-6-sol", "daybreak_blue"); got != CyberAccessEligible {
		t.Fatalf("stored catalogue changed with caller's map: %v", got)
	}
	for _, test := range []struct {
		account codex.AccountKey
		model   string
		program string
		want    CyberAccessState
	}{
		{"account-a", "gpt-6-sol", "daybreak_red", CyberAccessIneligible},
		{"account-a", "other-model", "daybreak_blue", CyberAccessUnknown},
		{"account-b", "gpt-6-sol", "daybreak_blue", CyberAccessUnknown},
	} {
		if got := store.Status(test.account, test.model, test.program); got != test.want {
			t.Fatalf("Status(%q, %q, %q) = %v, want %v", test.account, test.model, test.program, got, test.want)
		}
	}
	store.MarkIneligible("account-a", "gpt-6-sol", "daybreak_blue")
	store.ReplaceAccount("account-a", map[string]map[string]CyberAccessState{"gpt-6-sol": {"daybreak_blue": CyberAccessEligible}})
	if got := store.Status("account-a", "gpt-6-sol", "daybreak_blue"); got != CyberAccessIneligible {
		t.Fatalf("explicit upstream denial lost on catalogue refresh: %v", got)
	}
	if got := store.Status("account-a", "gpt-6-sol", "daybreak_red"); got != CyberAccessUnknown {
		t.Fatalf("stale catalogue was retained after successful refresh: %v", got)
	}
	now = now.Add(cyberDenialTTL)
	if got := store.Status("account-a", "gpt-6-sol", "daybreak_blue"); got != CyberAccessEligible {
		t.Fatalf("catalogue access did not recover after denial expiry: %v", got)
	}
}

func TestCyberPoolReconciliationPreservesIdentityValueAndBinding(t *testing.T) {
	inspector, directory, publisher, key := newRoutingPolicyAuthorityForTest(t)
	routing, err := OpenRoutingPolicyStore(context.Background(), inspector, directory, publisher, bytes.NewReader(bytes.Repeat([]byte{0x51}, 128)), key)
	if err != nil {
		t.Fatal(err)
	}
	all := []codex.AccountKey{"account-a", "account-b", "account-c"}
	changed, err := routing.ReconcileCyberPool(map[codex.AccountKey]bool{"account-a": true, "account-b": true}, all)
	if err != nil || !changed {
		t.Fatalf("created Cyber pool: changed=%v err=%v", changed, err)
	}
	first := routing.Current()
	if len(first.Pools) != 1 || first.Pools[0].Name != "Cyber" || first.Pools[0].Value != 10 || !reflect.DeepEqual(first.Pools[0].Members, []codex.AccountKey{"account-a", "account-b"}) {
		t.Fatalf("created Cyber pool = %#v", first.Pools)
	}
	if err := routing.SetPoolValue("Cyber", 37); err != nil {
		t.Fatal(err)
	}
	withBinding := routing.Current()
	withBinding.SessionBindings = []SessionBindingV2{{SessionDigest: strings.Repeat("a", 64), PoolID: first.Pools[0].ID}}
	advanceRoutingPolicy(&withBinding)
	if err := routing.Publish(withBinding); err != nil {
		t.Fatal(err)
	}
	changed, err = routing.ReconcileCyberPool(map[codex.AccountKey]bool{"account-b": false, "account-c": true}, all)
	if err != nil || !changed {
		t.Fatalf("updated Cyber pool: changed=%v err=%v", changed, err)
	}
	after := routing.Current()
	if after.Pools[0].ID != first.Pools[0].ID || after.Pools[0].Value != 37 || !reflect.DeepEqual(after.Pools[0].Members, []codex.AccountKey{"account-a", "account-c"}) || len(after.SessionBindings) != 1 || after.SessionBindings[0].PoolID != first.Pools[0].ID {
		t.Fatalf("reconciled Cyber policy = %#v", after)
	}
	eligibility := NewCyberEligibilityStore(routing)
	members := eligibility.Members()
	if !reflect.DeepEqual(members, []codex.AccountKey{"account-a", "account-c"}) {
		t.Fatalf("Cyber members = %v", members)
	}
	members[0] = "tampered"
	if got := eligibility.Members()[0]; got != "account-a" {
		t.Fatalf("caller mutated Cyber members: %q", got)
	}
	changed, err = routing.ReconcileCyberPool(nil, all)
	if err != nil || changed || !reflect.DeepEqual(routing.Current().Pools[0].Members, []codex.AccountKey{"account-a", "account-c"}) {
		t.Fatalf("unknown account evidence changed Cyber pool: changed=%v err=%v", changed, err)
	}
	changed, err = routing.ReconcileCyberPool(nil, []codex.AccountKey{"account-c"})
	if err != nil || !changed || !reflect.DeepEqual(routing.Current().Pools[0].Members, []codex.AccountKey{"account-c"}) {
		t.Fatalf("departed account remained in Cyber pool: changed=%v err=%v", changed, err)
	}
}
