package zai

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/quota"
)

var errStartPlanUnavailable = errors.New("ZCode Start Plan has no current model token quota")

type currentStartPlan struct {
	plan zcode.Plan
	end  int64
}

// Start Plan buckets are independent token entitlements, not paid subscription
// windows. Preserve the actual renewal period and scope every window to a model.
func startPlanResult(balance zcode.Balance) (quota.Result, error) {
	if balance.ReferenceTime.IsZero() {
		return quota.Result{}, invalidStartBalance()
	}
	now := balance.ReferenceTime.Unix()
	var active []currentStartPlan
	for _, plan := range balance.Plans {
		if !strings.EqualFold(strings.TrimSpace(plan.Status), "active") {
			continue
		}
		start, _, err := startTimestamp(plan.StartsAt)
		if err != nil {
			return quota.Result{}, err
		}
		end, _, err := startTimestamp(plan.EndsAt)
		if err != nil || start > 0 && end > 0 && end <= start {
			return quota.Result{}, invalidStartBalance()
		}
		if start > now || end > 0 && end <= now {
			continue
		}
		active = append(active, currentStartPlan{plan: plan, end: end})
	}
	windows := make(map[quota.WindowName]quota.Window)
	exhausted := false
	for _, bucket := range balance.Balances {
		if bucket.UnitType != "" && !strings.EqualFold(strings.TrimSpace(bucket.UnitType), "token") {
			continue
		}
		models, modelErr := startBucketModels(bucket)
		if len(models) == 0 && modelErr == nil {
			continue
		}
		var matches []currentStartPlan
		for _, candidate := range active {
			if bucket.UserPlanID != "" && bucket.UserPlanID != candidate.plan.UserPlanID || bucket.PlanID != "" && bucket.PlanID != candidate.plan.PlanID {
				continue
			}
			matches = append(matches, candidate)
		}
		if len(matches) == 0 {
			continue
		}
		if modelErr != nil {
			return quota.Result{}, modelErr
		}
		if len(matches) > 1 {
			return quota.Result{}, invalidStartBalance()
		}
		plan := matches[0]
		if bucket.EntitlementID != "" && len(plan.plan.Entitlements) > 0 {
			found := false
			effective := true
			for _, entitlement := range plan.plan.Entitlements {
				if entitlement.EntitlementID != bucket.EntitlementID {
					continue
				}
				if found {
					return quota.Result{}, invalidStartBalance()
				}
				found = true
				// The live service uses effective_at=0 for an entitlement
				// which applies immediately. It is not a period timestamp.
				value, known, err := startNumber(entitlement.EffectiveAt)
				if err != nil {
					return quota.Result{}, err
				}
				if known && value.Sign() == 0 {
					continue
				}
				at, _, err := startTimestamp(entitlement.EffectiveAt)
				if err != nil {
					return quota.Result{}, err
				}
				effective = at == 0 || at <= now
			}
			if !found || !effective {
				continue
			}
		}
		start, startKnown, err := startTimestamp(bucket.PeriodStart)
		if err != nil {
			return quota.Result{}, err
		}
		end, endKnown, err := startTimestamp(bucket.PeriodEnd)
		if err != nil {
			return quota.Result{}, err
		}
		expiry, _, err := startTimestamp(bucket.ExpiresAt)
		if err != nil {
			return quota.Result{}, err
		}
		if !startKnown || !endKnown || end <= start {
			return quota.Result{}, invalidStartBalance()
		}
		deadline := end
		for _, at := range []int64{expiry, plan.end} {
			if at > 0 && at < deadline {
				deadline = at
			}
		}
		if start > now || deadline <= now {
			continue
		}
		remaining, empty, err := startBucketPercentage(bucket)
		if err != nil {
			return quota.Result{}, err
		}
		for _, model := range models {
			name, ok := quota.WindowNameForPeriod(end-start, model)
			if !ok {
				return quota.Result{}, invalidStartBalance()
			}
			if _, exists := windows[name]; exists {
				return quota.Result{}, invalidStartBalance()
			}
			pct := remaining
			windows[name] = quota.Window{RemainingPct: int(math.Round(pct)), RemainingPctExact: &pct, ResetAtUnix: deadline}
		}
		exhausted = exhausted || empty
	}
	if len(windows) == 0 {
		return quota.Result{}, errStartPlanUnavailable
	}
	status := quota.StatusOK
	if exhausted {
		status = quota.StatusExhausted
	}
	return quota.Result{Active: true, Plan: "ZCode Start", Status: status, Windows: windows}, nil
}

func startBucketModels(bucket zcode.Bucket) ([]string, error) {
	var models []string
	seen := make(map[string]bool)
	for _, capability := range bucket.Capabilities {
		prefix, value, ok := strings.Cut(strings.TrimSpace(capability), ":")
		if !ok || !strings.EqualFold(prefix, "model") {
			continue
		}
		model := canonicalStartModel(value)
		if model == "" {
			return nil, invalidStartBalance()
		}
		if !seen[model] {
			models = append(models, model)
			seen[model] = true
		}
	}
	if len(models) == 0 {
		if model := canonicalStartModel(bucket.ShowName); strings.HasPrefix(model, "glm-") {
			models = append(models, model)
		}
	}
	return models, nil
}

func canonicalStartModel(value string) string {
	value = strings.ToLower(strings.Join(strings.Fields(value), "-"))
	if value == "" {
		return ""
	}
	for i, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || i > 0 && (r == '.' || r == '-' || r == '_') {
			continue
		}
		return ""
	}
	return value
}

func startBucketPercentage(bucket zcode.Bucket) (float64, bool, error) {
	raws := []json.RawMessage{bucket.TotalUnits, bucket.AvailableUnits, bucket.RemainingUnits, bucket.ReservedUnits, bucket.UsedUnits}
	values := make([]*big.Rat, len(raws))
	for i, raw := range raws {
		value, _, err := startNumber(raw)
		if err != nil {
			return 0, false, err
		}
		values[i] = value
	}
	total, available, remaining, reserved, used := values[0], values[1], values[2], values[3], values[4]
	if total == nil || total.Sign() <= 0 || available == nil && remaining == nil {
		return 0, false, invalidStartBalance()
	}
	for _, value := range values[1:] {
		if value != nil && value.Cmp(total) > 0 {
			return 0, false, invalidStartBalance()
		}
	}
	if remaining != nil {
		if used != nil && new(big.Rat).Add(remaining, used).Cmp(total) > 0 {
			return 0, false, invalidStartBalance()
		}
		if reserved != nil && reserved.Cmp(remaining) > 0 {
			return 0, false, invalidStartBalance()
		}
		if available != nil {
			sum := new(big.Rat).Set(available)
			if reserved != nil {
				sum.Add(sum, reserved)
			}
			if sum.Cmp(remaining) > 0 {
				return 0, false, invalidStartBalance()
			}
		}
	}
	if available == nil {
		available = new(big.Rat).Set(remaining)
		if reserved != nil {
			available.Sub(available, reserved)
		}
	}
	sum := new(big.Rat).Set(available)
	if reserved != nil {
		sum.Add(sum, reserved)
	}
	if used != nil {
		sum.Add(sum, used)
	}
	if sum.Cmp(total) > 0 {
		return 0, false, invalidStartBalance()
	}
	ratio := new(big.Rat).Quo(available, total)
	ratio.Mul(ratio, big.NewRat(100, 1))
	pct, _ := ratio.Float64()
	return pct, available.Sign() == 0, nil
}

// Numeric strings and JSON numbers share one contract. Rational comparisons
// avoid accepting contradictory quantities after floating-point rounding.
func startNumber(raw json.RawMessage) (*big.Rat, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, false, nil
	}
	text := string(raw)
	if raw[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return nil, false, invalidStartBalance()
		}
		text = strings.TrimSpace(text)
	}
	if text == "" || text[0] != '-' && (text[0] < '0' || text[0] > '9') {
		return nil, false, invalidStartBalance()
	}
	var number json.Number
	if json.Unmarshal([]byte(text), &number) != nil || number.String() == "" {
		return nil, false, invalidStartBalance()
	}
	bounded, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(bounded) || math.IsInf(bounded, 0) {
		return nil, false, invalidStartBalance()
	}
	value, ok := new(big.Rat).SetString(number.String())
	if !ok || value.Sign() < 0 {
		return nil, false, invalidStartBalance()
	}
	return value, true, nil
}

func startTimestamp(raw json.RawMessage) (int64, bool, error) {
	number, known, err := startNumber(raw)
	if err != nil || !known {
		return 0, known, err
	}
	if !number.IsInt() || !number.Num().IsInt64() || number.Sign() <= 0 {
		return 0, false, invalidStartBalance()
	}
	return number.Num().Int64(), true, nil
}

func invalidStartBalance() error { return errors.New("invalid ZCode Start Plan balance") }
