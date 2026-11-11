package codex

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/quota"
)

func TestParseUsageObservationPreservesIncludedAndModelPermission(t *testing.T) {
	observation := ParseUsageObservation([]byte(`{"rate_limit":{"allowed":false,"limit_reached":true},"model_usage":{"gpt-6-astra":{"available":false,"credits_would_enable":true}}}`), "", "")
	if observation.IncludedUsageAllowed == nil || *observation.IncludedUsageAllowed || observation.LimitReached == nil || !*observation.LimitReached {
		t.Fatalf("included allowance = %+v", observation)
	}
	model := observation.ModelUsage["gpt-6-astra"]
	if model.Available == nil || *model.Available || !model.CreditsWouldEnable {
		t.Fatalf("model = %+v", model)
	}
	missing := ParseUsageObservation([]byte(`{}`), "", "")
	if missing.IncludedUsageAllowed != nil || missing.LimitReached != nil {
		t.Fatal("missing permission became explicit permission")
	}
}

func TestParseUsageObservationValidatesIncludedPercentages(t *testing.T) {
	for _, tc := range []struct {
		name, percent string
		want          bool
	}{
		{"missing", "", false},
		{"null", `"used_percent":null,`, false},
		{"negative", `"used_percent":-1,`, false},
		{"over limit", `"used_percent":101,`, false},
		{"zero", `"used_percent":0,`, true},
		{"fraction", `"used_percent":99.6,`, true},
		{"exhausted", `"used_percent":100,`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseUsageObservation([]byte(`{"rate_limit":{"allowed":true,"primary_window":{`+tc.percent+`"limit_window_seconds":604800,"reset_at":2000000000}}}`), "", "")
			if got.IncludedUsageValid != tc.want {
				t.Fatalf("IncludedUsageValid = %v, want %v", got.IncludedUsageValid, tc.want)
			}
			if !got.Result.IsUsable() {
				t.Fatal("legacy report tolerance changed")
			}
		})
	}
	if ParseUsageObservation([]byte(`{"rate_limit":{"allowed":true}}`), "", "").IncludedUsageValid {
		t.Fatal("missing windows validated")
	}
}

func TestParseUsageObservationPreservesScopedPermission(t *testing.T) {
	got := ParseUsageObservation([]byte(`{"additional_rate_limits":[{"limit_name":"gpt-6-astra","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":50,"limit_window_seconds":604800,"reset_at":2000000000}}}]}`), "", "")
	model := got.ModelUsage["gpt-6-astra"]
	if model.IncludedUsageAllowed == nil || *model.IncludedUsageAllowed || model.LimitReached == nil || !*model.LimitReached {
		t.Fatalf("scoped permission = %+v", model)
	}
}

func TestExtraCreditsReportUsesCurrentPolicyWithoutChangingAvailability(t *testing.T) {
	fs := fsutil.NewMemFS()
	policy := &ExtraCreditsPolicy{FS: fs, Path: "/state/extra-credits.json"}
	identity := AccountIdentity{AccountID: "workspace", UserID: "user", Email: "account@example.test"}
	canonical := generationAccountKey(identity, SourceSystem, "")
	p := &Provider{ExtraCreditsPolicy: policy, inventory: staticCredentialInventory{inventory: Inventory{Accounts: []LogicalAccount{{Key: "account", Identity: identity}}}}}
	available := true
	rows := []quota.Result{{AccountKey: "account", Status: quota.StatusOK, ExtraCredits: &quota.ExtraCredits{Enabled: true, Available: &available}}}
	if err := policy.Set(canonical, false); err != nil {
		t.Fatal(err)
	}
	got := p.EnrichResults(context.Background(), rows)
	if got[0].ExtraCredits.Enabled || !*got[0].ExtraCredits.Available {
		t.Fatalf("disabled report = %+v", got[0].ExtraCredits)
	}
	if !rows[0].ExtraCredits.Enabled {
		t.Fatal("report enrichment mutated cached policy")
	}
	if err := policy.Set(canonical, true); err != nil {
		t.Fatal(err)
	}
	got = p.EnrichResults(context.Background(), got)
	if !got[0].ExtraCredits.Enabled {
		t.Fatal("cached permission hid current enabled policy")
	}
}

func TestExtraCreditsReportResolvesLegacyCachedIdentity(t *testing.T) {
	fs := fsutil.NewMemFS()
	policy := &ExtraCreditsPolicy{FS: fs, Path: "/state/extra-credits.json"}
	selected := AccountIdentity{AccountID: "workspace", UserID: "selected-user", Email: "selected@example.test"}
	if err := policy.Set(generationAccountKey(selected, SourceSystem, ""), false); err != nil {
		t.Fatal(err)
	}
	p := &Provider{ExtraCreditsPolicy: policy, inventory: staticCredentialInventory{inventory: Inventory{Accounts: []LogicalAccount{
		{Key: "selected", Identity: selected},
		{Key: "other", Identity: AccountIdentity{AccountID: "workspace", UserID: "other-user", Email: "other@example.test"}},
	}}}}
	rows := []quota.Result{{AccountID: "workspace", Email: "selected@example.test"}, {AccountID: "workspace", Email: "other@example.test"}, {AccountID: "workspace", ExtraCredits: &quota.ExtraCredits{Enabled: true}}}
	got := p.EnrichResults(context.Background(), rows)
	if got[0].AccountKey != "selected" || got[0].ExtraCredits.Enabled {
		t.Fatalf("selected = %+v", got[0])
	}
	if got[1].AccountKey != "other" || !got[1].ExtraCredits.Enabled {
		t.Fatalf("other = %+v", got[1])
	}
	if got[2].AccountKey != "" || got[2].ExtraCredits.Enabled {
		t.Fatalf("ambiguous = %+v", got[2])
	}
	if rows[0].AccountKey != "" {
		t.Fatal("legacy cache mutated")
	}
}

func TestExtraCreditsReportFailsClosedOnInvalidPolicy(t *testing.T) {
	fs := fsutil.NewMemFS()
	policy := &ExtraCreditsPolicy{FS: fs, Path: "/state/extra-credits.json"}
	if err := fs.MkdirAll("/state", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(policy.Path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Provider{ExtraCreditsPolicy: policy, inventory: staticCredentialInventory{inventory: Inventory{Accounts: []LogicalAccount{{Key: "account", Identity: AccountIdentity{AccountID: "workspace", UserID: "user"}}}}}}
	got := p.EnrichResults(context.Background(), []quota.Result{{AccountKey: "account", ExtraCredits: &quota.ExtraCredits{Enabled: true}}})
	if got[0].ExtraCredits.Enabled {
		t.Fatal("invalid policy showed enabled credit spending")
	}
}

func TestExtraCreditsReportKeepsDisabledIdentityAcrossLogicalKeyChange(t *testing.T) {
	fs := fsutil.NewMemFS()
	policy := &ExtraCreditsPolicy{FS: fs, Path: "/state/extra-credits.json"}
	identity := AccountIdentity{AccountID: "workspace", UserID: "user", Email: "account@example.test"}
	canonical := generationAccountKey(identity, SourceExternal, "codexbar")
	if err := policy.Set(canonical, false); err != nil {
		t.Fatal(err)
	}
	p := &Provider{ExtraCreditsPolicy: policy, inventory: staticCredentialInventory{inventory: Inventory{Accounts: []LogicalAccount{{Key: "acct:managed", Identity: identity}}}}}
	for _, reportedKey := range []string{string(canonical), "acct:managed"} {
		rows := []quota.Result{{AccountKey: reportedKey, AccountID: identity.AccountID, Email: identity.Email, ExtraCredits: &quota.ExtraCredits{Enabled: true}}}
		got := p.EnrichResults(context.Background(), rows)
		if got[0].ExtraCredits.Enabled || got[0].AccountKey != reportedKey {
			t.Fatalf("migrated cached row = %+v", got[0])
		}
	}
}

func TestExtraCreditsReportRejectsMissingOrAmbiguousStrongIdentity(t *testing.T) {
	for _, accounts := range [][]LogicalAccount{
		nil,
		{{Key: "account", Identity: AccountIdentity{AccountID: "workspace"}}},
		{{Key: "account", Identity: AccountIdentity{AccountID: "workspace", UserID: "user"}}, {Key: "account", Identity: AccountIdentity{AccountID: "workspace", UserID: "other-user"}}},
	} {
		p := &Provider{ExtraCreditsPolicy: &ExtraCreditsPolicy{FS: fsutil.NewMemFS(), Path: "/state/extra-credits.json"}, inventory: staticCredentialInventory{inventory: Inventory{Accounts: accounts}}}
		got := p.EnrichResults(context.Background(), []quota.Result{{AccountKey: "account", ExtraCredits: &quota.ExtraCredits{Enabled: true}}})
		if got[0].ExtraCredits.Enabled {
			t.Fatalf("unverified inventory granted permission: %+v", accounts)
		}
	}
}

func TestParseUsagePreservesExtraCreditAvailability(t *testing.T) {
	for _, tc := range []struct {
		name         string
		creditFields string
		want         any
	}{
		{"available", `"credits":{"has_credits":true,"unlimited":false,"balance":null}`, true},
		{"unlimited", `"credits":{"has_credits":false,"unlimited":true}`, true},
		{"empty", `"credits":{"has_credits":false,"unlimited":false,"balance":"0"}`, false},
		{"spend blocked", `"credits":{"has_credits":true},"spend_control":{"reached":true}`, false},
		{"overage blocked", `"credits":{"has_credits":true,"overage_limit_reached":true}`, false},
		{"unknown", `"credits":{}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := parseUsage([]byte(`{`+tc.creditFields+`}`), "account@example.test", "account")
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			credits, ok := got["extra_credits"].(map[string]any)
			if !ok {
				t.Fatalf("missing extra_credits in %s", encoded)
			}
			if credits["enabled"] != true || credits["available"] != tc.want {
				t.Fatalf("extra_credits = %v, want enabled true, available %v", credits, tc.want)
			}
		})
	}
}
