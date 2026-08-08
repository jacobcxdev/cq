package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/modelregistry"
	codex "github.com/jacobcxdev/cq/internal/provider/codex"
)

const defaultCodexPrimerPollInterval = 5 * time.Minute

type PrimerUsageReader interface {
	Read(context.Context, codex.AccountKey) (codex.UsageObservation, error)
}

type PrimerRequester interface {
	Send(context.Context, codex.AccountKey, string) (PrimerRequestResult, error)
}

type CodexPrimer struct {
	Accounts       func(context.Context) ([]codex.AccountKey, error)
	Usage          PrimerUsageReader
	Requester      PrimerRequester
	Store          *CodexPrimerStore
	Models         func() []modelregistry.Entry
	ModelOverrides map[string]string
	PollInterval   time.Duration
	OnError        func(error)

	healthMu  sync.Mutex
	lastError string
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
	for _, target := range targets {
		if err := p.Store.Observe(account, target); err != nil {
			return time.Time{}, err
		}
		if target.ResetAt.After(now) {
			if next.IsZero() || target.ResetAt.Before(next) {
				next = target.ResetAt
			}
			continue
		}
		claimed, err := p.Store.Claim(account, target)
		if err != nil {
			return time.Time{}, err
		}
		if !claimed {
			continue
		}
		result, err := p.Requester.Send(ctx, account, target.ModelID)
		if err != nil {
			if markErr := p.Store.Mark(account, target, PrimerStateAmbiguous, "request_error"); markErr != nil {
				return time.Time{}, errors.Join(err, markErr)
			}
			return time.Time{}, err
		}
		switch result.State {
		case PrimerRequestRejected:
			if err := p.Store.Mark(account, target, PrimerStateRejected, fmt.Sprintf("http_%d", result.HTTPStatus)); err != nil {
				return time.Time{}, err
			}
		case PrimerRequestAdmitted:
			if err := p.Store.Mark(account, target, PrimerStateAdmitted, "admitted"); err != nil {
				return time.Time{}, err
			}
		case PrimerRequestAmbiguous:
			if err := p.Store.Mark(account, target, PrimerStateAmbiguous, "ambiguous"); err != nil {
				return time.Time{}, err
			}
		default:
			if err := p.Store.Mark(account, target, PrimerStateAmbiguous, "unknown_result"); err != nil {
				return time.Time{}, err
			}
		}
		verified, err := p.Usage.Read(ctx, account)
		if err != nil {
			record, found := p.Store.Lookup(account, target)
			if found && (record.State == PrimerStateAdmitted || record.State == PrimerStateAmbiguous) {
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
		if record, found := p.Store.Lookup(account, target); found && (record.State == PrimerStateAdmitted || record.State == PrimerStateAmbiguous) {
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
