package zcode

import (
	"encoding/json"
	"errors"
	"time"
)

type Balance struct {
	ServerTime    json.RawMessage `json:"server_time,omitempty"`
	Plans         []Plan          `json:"plans,omitempty"`
	Balances      []Bucket        `json:"balances,omitempty"`
	ReferenceTime time.Time       `json:"reference_time"`
}

type Plan struct {
	UserPlanID   string            `json:"user_plan_id,omitempty"`
	PlanID       string            `json:"plan_id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Status       string            `json:"status,omitempty"`
	StartsAt     json.RawMessage   `json:"starts_at,omitempty"`
	EndsAt       json.RawMessage   `json:"ends_at,omitempty"`
	Entitlements []PlanEntitlement `json:"entitlements,omitempty"`
}

type PlanEntitlement struct {
	EntitlementID string          `json:"entitlement_id,omitempty"`
	ShowName      string          `json:"show_name,omitempty"`
	Period        string          `json:"period,omitempty"`
	EffectiveAt   json.RawMessage `json:"effective_at,omitempty"`
}

type Bucket struct {
	BucketID       string          `json:"bucket_id,omitempty"`
	UserPlanID     string          `json:"user_plan_id,omitempty"`
	PlanID         string          `json:"plan_id,omitempty"`
	EntitlementID  string          `json:"entitlement_id,omitempty"`
	ShowName       string          `json:"show_name,omitempty"`
	Meter          string          `json:"meter,omitempty"`
	UnitType       string          `json:"unit_type,omitempty"`
	Capabilities   []string        `json:"capabilities,omitempty"`
	TotalUnits     json.RawMessage `json:"total_units,omitempty"`
	UsedUnits      json.RawMessage `json:"used_units,omitempty"`
	ReservedUnits  json.RawMessage `json:"reserved_units,omitempty"`
	RemainingUnits json.RawMessage `json:"remaining_units,omitempty"`
	AvailableUnits json.RawMessage `json:"available_units,omitempty"`
	PeriodStart    json.RawMessage `json:"period_start,omitempty"`
	PeriodEnd      json.RawMessage `json:"period_end,omitempty"`
	ExpiresAt      json.RawMessage `json:"expires_at,omitempty"`
}

func (b Balance) validScalars() bool {
	if !scalar(b.ServerTime) {
		return false
	}
	for _, plan := range b.Plans {
		if !scalar(plan.StartsAt) || !scalar(plan.EndsAt) {
			return false
		}
		for _, entitlement := range plan.Entitlements {
			if !scalar(entitlement.EffectiveAt) {
				return false
			}
		}
	}
	for _, bucket := range b.Balances {
		for _, value := range []json.RawMessage{bucket.TotalUnits, bucket.UsedUnits, bucket.ReservedUnits, bucket.RemainingUnits, bucket.AvailableUnits, bucket.PeriodStart, bucket.PeriodEnd, bucket.ExpiresAt} {
			if !scalar(value) {
				return false
			}
		}
	}
	return true
}

type ResetStatus struct {
	AvailableFiveHourResets    []ResetOpportunity `json:"available_five_hour_resets"`
	AvailableWeekResets        []ResetOpportunity `json:"available_week_resets"`
	LatestFiveHourResetHistory *ResetHistory      `json:"latest_five_hour_reset_history"`
	LatestWeekResetHistory     *ResetHistory      `json:"latest_week_reset_history"`
	HasUnreadHistory           bool               `json:"has_unread_history"`
}

type ResetOpportunity struct {
	ExpireAt json.Number `json:"expire_at"`
}

type ResetHistory struct {
	UsedAt json.Number `json:"used_at"`
}

func (r *ResetOpportunity) UnmarshalJSON(data []byte) error {
	value, err := resetTimestamp(data, "expire_at")
	r.ExpireAt = value
	return err
}

func (r *ResetHistory) UnmarshalJSON(data []byte) error {
	value, err := resetTimestamp(data, "used_at")
	r.UsedAt = value
	return err
}

func resetTimestamp(data []byte, field string) (json.Number, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) == nil {
		raw := object[field]
		var number json.Number
		// json.Number also accepts quoted numbers; the server contract does not.
		if len(raw) > 0 && raw[0] != '"' && json.Unmarshal(raw, &number) == nil && positiveNumber(number) {
			return number, nil
		}
	}
	return "", errors.New("invalid ZCode reset timestamp")
}
