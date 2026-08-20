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

type usageEnvelope struct {
	Status   string        `json:"status"`
	NumTurns int           `json:"num_turns"`
	Usage    *usageTotals  `json:"usage"`
	Command  *usageCommand `json:"command"`
}

type usageTotals struct {
	TotalTokens int64 `json:"total_tokens"`
}

type usageCommand struct {
	Name string    `json:"name"`
	Data usageData `json:"data"`
}

type usageData struct {
	Groups []usageGroup `json:"groups"`
}

type usageGroup struct {
	Buckets []usageBucket `json:"buckets"`
}

type usageBucket struct {
	ID                string   `json:"id"`
	RemainingFraction *float64 `json:"remaining_fraction"`
	ResetTime         string   `json:"reset_time"`
}

func parseUsage(data []byte) (quota.Result, error) {
	var envelope usageEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return quota.Result{}, errors.New("decode usage output")
	}
	if envelope.Status != "SUCCESS" || envelope.NumTurns != 0 {
		return quota.Result{}, errors.New("unsafe usage status")
	}
	if envelope.Usage == nil || envelope.Usage.TotalTokens != 0 {
		return quota.Result{}, errors.New("unsafe usage token count")
	}
	if envelope.Command == nil || envelope.Command.Name != "usage" {
		return quota.Result{}, errors.New("unexpected usage command")
	}

	windows := make(map[quota.WindowName]quota.Window, 2)
	counts := map[string]int{
		geminiFiveHourBucketID: 0,
		geminiWeeklyBucketID:   0,
	}
	for _, group := range envelope.Command.Data.Groups {
		for _, bucket := range group.Buckets {
			var name quota.WindowName
			switch bucket.ID {
			case geminiFiveHourBucketID:
				name = quota.Window5Hour
			case geminiWeeklyBucketID:
				name = quota.Window7Day
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
