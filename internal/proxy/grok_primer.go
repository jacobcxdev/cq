package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/provider/grok"
	"github.com/jacobcxdev/cq/internal/quota"
)

// GrokPrimer observes subscription windows and primes only a confirmed dormant
// five-hour timer. Scheduled weekly pools require no synthetic consumption.
type GrokPrimer struct {
	*subscriptionPrimerJournal
	Authority    grok.Authority
	PollInterval time.Duration
	OnError      func(error)
	samples      map[string]primerDormantSample
}
type GrokPrimerHealth = subscriptionPrimerHealth

func NewGrokPrimer(fsys fsutil.FileSystem, stateDir string, authority grok.Authority) (*GrokPrimer, error) {
	if authority == nil {
		return nil, errors.New("Grok primer authority unavailable")
	}
	journal, err := newSubscriptionPrimerJournal(fsys, stateDir, "grok-window-primer", "Grok", grokPrimerAccountID)
	if err != nil {
		return nil, err
	}
	return &GrokPrimer{subscriptionPrimerJournal: journal, Authority: authority, samples: make(map[string]primerDormantSample)}, nil
}
func (p *GrokPrimer) Health(configured bool) GrokPrimerHealth {
	if p == nil {
		return GrokPrimerHealth{Configured: configured}
	}
	return p.snapshot(configured)
}
func (p *GrokPrimer) pollInterval() time.Duration {
	if p.PollInterval > 0 && p.PollInterval < 5*time.Minute {
		return p.PollInterval
	}
	return 5 * time.Minute
}
func (p *GrokPrimer) Run(ctx context.Context) error {
	return runSubscriptionPrimer(ctx, p.pollInterval, p.RunOnce, p.OnError)
}

// PrimeNow keeps the first sample alive for the confirming read. A manual
// invocation must not depend on another process retaining in-memory samples.
func (p *GrokPrimer) PrimeNow(ctx context.Context) error {
	started := time.Now()
	next, err := p.RunOnce(ctx, started)
	if next.IsZero() || next.After(time.Now().Add(5*time.Second)) {
		return err
	}
	timer := time.NewTimer(max(0, time.Until(next)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		_, confirmErr := p.RunOnce(ctx, time.Now())
		return errors.Join(err, confirmErr)
	}
}
func (p *GrokPrimer) RunOnce(ctx context.Context, now time.Time) (time.Time, error) {
	if p == nil {
		return time.Time{}, errors.New("Grok primer unavailable")
	}
	started := time.Now()
	return p.withJournal(ctx, now, p.pollInterval(), func() (time.Time, error) {
		next := now.Add(p.pollInterval())
		results, err := grok.New(p.Authority).Fetch(ctx, now)
		if err != nil {
			return next, errors.New("read Grok primer usage")
		}
		var failures []error
		for _, result := range results {
			if result.AccountID == "" {
				continue
			}
			if !result.IsUsable() {
				failures = append(failures, errors.New("read Grok primer account usage"))
				continue
			}
			at, err := p.runAccount(ctx, now.Add(time.Since(started)), result)
			if err != nil {
				failures = append(failures, err)
			}
			if at.After(now) && at.Before(next) {
				next = at
			}
			if ctx.Err() != nil {
				break
			}
		}
		return next, errors.Join(failures...)
	})
}
func (p *GrokPrimer) runAccount(ctx context.Context, now time.Time, result quota.Result) (time.Time, error) {
	id := result.AccountID
	if !grokPrimerAccountID(id) {
		return time.Time{}, errors.New("invalid Grok primer account")
	}
	five, ok := result.Windows[quota.Window5Hour]
	if !ok || result.MinRemainingPct() <= 0 || five.RemainingPercent() != 100 {
		delete(p.samples, id)
		return time.Time{}, nil
	}
	reset := time.Unix(five.ResetAtUnix, 0)
	if absDuration(reset.Sub(now)-5*time.Hour) > 2*time.Second {
		delete(p.samples, id)
		return time.Time{}, nil
	}
	previous, seen := p.samples[id]
	p.samples[id] = primerDormantSample{ResetAt: reset, ObservedAt: now}
	if !seen {
		return now.Add(5 * time.Second), nil
	}
	if !movingPrimerReset(previous.ResetAt, previous.ObservedAt, reset, now) {
		return time.Time{}, nil
	}
	delete(p.samples, id)
	account := grok.AccountID(id)
	return p.dispatch(ctx, now, id, 5*time.Hour, p.pollInterval(), func() (string, error) {
		models, err := grok.FetchModels(ctx, p.Authority, account)
		if err != nil {
			return "rejected", errors.New("read Grok primer account models")
		}
		model, err := grokPrimerModel(models)
		if err != nil {
			return "rejected", err
		}
		return p.send(ctx, account, model)
	}, func() (int64, error) {
		rows, err := grok.New(p.Authority).Fetch(ctx, now)
		if err == nil {
			for _, row := range rows {
				if row.AccountID == id && row.IsUsable() {
					window, found := row.Windows[quota.Window5Hour]
					if found && window.ResetAtUnix > now.Unix() && (window.RemainingPercent() < 100 || time.Unix(window.ResetAtUnix, 0).Sub(now) < 5*time.Hour-2*time.Second) {
						return window.ResetAtUnix, nil
					}
				}
			}
		}
		return 0, errors.New("Grok primer window activation unverified")
	})
}
func (p *GrokPrimer) send(ctx context.Context, account grok.AccountID, model grok.Model) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request := map[string]any{"model": model.ID, "input": "Reply with OK.", "max_output_tokens": 16, "store": false, "stream": false}
	if len(model.ReasoningEfforts) > 0 {
		effort := model.ReasoningEfforts[0]
		for _, candidate := range model.ReasoningEfforts {
			if candidate == "low" {
				effort = candidate
				break
			}
		}
		request["reasoning"] = map[string]string{"effort": effort}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "rejected", errors.New("encode Grok primer request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, grok.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return "rejected", errors.New("create Grok primer request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := p.Authority.Do(ctx, account, req)
	return subscriptionPrimerResponse(response, err, "Grok", nil)
}
func grokPrimerModel(models []grok.Model) (grok.Model, error) {
	var choices []grok.Model
	for _, model := range models {
		if !model.Hidden && model.Backend == "responses" {
			choices = append(choices, model)
		}
	}
	if len(choices) == 0 {
		return grok.Model{}, errors.New("Grok primer account Responses model unavailable")
	}
	sort.Slice(choices, func(i, j int) bool {
		fastI, fastJ := strings.Contains(choices[i].ID, "build-fast"), strings.Contains(choices[j].ID, "build-fast")
		if fastI != fastJ {
			return fastI
		}
		return choices[i].ID < choices[j].ID
	})
	return choices[0], nil
}
func grokPrimerAccountID(id string) bool {
	if len(id) != 37 || !strings.HasPrefix(id, "grok_") {
		return false
	}
	_, err := hex.DecodeString(id[5:])
	return err == nil
}
