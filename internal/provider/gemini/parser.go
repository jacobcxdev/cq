package gemini

import (
	"encoding/json"
	"errors"
	"math"

	"github.com/jacobcxdev/cq/internal/quota"
)

const (
	antigravityAccountID   = "antigravity-cli"
	geminiFiveHourBucketID = "gemini-5h"
	geminiWeeklyBucketID   = "gemini-weekly"
)

var errGeminiQuotaUnavailable = errors.New("Gemini quota not reported by Antigravity")

type quotaSummary struct {
	Groups []usageGroup `json:"groups"`
}

type usageGroup struct {
	Buckets []usageBucket `json:"buckets"`
}

type usageBucket struct {
	ID                string   `json:"bucketId"`
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

func parseQuotaSummary(data []byte) (quota.Result, error) {
	var summary quotaSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return quota.Result{}, errors.New("decode quota summary")
	}

	windows := make(map[quota.WindowName]quota.Window, 2)
	counts := map[string]int{
		geminiFiveHourBucketID: 0,
		geminiWeeklyBucketID:   0,
	}
	thirdPartyQuota := false
	for _, group := range summary.Groups {
		for _, bucket := range group.Buckets {
			var name quota.WindowName
			switch bucket.ID {
			case geminiFiveHourBucketID:
				name = quota.Window5Hour
			case geminiWeeklyBucketID:
				name = quota.Window7Day
			case "3p-5h", "3p-weekly":
				thirdPartyQuota = true
				continue
			default:
				continue
			}
			counts[bucket.ID]++
			if bucket.RemainingFraction == nil {
				return quota.Result{}, errors.New("missing quota fraction")
			}
			fraction := max(0, min(1, *bucket.RemainingFraction))
			windows[name] = quota.Window{
				RemainingPct: int(math.Round(fraction * 100)),
				ResetAtUnix:  quota.ParseResetTime(bucket.ResetTime),
			}
		}
	}
	if counts[geminiFiveHourBucketID] == 0 && counts[geminiWeeklyBucketID] == 0 && thirdPartyQuota {
		return quota.Result{}, errGeminiQuotaUnavailable
	}
	if counts[geminiFiveHourBucketID] != 1 || counts[geminiWeeklyBucketID] != 1 {
		return quota.Result{}, errors.New("missing or duplicate Gemini quota bucket")
	}

	return quota.Result{
		AccountID: antigravityAccountID,
		Active:    true,
		Status:    quota.StatusFromWindows(windows),
		Windows:   windows,
	}, nil
}
