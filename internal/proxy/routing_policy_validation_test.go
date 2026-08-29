package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	providerCodex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const (
	testPoolIDA PoolID = "11111111-1111-4111-8111-111111111111"
	testPoolIDB PoolID = "22222222-2222-4222-8222-222222222222"
)

func TestRoutingPolicyV2ValidatesPoolIdentityNamesAndReferences(t *testing.T) {
	digest := strings.Repeat("a", 64)
	valid := RoutingPolicyV2{
		SchemaVersion: 2, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools: []AccountPoolV2{
			{ID: testPoolIDA, Name: "Cyber", Value: 10, Members: []providerCodex.AccountKey{"acct-a"}},
			{ID: testPoolIDB, Name: "R&D", Members: []providerCodex.AccountKey{"acct-b"}},
		},
		SessionBindings: []SessionBindingV2{{SessionDigest: digest, PoolID: testPoolIDA}},
	}
	if err := validateRoutingPolicyV2(valid, nil); err != nil {
		t.Fatalf("valid policy: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RoutingPolicyV2)
	}{
		{name: "malformed ID", mutate: func(policy *RoutingPolicyV2) { policy.Pools[0].ID = "not-a-uuid" }},
		{name: "duplicate ID", mutate: func(policy *RoutingPolicyV2) { policy.Pools[1].ID = testPoolIDA }},
		{name: "case-insensitive duplicate name", mutate: func(policy *RoutingPolicyV2) { policy.Pools[1].Name = "cYbEr" }},
		{name: "empty trimmed name", mutate: func(policy *RoutingPolicyV2) { policy.Pools[0].Name = "  " }},
		{name: "control-bearing name", mutate: func(policy *RoutingPolicyV2) { policy.Pools[0].Name = "Cyber\n" }},
		{name: "duplicate member", mutate: func(policy *RoutingPolicyV2) {
			policy.Pools[0].Members = []providerCodex.AccountKey{"acct-a", "acct-a"}
		}},
		{name: "dangling binding", mutate: func(policy *RoutingPolicyV2) {
			policy.SessionBindings[0].PoolID = "33333333-3333-4333-8333-333333333333"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := cloneRoutingPolicyV2(valid)
			test.mutate(&policy)
			if err := validateRoutingPolicyV2(policy, nil); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestRoutingPolicyDocumentHidesIdentityAndProjectsNames(t *testing.T) {
	digest := strings.Repeat("b", 64)
	policy := RoutingPolicyV2{
		SchemaVersion: 2, AuthorityGeneration: 2, RoutingGeneration: 2, EffectiveGeneration: 2,
		Pools:           []AccountPoolV2{{ID: testPoolIDA, Name: "Cyber", Value: 10, Members: []providerCodex.AccountKey{"acct-a"}}},
		SessionBindings: []SessionBindingV2{{SessionDigest: digest, PoolID: testPoolIDA}},
		CapabilityPool:  testPoolIDA,
		MAC:             strings.Repeat("c", 64),
	}
	document, err := routingPolicyDocument(policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Pools) != 1 || document.Pools[0].Name != "Cyber" || document.Pools[0].Value != 10 {
		t.Fatalf("document pools = %#v", document.Pools)
	}
	if len(document.SessionBindings) != 1 || document.SessionBindings[0].Pool != "Cyber" || document.CapabilityPool != "Cyber" {
		t.Fatalf("document references = %#v / %q", document.SessionBindings, document.CapabilityPool)
	}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte(testPoolIDA), []byte(`"id"`), []byte(`"pool_id"`), []byte(`"mac"`)} {
		if bytes.Contains(body, forbidden) {
			t.Fatalf("public policy leaked internal field %q: %s", forbidden, body)
		}
	}
}

func TestCompileRoutingPolicyDocumentRetainsIdentityByName(t *testing.T) {
	current := RoutingPolicyV2{
		SchemaVersion: 2, AuthorityGeneration: 1, RoutingGeneration: 1, EffectiveGeneration: 1,
		Pools: []AccountPoolV2{{ID: testPoolIDA, Name: "Cyber", Value: 10, Members: []providerCodex.AccountKey{"acct-a"}}},
	}
	document := RoutingPolicyDocument{
		SchemaVersion: 1, AuthorityGeneration: 2, RoutingGeneration: 2, EffectiveGeneration: 2,
		Pools: []AccountPoolDocument{
			{Name: "cyber", Value: 12, Members: []providerCodex.AccountKey{"acct-b"}},
			{Name: "Security Research", Members: []providerCodex.AccountKey{"acct-c"}},
		},
		SessionBindings: []SessionBindingDocument{{SessionDigest: strings.Repeat("d", 64), Pool: "SECURITY RESEARCH"}},
	}
	compiled, err := compileRoutingPolicyDocument(document, &current, bytes.NewReader(bytes.Repeat([]byte{0x44}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Pools[0].ID != testPoolIDA || compiled.Pools[0].Name != "Cyber" || compiled.Pools[0].Value != 12 {
		t.Fatalf("existing pool = %#v", compiled.Pools[0])
	}
	if compiled.Pools[1].ID == "" || compiled.Pools[1].ID == testPoolIDA || compiled.Pools[1].Name != "Security Research" || compiled.Pools[1].Value != 0 {
		t.Fatalf("new pool = %#v", compiled.Pools[1])
	}
	if compiled.SessionBindings[0].PoolID != compiled.Pools[1].ID {
		t.Fatalf("binding = %#v, pool = %#v", compiled.SessionBindings[0], compiled.Pools[1])
	}
}

func TestRoutingPolicyDocumentRejectsInvalidPoolValues(t *testing.T) {
	for _, value := range []string{"-1", "1.5", "4294967296", `"10"`} {
		body := []byte(`{"schema_version":1,"authority_generation":1,"routing_generation":1,"effective_generation":1,"pools":[{"name":"Cyber","value":` + value + `,"members":["acct-a"]}]}`)
		var document RoutingPolicyDocument
		if err := json.Unmarshal(body, &document); err == nil {
			t.Fatalf("value %s accepted", value)
		}
	}
}
