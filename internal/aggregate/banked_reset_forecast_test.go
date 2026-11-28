package aggregate

import (
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/quota"
)

func TestCodexForecastBridgesGapWithScheduledRefill(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC).Unix()
	hour := int64(time.Hour / time.Second)
	a := codexForecastAccount("a", 30, now+136*hour)
	b := codexForecastAccount("b", 0, now+136*hour)
	b.BankedResets = &quota.BankedResetForecast{ScheduledAtUnix: []int64{now + 4*hour}}
	current := []quota.Result{a, b}
	baseline, _ := Compute([]quota.Result{a, codexForecastAccount("b", 0, now+136*hour)}, now, "codex", nil)
	forecast, _ := Compute(current, now, "codex", nil)
	before, after := baseline[quota.Window7Day], forecast[quota.Window7Day]
	if before.GapStartS <= 4*hour || before.GapStartS > 6*hour {
		t.Fatalf("fixture did not show the expected gap before the later natural reset: %+v", before)
	}
	if after.ScheduledResetCount != 1 || after.NextScheduledResetAtUnix != now+4*hour ||
		after.GapStartS <= 20*hour || after.Burndown != after.GapStartS || after.RemainingPct != before.RemainingPct ||
		after.GaugePos >= 3 || after.PaceAdvice != quota.PaceKeep {
		t.Fatalf("scheduled refill failed to move the first gap without changing current quota: %+v", after)
	}
}

func TestCodexForecastAdvisesForTheIntervalBeforeTheNextRefill(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC).Unix()
	hour := int64(time.Hour / time.Second)
	a := codexForecastAccount("a", 30, now+136*hour)
	b := codexForecastAccount("b", 0, now+136*hour)
	a.BankedResets = &quota.BankedResetForecast{ScheduledAtUnix: []int64{now + 4*hour}}
	windows, _ := Compute([]quota.Result{a, b}, now, "codex", nil)
	r := windows[quota.Window7Day]
	if r.GapStartS <= 4*hour || r.PaceAdvice != quota.PaceBurnHarder ||
		r.RemainingAtNextResetPct == nil || *r.RemainingAtNextResetPct != 9 ||
		r.PaceTargetPct == nil || *r.PaceTargetPct != 141 {
		t.Fatalf("save quota before the refill even though a later gap is projected: %+v", r)
	}
	a.BankedResets.ScheduledAtUnix[0] = now + 8*hour
	windows, _ = Compute([]quota.Result{a, b}, now, "codex", nil)
	r = windows[quota.Window7Day]
	if r.GapStartS >= 8*hour || r.PaceAdvice != quota.PaceSlowDown ||
		r.RemainingAtNextResetPct == nil || *r.RemainingAtNextResetPct != 0 ||
		r.PaceTargetPct == nil || *r.PaceTargetPct != 70 {
		t.Fatalf("slow down when current quota will run out before the refill: %+v", r)
	}
}

func TestCodexForecastKeepsBurnWindowBetweenConsecutiveRefills(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC).Unix()
	hour := int64(time.Hour / time.Second)
	a := codexForecastAccount("a", 60, now+120*hour)
	a.BankedResets = &quota.BankedResetForecast{LastRedeemedAtUnix: now - 6*hour, ScheduledAtUnix: []int64{now + 20*hour}}
	b := codexForecastAccount("b", 100, now+120*hour)
	windows, _ := Compute([]quota.Result{a, b}, now, "codex", nil)
	w := windows[quota.Window7Day]
	if w.ExpectedPct != 74 || w.PaceDiff != 6 || w.RemainingPct != 80 {
		t.Fatalf("pace should use 20h left in a 26h refill interval alongside the other account: %+v", w)
	}
}

func TestCodexForecastAppliesEveryScheduledRefillToWaste(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC).Unix()
	hour := int64(time.Hour / time.Second)
	a := codexForecastAccount("a", 70, now+144*hour)
	b := codexForecastAccount("b", 100, now+144*hour)
	a.BankedResets = &quota.BankedResetForecast{ScheduledAtUnix: []int64{now + 4*hour}}
	one, _ := Compute([]quota.Result{a, b}, now, "codex", nil)
	a.BankedResets = &quota.BankedResetForecast{ScheduledAtUnix: []int64{now + 4*hour, now + 30*hour}}
	both, _ := Compute([]quota.Result{a, b}, now, "codex", nil)
	first, second := one[quota.Window7Day], both[quota.Window7Day]
	if second.ScheduledResetCount != 2 || second.GapDurationS != 0 || second.WasteDeadlineS != 4*hour ||
		second.WastedPct <= first.WastedPct {
		t.Fatalf("the second refill did not account for discarded quota after 26h: first %+v, both %+v", first, second)
	}
}

func TestCodexScheduledRefillClearsWeeklyGateForSharedSessionOnly(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC).Unix()
	hour := int64(time.Hour / time.Second)
	a := codexForecastAccount("a", 0, now+136*hour)
	b := codexForecastAccount("b", 0, now+136*hour)
	const scoped = quota.WindowName("5h:codex-special")
	for _, r := range []quota.Result{a, b} {
		r.Windows[quota.Window5Hour] = quota.Window{RemainingPct: 50, ResetAtUnix: now + 3*hour}
		r.Windows[scoped] = r.Windows[quota.Window5Hour]
	}
	baseline, _ := Compute([]quota.Result{a, b}, now, "codex", nil)
	a.BankedResets = &quota.BankedResetForecast{ScheduledAtUnix: []int64{now + hour}}
	windows, _ := Compute([]quota.Result{a, b}, now, "codex", nil)
	shared, model := windows[quota.Window5Hour], windows[scoped]
	if shared.RemainingPct != 0 || shared.GapStartS != 0 || shared.GapDurationS != hour || shared.ScheduledResetCount != 1 ||
		shared.PaceAdvice != quota.PaceNeedsRefill {
		t.Fatalf("shared session must stay exhausted until the scheduled weekly refill: %+v", shared)
	}
	if model != baseline[scoped] {
		t.Fatalf("scheduled shared refill must not credit a model-scoped window: %+v", model)
	}
}

func TestCodexForecastShowsNoGapWhenScheduledRefillBridgesWholeHorizon(t *testing.T) {
	now := time.Date(2026, 11, 28, 4, 0, 0, 0, time.UTC).Unix()
	hour := int64(time.Hour / time.Second)
	a := codexForecastAccount("a", 50, now+3*hour)
	b := codexForecastAccount("b", 0, now+2*hour)
	for _, r := range []quota.Result{a, b} {
		r.Windows[quota.Window5Hour] = r.Windows[quota.Window7Day]
		delete(r.Windows, quota.Window7Day)
	}
	baseline, _ := Compute([]quota.Result{a, b}, now, "codex", nil)
	b.BankedResets = &quota.BankedResetForecast{ScheduledAtUnix: []int64{now + hour/2}}
	forecast, _ := Compute([]quota.Result{a, b}, now, "codex", nil)
	w := forecast[quota.Window5Hour]
	if baseline[quota.Window5Hour].GapDurationS == 0 || w.ScheduledResetCount != 1 ||
		w.GapDurationS != 0 || w.Burndown != 0 || w.GaugePos < 3 {
		t.Fatalf("planned refill must cover the baseline gap: baseline %+v, forecast %+v", baseline, w)
	}
}

func codexForecastAccount(key string, remaining int, reset int64) quota.Result {
	return quota.Result{
		AccountKey: key, Plan: "pro", RateLimitTier: "codex_pro_20x", Status: quota.StatusOK,
		Windows: map[quota.WindowName]quota.Window{quota.Window7Day: {RemainingPct: remaining, ResetAtUnix: reset}},
	}
}
