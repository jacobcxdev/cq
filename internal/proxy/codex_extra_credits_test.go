package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

type extraCreditPolicyFunc func(codex.AccountKey) (bool, error)

func (f extraCreditPolicyFunc) Allowed(key codex.AccountKey) (bool, error) { return f(key) }

type extraCreditUsageFunc func(context.Context, codex.AccountKey) (codex.UsageObservation, error)

func (f extraCreditUsageFunc) Read(ctx context.Context, key codex.AccountKey) (codex.UsageObservation, error) {
	return f(ctx, key)
}

func TestExtraCreditsGuardFreshIncludedAllowance(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tc := range []struct {
		name, body string
		blocked    bool
	}{
		{"included", `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1800003600}}}`, false},
		{"credit fallback", `{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1800003600}},"credits":{"has_credits":true}}`, true},
		{"missing decision", `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1800003600}}}`, true},
		{"missing windows", `{"rate_limit":{"allowed":true}}`, true},
		{"expired window", `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1799999999}}}`, true},
		{"contradictory", `{"rate_limit":{"allowed":true,"limit_reached":true,"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1800003600}}}`, true},
		{"missing percentage", `{"rate_limit":{"allowed":true,"primary_window":{"limit_window_seconds":604800,"reset_at":1800003600}}}`, true},
		{"negative percentage", `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":-1,"limit_window_seconds":604800,"reset_at":1800003600}}}`, true},
		{"scoped denial", `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1800003600}},"additional_rate_limits":[{"limit_name":"gpt-6-astra","rate_limit":{"allowed":false,"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1800003600}}}]}`, true},
		{"unrelated depleted model", `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1800003600}},"additional_rate_limits":[{"limit_name":"gpt-5.3-codex-spark","rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1800003600}}}]}`, false},
		{"fractional remaining", `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":99.9,"limit_window_seconds":604800,"reset_at":1800003600}}}`, false},
		{"model credits", `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":1800003600}},"model_usage":{"gpt-6-astra":{"available":false,"credits_would_enable":true}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			guard := &CodexExtraCreditsGuard{
				Policy: extraCreditPolicyFunc(func(key codex.AccountKey) (bool, error) { return key != "firm", nil }),
				Usage: extraCreditUsageFunc(func(_ context.Context, key codex.AccountKey) (codex.UsageObservation, error) {
					reads++
					if key != "firm" {
						t.Fatalf("wrong identity: %q", key)
					}
					return codex.ParseUsageObservation([]byte(tc.body), "", ""), nil
				}),
				Now: func() time.Time { return now },
			}
			err := guard.Check(context.Background(), RouteChoice{AccountKey: "firm", EffectiveModel: "gpt-6-astra"})
			var limit *codexReserveLimitError
			if errors.As(err, &limit) != tc.blocked || reads != 1 {
				t.Fatalf("error=%v reads=%d, blocked=%v", err, reads, tc.blocked)
			}
			if err := guard.Check(context.Background(), RouteChoice{AccountKey: "personal"}); err != nil || reads != 1 {
				t.Fatalf("enabled account changed: %v, reads=%d", err, reads)
			}
		})
	}
}

func TestExtraCreditsGuardFailsClosed(t *testing.T) {
	for _, guard := range []*CodexExtraCreditsGuard{
		{Policy: extraCreditPolicyFunc(func(codex.AccountKey) (bool, error) { return false, errors.New("unreadable") })},
		{Policy: extraCreditPolicyFunc(func(codex.AccountKey) (bool, error) { return false, nil })},
		{Policy: extraCreditPolicyFunc(func(codex.AccountKey) (bool, error) { return false, nil }), Usage: extraCreditUsageFunc(func(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
			return codex.UsageObservation{}, errors.New("offline")
		})},
	} {
		var limit *codexReserveLimitError
		if err := guard.Check(context.Background(), RouteChoice{AccountKey: "firm"}); !errors.As(err, &limit) {
			t.Fatalf("not blocked: %v", err)
		}
	}
}

func TestExtraCreditsGuardKeepsPermissionAcrossLogicalKeyChanges(t *testing.T) {
	identity := codex.AccountIdentity{AccountID: "workspace", UserID: "user"}
	key, err := codex.ExtraCreditsIdentityKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	for _, logicalKey := range []codex.AccountKey{key, "acct_new-managed-key"} {
		guard := &CodexExtraCreditsGuard{
			Inventory: &staticCredentialInventory{inventory: codex.Inventory{Accounts: []codex.LogicalAccount{{Key: logicalKey, Identity: identity}}}},
			Policy:    extraCreditPolicyFunc(func(candidate codex.AccountKey) (bool, error) { return candidate != key, nil }),
			Usage: extraCreditUsageFunc(func(_ context.Context, account codex.AccountKey) (codex.UsageObservation, error) {
				reads++
				if account != logicalKey {
					t.Fatalf("usage used policy key instead of routing key: %q", account)
				}
				return extraCreditsBypassObservation(0), nil
			}),
		}
		var limit *CachedUsageLimitError
		if err := guard.Check(context.Background(), RouteChoice{AccountKey: logicalKey, EffectiveModel: "gpt-6-astra"}); !errors.As(err, &limit) {
			t.Fatalf("identity restriction lost: %v", err)
		}
	}
	if reads != 2 {
		t.Fatalf("usage reads=%d", reads)
	}
}

func TestExtraCreditsLegacyFrameCannotHideModel(t *testing.T) {
	capacity := NewCodexCapacityLedger(time.Now, time.Minute)
	capacity.ExtraCredits = &CodexExtraCreditsGuard{
		Policy: extraCreditPolicyFunc(func(codex.AccountKey) (bool, error) { return false, nil }),
		Usage: extraCreditUsageFunc(func(context.Context, codex.AccountKey) (codex.UsageObservation, error) {
			observation := extraCreditsBypassObservation(50)
			observation.ModelUsage = map[string]codex.ModelUsage{"gpt-6-astra": {CreditsWouldEnable: true}}
			return observation, nil
		}),
	}
	check := codexExtraCreditsFrameGuard(context.Background(), capacity, RouteChoice{AccountKey: "firm", EffectiveModel: "gpt-5.4"})
	for _, frame := range [][]byte{[]byte(`{"type":"response.create","params":{"model":"gpt-6-astra"}}`), []byte(`{"type":"response.create"}`), []byte(`invalid`)} {
		for _, kind := range []int{websocket.TextMessage, websocket.BinaryMessage} {
			var limit *CachedUsageLimitError
			if err := check(kind, frame); !errors.As(err, &limit) {
				t.Fatalf("kind=%d frame=%q error=%v", kind, frame, err)
			}
		}
	}
}
