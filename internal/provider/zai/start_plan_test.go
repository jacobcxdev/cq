package zai

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/quota"
)

func startBalance(t *testing.T, raw string) zcode.Balance {
	t.Helper()
	var b zcode.Balance
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	b.ReferenceTime = time.Unix(1793808000, 0)
	return b
}

const currentStartBalance = `{"plans":[{"user_plan_id":"user-plan","plan_id":"start","status":"active","starts_at":1793721600,"ends_at":1794153599}],"balances":[{"bucket_id":"glm","user_plan_id":"user-plan","plan_id":"start","show_name":"GLM-5.3","capabilities":["model:glm-5.3"],"total_units":3000000,"used_units":0,"remaining_units":3000000,"available_units":3000000,"reserved_units":0,"period_start":1793807999,"period_end":1793894399,"expires_at":1793894399},{"bucket_id":"flash","user_plan_id":"user-plan","plan_id":"start","show_name":"GLM-5.3-Flash","capabilities":["model:glm-5.3-flash"],"total_units":"5000000","used_units":"1000000","remaining_units":"4000000","available_units":"3500000","reserved_units":"500000","period_start":"1793807999","period_end":"1793894399","expires_at":"1793894399"}]}`

func TestStartPlanResultUsesActualModelBuckets(t *testing.T) {
	b := startBalance(t, currentStartBalance)
	result, err := startPlanResult(b)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan != "ZCode Start" || !result.Active || result.Status != quota.StatusOK || len(result.Windows) != 2 {
		t.Fatalf("result: %+v", result)
	}
	for name, want := range map[quota.WindowName]float64{"1d:glm-5.3": 100, "1d:glm-5.3-flash": 70} {
		window, ok := result.Windows[name]
		if !ok || window.RemainingPctExact == nil || *window.RemainingPctExact != want || window.ResetAtUnix != 1793894399 {
			t.Fatalf("window %s: %+v", name, window)
		}
	}
	if _, ok := result.Windows[quota.Window5Hour]; ok {
		t.Fatal("invented paid window")
	}
	if _, ok := result.Windows[quota.Window7Day]; ok {
		t.Fatal("invented paid window")
	}
}

func TestStartPlanResultPreservesReportedInclusivePeriod(t *testing.T) {
	b := startBalance(t, currentStartBalance)
	for i := range b.Balances {
		b.Balances[i].PeriodStart = json.RawMessage(`1793808000`)
		b.Balances[i].Meter = "model_usage"
		b.Balances[i].UnitType = "token"
		b.Balances[i].ReservedUnits = nil
		b.Balances[i].UsedUnits = nil
		b.Balances[i].RemainingUnits = b.Balances[i].TotalUnits
		b.Balances[i].AvailableUnits = b.Balances[i].TotalUnits
	}
	result, err := startPlanResult(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []quota.WindowName{"86399s:glm-5.3", "86399s:glm-5.3-flash"} {
		if window, ok := result.Windows[name]; !ok || window.RemainingPct != 100 || window.ResetAtUnix != 1793894399 {
			t.Fatalf("reported period changed: %+v", result.Windows)
		}
	}
}

func TestStartPlanResultAcceptedImmediateEntitlementSentinel(t *testing.T) {
	for _, raw := range []string{`0`, `"0"`} {
		b := startBalance(t, currentStartBalance)
		b.Plans[0].Entitlements = []zcode.PlanEntitlement{{EntitlementID: "glm", EffectiveAt: json.RawMessage(raw)}, {EntitlementID: "flash", EffectiveAt: json.RawMessage(raw)}}
		b.Balances[0].EntitlementID, b.Balances[1].EntitlementID = "glm", "flash"
		if result, err := startPlanResult(b); err != nil || len(result.Windows) != 2 {
			t.Fatalf("immediate entitlement sentinel %s rejected: %v", raw, err)
		}
	}
}

func TestStartPlanResultRespectsRenewalAndExpiry(t *testing.T) {
	b := startBalance(t, currentStartBalance)
	b.Balances[0].PeriodEnd = json.RawMessage(`1793808000`)
	b.Balances[0].ExpiresAt = json.RawMessage(`1793808000`)
	next := b.Balances[0]
	next.PeriodStart = json.RawMessage(`1793808000`)
	next.PeriodEnd = json.RawMessage(`1793894400`)
	next.ExpiresAt = json.RawMessage(`1793894400`)
	next.AvailableUnits = json.RawMessage(`1500000`)
	b.Balances = append(b.Balances, next)
	result, err := startPlanResult(b)
	if err != nil {
		t.Fatal(err)
	}
	if result.Windows["1d:glm-5.3"].RemainingPct != 50 || result.Windows["1d:glm-5.3"].ResetAtUnix != 1793894400 {
		t.Fatalf("renewed bucket: %+v", result.Windows)
	}
	b.ReferenceTime = time.Unix(1794153599, 0)
	if _, err := startPlanResult(b); !errors.Is(err, errStartPlanUnavailable) {
		t.Fatalf("expired plan: %v", err)
	}
}

func TestStartPlanResultExcludesInactiveAndForeignPlans(t *testing.T) {
	b := startBalance(t, currentStartBalance)
	b.Plans = append(b.Plans, zcode.Plan{UserPlanID: "inactive", PlanID: "expired", Status: "expired"})
	b.Balances[0].UserPlanID = "inactive"
	b.Balances[0].PlanID = "expired"
	b.Balances[0].Capabilities = []string{"model:unrepresentable:model"}
	result, err := startPlanResult(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Windows) != 1 {
		t.Fatalf("inactive balance leaked: %+v", result.Windows)
	}
	if _, ok := result.Windows["1d:glm-5.3"]; ok {
		t.Fatal("inactive model included")
	}
	b.Balances[1].UserPlanID = "unknown-user-plan"
	if _, err := startPlanResult(b); !errors.Is(err, errStartPlanUnavailable) {
		t.Fatalf("foreign plan: %v", err)
	}
}

func TestStartPlanResultIgnoresOtherUnitsAndRetainsSmallBalances(t *testing.T) {
	b := startBalance(t, currentStartBalance)
	b.Balances[0].UnitType = "credit"
	b.Balances[0].Capabilities = []string{"model:unrepresentable:model"}
	b.Balances[1].TotalUnits = json.RawMessage(`5000000`)
	b.Balances[1].AvailableUnits = json.RawMessage(`1`)
	b.Balances[1].UsedUnits = nil
	b.Balances[1].RemainingUnits = nil
	b.Balances[1].ReservedUnits = nil
	result, err := startPlanResult(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Windows) != 1 || result.Status != quota.StatusOK {
		t.Fatalf("small real balance or unit isolation lost: %+v", result)
	}
	if value := result.Windows["1d:glm-5.3-flash"].RemainingPctExact; value == nil || *value <= 0 {
		t.Fatal("positive balance rounded into exhaustion")
	}
}

func TestStartPlanResultRejectsUnknownAndContradictoryValues(t *testing.T) {
	for name, mutate := range map[string]func(*zcode.Balance){
		"missing total": func(b *zcode.Balance) { b.Balances[0].TotalUnits = nil },
		"null available and remaining": func(b *zcode.Balance) {
			b.Balances[0].AvailableUnits = json.RawMessage(`null`)
			b.Balances[0].RemainingUnits = json.RawMessage(`null`)
		},
		"negative":                  func(b *zcode.Balance) { b.Balances[0].AvailableUnits = json.RawMessage(`-1`) },
		"over total":                func(b *zcode.Balance) { b.Balances[0].AvailableUnits = json.RawMessage(`3000001`) },
		"invalid string":            func(b *zcode.Balance) { b.Balances[0].TotalUnits = json.RawMessage(`"NaN"`) },
		"fraction string":           func(b *zcode.Balance) { b.Balances[0].TotalUnits = json.RawMessage(`"3/4"`) },
		"boolean":                   func(b *zcode.Balance) { b.Balances[0].TotalUnits = json.RawMessage(`true`) },
		"contradictory reservation": func(b *zcode.Balance) { b.Balances[0].ReservedUnits = json.RawMessage(`1`) },
		"contradictory usage":       func(b *zcode.Balance) { b.Balances[0].UsedUnits = json.RawMessage(`1`) },
		"unknown period":            func(b *zcode.Balance) { b.Balances[0].PeriodEnd = nil },
		"fractional timestamp":      func(b *zcode.Balance) { b.Balances[0].PeriodStart = json.RawMessage(`1793807999.5`) },
		"unknown clock":             func(b *zcode.Balance) { b.ReferenceTime = time.Time{} },
		"duplicate":                 func(b *zcode.Balance) { b.Balances = append(b.Balances, b.Balances[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			b := startBalance(t, currentStartBalance)
			mutate(&b)
			if _, err := startPlanResult(b); err == nil {
				t.Fatal("accepted unknown or contradictory values")
			}
		})
	}
}

func TestStartPlanResultRemainingFallbackAndPrecision(t *testing.T) {
	b := startBalance(t, currentStartBalance)
	b.Balances = b.Balances[1:]
	b.Balances[0].AvailableUnits = nil
	b.Balances[0].Capabilities = nil
	b.Balances[0].ShowName = "GLM-5.3 Flash"
	result, err := startPlanResult(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Windows["1d:glm-5.3-flash"].RemainingPct; got != 70 {
		t.Fatalf("reservation deducted incorrectly: %d", got)
	}
	b.Balances[0].TotalUnits = json.RawMessage(`"10000000000000000"`)
	b.Balances[0].AvailableUnits = json.RawMessage(`"10000000000000001"`)
	if _, err := startPlanResult(b); err == nil {
		t.Fatal("rounded contradictory large quantities")
	}
}

func TestStartPlanResultUsesEffectiveDeadlinesAndServerClock(t *testing.T) {
	b := startBalance(t, currentStartBalance)
	b.Balances = b.Balances[:1]
	b.Balances[0].ExpiresAt = json.RawMessage(`1793810000`)
	result, err := startPlanResult(b)
	if err != nil {
		t.Fatal(err)
	}
	if result.Windows["1d:glm-5.3"].ResetAtUnix != 1793810000 {
		t.Fatal("lost earlier bucket expiry")
	}
	b.Plans[0].EndsAt = json.RawMessage(`1793809000`)
	result, err = startPlanResult(b)
	if err != nil {
		t.Fatal(err)
	}
	if result.Windows["1d:glm-5.3"].ResetAtUnix != 1793809000 {
		t.Fatal("lost earlier plan expiry")
	}
	b.Plans[0].Entitlements = []zcode.PlanEntitlement{{EntitlementID: "future", EffectiveAt: json.RawMessage(`1793809000`)}}
	b.Balances[0].EntitlementID = "future"
	if _, err := startPlanResult(b); !errors.Is(err, errStartPlanUnavailable) {
		t.Fatalf("future entitlement: %v", err)
	}
}
