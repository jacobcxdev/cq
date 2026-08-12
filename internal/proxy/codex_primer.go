package proxy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const defaultCodexPrimerPollInterval = 5 * time.Minute
const defaultCodexPrimerDormantProbeInterval = 5 * time.Second

type PrimerUsageReader interface {
	Read(context.Context, codex.AccountKey) (codex.UsageObservation, error)
}

type PrimerRequester interface {
	Send(context.Context, codex.AccountKey, string) (PrimerRequestResult, error)
}

type CodexPrimer struct {
	Accounts             func(context.Context) ([]codex.AccountKey, error)
	Usage                PrimerUsageReader
	Requester            PrimerRequester
	Store                *CodexPrimerStore
	Models               func() []modelregistry.Entry
	ModelOverrides       map[string]string
	PollInterval         time.Duration
	DormantProbeInterval time.Duration
	OnError              func(error)

	healthMu       sync.Mutex
	lastError      string
	dormantMu      sync.Mutex
	dormantSamples map[string]primerDormantSample
}

type primerDormantSample struct {
	ResetAt    time.Time
	ObservedAt time.Time
}

type CodexPrimerHealth struct {
	Configured bool                `json:"configured"`
	Owner      bool                `json:"owner"`
	Counts     map[PrimerState]int `json:"counts,omitempty"`
	LastError  string              `json:"last_error,omitempty"`
}

func (p *CodexPrimer) Health(configured bool) CodexPrimerHealth {
	health := CodexPrimerHealth{Configured: configured, Owner: p != nil}
	if p == nil || p.Store == nil {
		return health
	}
	health.Counts = make(map[PrimerState]int)
	for _, record := range p.Store.Records() {
		health.Counts[record.State]++
	}
	p.healthMu.Lock()
	health.LastError = p.lastError
	p.healthMu.Unlock()
	return health
}

func (p *CodexPrimer) RunOnce(ctx context.Context, now time.Time) (time.Time, error) {
	if p == nil || p.Accounts == nil || p.Usage == nil || p.Requester == nil || p.Store == nil || p.Models == nil {
		return time.Time{}, errors.New("Codex primer unavailable")
	}
	accounts, err := p.Accounts(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("list Codex primer accounts: %w", err)
	}
	var next time.Time
	var runErrors []error
	for _, account := range accounts {
		accountNext, accountErr := p.runAccount(ctx, account, now)
		if accountErr != nil {
			runErrors = append(runErrors, accountErr)
		}
		if !accountNext.IsZero() && (next.IsZero() || accountNext.Before(next)) {
			next = accountNext
		}
	}
	if next.IsZero() {
		next = now.Add(p.pollInterval())
	}
	return next, errors.Join(runErrors...)
}

func (p *CodexPrimer) Run(ctx context.Context) error {
	for {
		next, err := p.RunOnce(ctx, time.Now())
		if err != nil && ctx.Err() == nil {
			p.setLastError(err)
			if p.OnError != nil {
				p.OnError(err)
			}
		} else if err == nil {
			p.setLastError(nil)
		}
		wait := time.Until(next)
		if poll := p.pollInterval(); wait > poll {
			wait = poll
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *CodexPrimer) setLastError(err error) {
	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	p.lastError = ""
	if err != nil {
		p.lastError = err.Error()
	}
}

func (p *CodexPrimer) runAccount(ctx context.Context, account codex.AccountKey, now time.Time) (time.Time, error) {
	observation, err := p.Usage.Read(ctx, account)
	if err != nil {
		return time.Time{}, fmt.Errorf("read Codex primer usage: %w", err)
	}
	targets, unresolved := PlanCodexPrimerTargets(observation.Windows, p.ModelOverrides, p.Models())
	if err := p.Store.ReconcileAdvanced(account, primerResetEpochs(observation.Windows), now); err != nil {
		return time.Time{}, err
	}
	next := time.Time{}
	dormantNext, err := p.Store.ReconcileDormant(account, targets, now, now.Add(p.dormantProbeInterval()))
	if err != nil {
		return time.Time{}, err
	}
	if !dormantNext.IsZero() {
		next = dormantNext
	}
	for _, target := range targets {
		if err := p.Store.Observe(account, target); err != nil {
			return time.Time{}, err
		}
		dormant := false
		if target.ResetAt.After(now) {
			var probeAt time.Time
			dormant, probeAt = p.dormantDue(account, target, now)
			if !dormant && !probeAt.IsZero() && (next.IsZero() || probeAt.Before(next)) {
				next = probeAt
			}
		}
		if target.ResetAt.After(now) && !dormant {
			if next.IsZero() || target.ResetAt.Before(next) {
				next = target.ResetAt
			}
			continue
		}
		var claimed bool
		if dormant {
			claimed, err = p.Store.ClaimDormant(account, target, now)
		} else {
			claimed, err = p.Store.Claim(account, target)
		}
		if err != nil {
			return time.Time{}, err
		}
		if !claimed {
			continue
		}
		result, err := p.Requester.Send(ctx, account, target.ModelID)
		if err != nil {
			code := "request_error"
			if dormant {
				code = "dormant_ambiguous"
			}
			if markErr := p.Store.Mark(account, target, PrimerStateAmbiguous, code); markErr != nil {
				return time.Time{}, errors.Join(err, markErr)
			}
			return time.Time{}, err
		}
		switch result.State {
		case PrimerRequestRejected:
			code := fmt.Sprintf("http_%d", result.HTTPStatus)
			if dormant {
				code = "dormant_" + code
			}
			if err := p.Store.Mark(account, target, PrimerStateRejected, code); err != nil {
				return time.Time{}, err
			}
		case PrimerRequestAdmitted:
			code := "admitted"
			if dormant {
				code = "dormant_admitted"
			}
			if err := p.Store.Mark(account, target, PrimerStateAdmitted, code); err != nil {
				return time.Time{}, err
			}
		case PrimerRequestAmbiguous:
			code := "ambiguous"
			if dormant {
				code = "dormant_ambiguous"
			}
			if err := p.Store.Mark(account, target, PrimerStateAmbiguous, code); err != nil {
				return time.Time{}, err
			}
		default:
			code := "unknown_result"
			if dormant {
				code = "dormant_ambiguous"
			}
			if err := p.Store.Mark(account, target, PrimerStateAmbiguous, code); err != nil {
				return time.Time{}, err
			}
		}
		verified, err := p.Usage.Read(ctx, account)
		if err != nil {
			record, found := p.Store.Lookup(account, target)
			if found && !dormant && (record.State == PrimerStateAdmitted || record.State == PrimerStateAmbiguous) {
				if markErr := p.Store.Mark(account, target, PrimerStateVerifying, "usage_unavailable"); markErr != nil {
					return time.Time{}, errors.Join(err, markErr)
				}
			}
			return time.Time{}, err
		}
		verifiedTargets, verifiedUnresolved := PlanCodexPrimerTargets(verified.Windows, p.ModelOverrides, p.Models())
		unresolved = append(unresolved, verifiedUnresolved...)
		if err := p.Store.ReconcileAdvanced(account, primerResetEpochs(verified.Windows), now); err != nil {
			return time.Time{}, err
		}
		if dormant {
			record, recordFound := p.Store.Lookup(account, target)
			if expected, found := matchingPrimerTarget(target, verifiedTargets); found && recordFound && (record.State == PrimerStateAdmitted || record.State == PrimerStateAmbiguous || record.State == PrimerStateVerifying) {
				if err := p.Store.MarkDormantVerifying(account, target, expected, now.Add(p.dormantProbeInterval())); err != nil {
					return time.Time{}, err
				}
				p.rememberDormantSample(account, expected, now)
				if next.IsZero() || now.Add(p.dormantProbeInterval()).Before(next) {
					next = now.Add(p.dormantProbeInterval())
				}
			}
		} else if record, found := p.Store.Lookup(account, target); found && (record.State == PrimerStateAdmitted || record.State == PrimerStateAmbiguous) {
			if err := p.Store.Mark(account, target, PrimerStateVerifying, "epoch_unchanged"); err != nil {
				return time.Time{}, err
			}
		}
		for _, verifiedTarget := range verifiedTargets {
			if err := p.Store.Observe(account, verifiedTarget); err != nil {
				return time.Time{}, err
			}
			if verifiedTarget.ResetAt.After(now) && (next.IsZero() || verifiedTarget.ResetAt.Before(next)) {
				next = verifiedTarget.ResetAt
			}
		}
	}
	if len(unresolved) != 0 {
		return next, fmt.Errorf("%d Codex primer windows unresolved", len(unresolved))
	}
	return next, nil
}

func (p *CodexPrimer) dormantDue(account codex.AccountKey, target CodexPrimerTarget, now time.Time) (bool, time.Time) {
	probe := p.dormantProbeInterval()
	tolerance := 2 * time.Second
	if len(target.Windows) == 0 {
		return false, time.Time{}
	}
	for _, window := range target.Windows {
		if window.RemainingPct != 100 || window.Period <= 0 || absDuration(target.ResetAt.Sub(now)-window.Period) > tolerance {
			return false, time.Time{}
		}
	}
	key := primerDormantKey(account, target)
	p.dormantMu.Lock()
	defer p.dormantMu.Unlock()
	if p.dormantSamples == nil {
		p.dormantSamples = make(map[string]primerDormantSample)
	}
	previous, found := p.dormantSamples[key]
	p.dormantSamples[key] = primerDormantSample{ResetAt: target.ResetAt, ObservedAt: now}
	if !found {
		return false, now.Add(probe)
	}
	shift := target.ResetAt.Sub(previous.ResetAt)
	elapsed := now.Sub(previous.ObservedAt)
	if shift > 0 && elapsed > 0 && absDuration(shift-elapsed) <= tolerance {
		delete(p.dormantSamples, key)
		return true, time.Time{}
	}
	return false, time.Time{}
}

func (p *CodexPrimer) rememberDormantSample(account codex.AccountKey, target CodexPrimerTarget, now time.Time) {
	p.dormantMu.Lock()
	defer p.dormantMu.Unlock()
	if p.dormantSamples == nil {
		p.dormantSamples = make(map[string]primerDormantSample)
	}
	p.dormantSamples[primerDormantKey(account, target)] = primerDormantSample{ResetAt: target.ResetAt, ObservedAt: now}
}

func primerDormantKey(account codex.AccountKey, target CodexPrimerTarget) string {
	return string(account) + "\x00" + strings.Join(primerWindowParts(target), ",")
}

func matchingPrimerTarget(dispatched CodexPrimerTarget, targets []CodexPrimerTarget) (CodexPrimerTarget, bool) {
	want := primerWindowParts(dispatched)
	best := -1
	for index, target := range targets {
		parts := primerWindowParts(target)
		if stringSubset(want, parts) && (best < 0 || len(parts) < len(primerWindowParts(targets[best]))) {
			best = index
		}
	}
	if best < 0 {
		return CodexPrimerTarget{}, false
	}
	return targets[best], true
}

func primerWindowParts(target CodexPrimerTarget) []string {
	parts := make([]string, 0, len(target.Windows))
	for _, window := range target.Windows {
		parts = append(parts, window.RawLimitName+"|"+string(window.WindowName)+"|"+window.Period.String())
	}
	sort.Strings(parts)
	return parts
}

func stringSubset(want, available []string) bool {
	present := make(map[string]bool, len(available))
	for _, value := range available {
		present[value] = true
	}
	for _, value := range want {
		if !present[value] {
			return false
		}
	}
	return true
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func primerResetEpochs(windows []codex.WindowDescriptor) []time.Time {
	epochs := make([]time.Time, 0, len(windows))
	seen := make(map[int64]bool, len(windows))
	for _, window := range windows {
		key := window.ResetAt.UTC().UnixNano()
		if window.ResetAt.IsZero() || seen[key] {
			continue
		}
		seen[key] = true
		epochs = append(epochs, window.ResetAt)
	}
	return epochs
}

func (p *CodexPrimer) pollInterval() time.Duration {
	if p.PollInterval > 0 {
		return p.PollInterval
	}
	return defaultCodexPrimerPollInterval
}

func (p *CodexPrimer) dormantProbeInterval() time.Duration {
	if p.DormantProbeInterval > 0 {
		return p.DormantProbeInterval
	}
	return defaultCodexPrimerDormantProbeInterval
}
