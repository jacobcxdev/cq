package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
	"github.com/jacobcxdev/cq/internal/modelregistry"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
	"github.com/jacobcxdev/cq/internal/quota"
)

const zaiPrimerWindow = 5 * time.Hour
const zaiPrimerPoll = 5 * time.Minute

// ZAIPrimer starts dormant five-hour Coding Plan windows. Weekly windows are
// subscription-anchored and need no synthetic request.
type ZAIPrimer struct {
	*subscriptionPrimerJournal
	PollInterval  time.Duration
	OnError       func(error)
	Gateway       *zcode.Gateway
	ProjectTokens *zcode.ProjectTokenClient

	client   httputil.Doer
	accounts func() ([]zaiprov.Account, error)
	models   func() []modelregistry.Entry
}

// ZAIPrimerRecord contains public account identity and dispatch state only.
type ZAIPrimerRecord = subscriptionPrimerRecord

type ZAIPrimerHealth = subscriptionPrimerHealth

// NewZAIPrimer reads the restart-safe dispatch journal without creating state.
// RunOnce serialises overlapping workers through one filesystem lock.
func NewZAIPrimer(fsys fsutil.FileSystem, stateDir string, client httputil.Doer, accounts func() ([]zaiprov.Account, error), models func() []modelregistry.Entry) (*ZAIPrimer, error) {
	if fsys == nil || stateDir == "" || client == nil || accounts == nil || models == nil {
		return nil, errors.New("Z.ai primer dependencies unavailable")
	}
	journal, err := newSubscriptionPrimerJournal(fsys, stateDir, "zai-window-primer", "Z.ai", zaiPrimerAccountID)
	if err != nil {
		return nil, err
	}
	return &ZAIPrimer{subscriptionPrimerJournal: journal, client: client, accounts: accounts, models: models}, nil
}

func (p *ZAIPrimer) Health(configured bool) ZAIPrimerHealth {
	if p == nil {
		return ZAIPrimerHealth{Configured: configured}
	}
	return p.snapshot(configured)
}

func (p *ZAIPrimer) Run(ctx context.Context) error {
	return runSubscriptionPrimer(ctx, p.pollInterval, p.RunOnce, p.OnError)
}

// RunOnce observes each account independently. A persisted intent precedes any
// POST, so a crash or ambiguous response cannot repeatedly consume quota.
func (p *ZAIPrimer) RunOnce(ctx context.Context, now time.Time) (time.Time, error) {
	if p == nil {
		return time.Time{}, errors.New("Z.ai primer unavailable")
	}
	return p.withJournal(ctx, now, p.pollInterval(), func() (time.Time, error) {
		next := now.Add(p.pollInterval())
		accounts, err := p.accounts()
		if err != nil {
			return next, errors.New("read Z.ai primer accounts")
		}
		var model string
		for _, account := range accounts {
			if account.Transport != "zcode-start" {
				model, err = zaiPrimerModel(p.models())
				if err != nil {
					return next, err
				}
				break
			}
		}
		var failures []error
		seen := make(map[string]bool, len(accounts))
		for _, account := range accounts {
			// Start Plan pools have provider-defined token periods, not dormant
			// five-hour Coding Plan windows to prime.
			if account.Transport == "zcode-start" {
				continue
			}
			if seen[account.ID] {
				continue
			}
			seen[account.ID] = true
			accountNext, err := p.runAccount(ctx, now, account, model)
			if err != nil {
				failures = append(failures, err)
			}
			if accountNext.After(now) && accountNext.Before(next) {
				next = accountNext
			}
			if ctx.Err() != nil {
				break
			}
		}
		return next, errors.Join(failures...)
	})
}

func (p *ZAIPrimer) runAccount(ctx context.Context, now time.Time, account zaiprov.Account, model string) (next time.Time, runErr error) {
	defer func() {
		if recover() != nil {
			runErr = errors.New("Z.ai primer account failed")
		}
	}()
	if !zaiPrimerAccountID(account.ID) {
		return time.Time{}, errors.New("invalid Z.ai primer account")
	}
	observed, err := p.readUsage(ctx, account)
	if err != nil {
		return time.Time{}, err
	}
	five, hasFive := observed.Windows[quota.Window5Hour]
	weekly, hasWeekly := observed.Windows[quota.Window7Day]
	if five.ResetAtUnix > now.Unix() {
		return time.Unix(five.ResetAtUnix, 0), nil
	}
	if !hasFive || !hasWeekly || zaiPrimerRemaining(five) != 100 || zaiPrimerRemaining(weekly) <= 0 {
		return time.Time{}, nil
	}
	return p.dispatch(ctx, now, account.ID, zaiPrimerWindow, p.pollInterval(), func() (string, error) {
		return p.send(ctx, account, model)
	}, func() (int64, error) {
		verified, err := p.readUsage(ctx, account)
		return verified.Windows[quota.Window5Hour].ResetAtUnix, err
	})
}

func (p *ZAIPrimer) readUsage(ctx context.Context, account zaiprov.Account) (quota.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	provider := zaiprov.NewAccounts(p.client, func() ([]zaiprov.Account, error) { return []zaiprov.Account{account}, nil })
	results, err := provider.Fetch(ctx, time.Now())
	if err != nil || len(results) != 1 || !results[0].IsUsable() {
		return quota.Result{}, errors.New("read Z.ai primer usage")
	}
	return results[0], nil
}

func (p *ZAIPrimer) send(ctx context.Context, account zaiprov.Account, model string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{
		"model": model, "input": "Reply with OK.", "max_output_tokens": 16,
		"store": false, "stream": false, "reasoning": map[string]string{"effort": "low"},
	})
	credential, err := account.Credential(ctx, p.ProjectTokens)
	if err != nil {
		return "rejected", errors.New("resolve Z.ai primer credential")
	}
	var response *http.Response
	if account.Transport == "zcode" {
		if p.Gateway == nil {
			return "rejected", errors.New("ZCode primer gateway unavailable")
		}
		auth := zcode.GatewayAuth{Credential: credential}
		if account.OAuth != nil {
			auth.ZCodeJWT = account.OAuth.ZCodeJWT
			auth.DeviceMid = account.OAuth.DeviceMid
		}
		response, err = p.Gateway.Do(ctx, body, auth, "cq-primer-"+account.ID)
	} else if account.Transport == "zcode-start" {
		return "rejected", errors.New("Start Plan token pools do not have five-hour windows to prime")
	} else {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, zaiprov.ResponsesBaseURL+"/responses", bytes.NewReader(body))
		if err != nil {
			return "rejected", errors.New("create Z.ai primer request")
		}
		setZAIHeaders(req.Header, nil, credential)
		req.Header.Set("Accept", "application/json")
		response, err = p.client.Do(req)
	}
	return subscriptionPrimerResponse(response, err, "Z.ai", zaiPrimerRejection)
}

// Only known provider codes select fixed diagnostics. Raw error messages and
// unknown codes may contain credentials and are never returned or journalled.
func zaiPrimerRejection(status int, data []byte) error {
	var response struct {
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &response) == nil {
		var code string
		if json.Unmarshal(response.Error.Code, &code) != nil {
			code = strings.TrimSpace(string(response.Error.Code))
		}
		switch code {
		case "insufficient_quota", "1113":
			return errors.New("Z.ai inference package unavailable; check that the API key comes from Personal Coding Plan > Plan Overview")
		case "1308", "1310", "1316", "1317", "1318", "1319", "1320", "1321":
			return errors.New("Z.ai Coding Plan quota exhausted")
		}
	}
	return fmt.Errorf("Z.ai primer request rejected (HTTP %d)", status)
}

func (p *ZAIPrimer) pollInterval() time.Duration {
	if p.PollInterval > 0 && p.PollInterval < zaiPrimerPoll {
		return p.PollInterval
	}
	return zaiPrimerPoll
}

func zaiPrimerModel(entries []modelregistry.Entry) (string, error) {
	var choices []modelregistry.Entry
	for _, entry := range entries {
		if entry.Provider == modelregistry.ProviderZAI && entry.Source == modelregistry.SourceNative && entry.ID != "" && (entry.Visibility == "" || entry.Visibility == "list") {
			var flags struct {
				Supported *bool `json:"supported_in_api"`
			}
			if len(entry.Raw) > 0 && json.Unmarshal(entry.Raw, &flags) != nil || flags.Supported != nil && !*flags.Supported {
				continue
			}
			choices = append(choices, entry)
		}
	}
	if len(choices) == 0 {
		return "", errors.New("Z.ai primer native model unavailable")
	}
	sort.SliceStable(choices, func(i, j int) bool {
		flashI, flashJ := strings.Contains(strings.ToLower(choices[i].ID), "flash"), strings.Contains(strings.ToLower(choices[j].ID), "flash")
		if flashI != flashJ {
			return flashI
		}
		if choices[i].Priority != choices[j].Priority {
			return choices[i].Priority < choices[j].Priority
		}
		return choices[i].ID < choices[j].ID
	})
	return choices[0].ID, nil
}

func zaiPrimerRemaining(window quota.Window) float64 {
	if window.RemainingPctExact != nil {
		return *window.RemainingPctExact
	}
	return float64(window.RemainingPct)
}

func zaiPrimerAccountID(id string) bool {
	if len(id) != 20 || !strings.HasPrefix(id, "zai_") {
		return false
	}
	_, err := hex.DecodeString(id[4:])
	return err == nil
}
