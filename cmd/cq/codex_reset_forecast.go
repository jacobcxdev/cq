package main

import (
	"context"
	"slices"
	"time"

	codex "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/quota"
)

func loadCodexResetForecast(ctx context.Context, now time.Time) map[string]quota.BankedResetForecast {
	deps, err := localCodexResetScheduleDependencies()
	if err != nil {
		return nil
	}
	return codexResetForecastFrom(ctx, now, deps.App.Schedule, deps.ExecutorStatus)
}

func codexResetForecastFrom(
	ctx context.Context,
	now time.Time,
	store *codex.ResetAutomationStore,
	executorStatus func(context.Context) proxy.CodexResetSchedulerStatus,
) map[string]quota.BankedResetForecast {
	state, err := store.Load()
	if err != nil {
		return nil
	}

	forecasts := make(map[string]quota.BankedResetForecast)
	unresolved := make(map[codex.AccountKey]bool)
	for _, entry := range state.Entries {
		if entry.State == codex.ResetAutomationRunning || entry.State == codex.ResetAutomationRetrying ||
			(entry.State == codex.ResetAutomationBlocked && entry.LastError == "consume_indeterminate") {
			unresolved[entry.AccountKey] = true
		}
		if entry.State != codex.ResetAutomationCompleted || entry.Outcome != codex.ConsumeReset ||
			entry.UpdatedAt.IsZero() || entry.UpdatedAt.After(now) {
			continue
		}
		key := string(entry.AccountKey)
		forecast := forecasts[key]
		if redeemedAt := entry.UpdatedAt.Unix(); redeemedAt > forecast.LastRedeemedAtUnix {
			forecast.LastRedeemedAtUnix = redeemedAt
			forecasts[key] = forecast
		}
	}

	if !state.Enabled {
		return forecasts
	}
	adaptiveReady := freshCodexAdaptivePlan(state, now)
	upcoming := make(map[string][]int64)
	for _, entry := range state.Entries {
		if entry.State != codex.ResetAutomationPending || entry.At == nil || entry.At.Before(now) ||
			unresolved[entry.AccountKey] ||
			(state.Mode == codex.ResetAutomationAdaptive && !entry.Pinned && !adaptiveReady) {
			continue
		}
		if (entry.ExpiresAt != nil && !entry.At.Before(*entry.ExpiresAt)) ||
			(entry.NextAttemptAt != nil && entry.NextAttemptAt.After(*entry.At)) {
			continue
		}
		key := string(entry.AccountKey)
		upcoming[key] = append(upcoming[key], entry.At.Unix())
	}

	if len(upcoming) == 0 {
		return forecasts
	}
	status := executorStatus(ctx)
	if !status.Running || status.State != "running" || status.LastError != "" {
		return forecasts
	}
	for key, scheduled := range upcoming {
		slices.Sort(scheduled)
		forecast := forecasts[key]
		forecast.ScheduledAtUnix = slices.Compact(scheduled)
		forecasts[key] = forecast
	}
	return forecasts
}

func freshCodexAdaptivePlan(state codex.ResetAutomationState, now time.Time) bool {
	return state.PlanValid && state.PlanError == "" &&
		state.LastReplanAt != nil && !state.LastReplanAt.After(now) &&
		state.NextReplanAt != nil && state.NextReplanAt.After(now) &&
		now.Sub(*state.LastReplanAt) <= 5*time.Minute+45*time.Second
}
