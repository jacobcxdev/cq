package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	PollInterval  time.Duration
	OnError       func(error)
	Gateway       *zcode.Gateway
	ProjectTokens *zcode.ProjectTokenClient

	fs       fsutil.FileSystem
	path     string
	client   httputil.Doer
	accounts func() ([]zaiprov.Account, error)
	models   func() []modelregistry.Entry
	runMu    sync.Mutex
	mu       sync.Mutex
	records  map[string]ZAIPrimerRecord
	lastErr  string
}

// ZAIPrimerRecord contains public account identity and dispatch state only.
type ZAIPrimerRecord struct {
	State             string `json:"state"`
	AttemptedAtUnix   int64  `json:"attempted_at_unix"`
	NextAttemptAtUnix int64  `json:"next_attempt_at_unix"`
	ResetAtUnix       int64  `json:"reset_at_unix,omitempty"`
}

type zaiPrimerJournal struct {
	SchemaVersion int                        `json:"schema_version"`
	Records       map[string]ZAIPrimerRecord `json:"records"`
}

type ZAIPrimerHealth struct {
	Configured bool                       `json:"configured"`
	Owner      bool                       `json:"owner"`
	Accounts   map[string]ZAIPrimerRecord `json:"accounts,omitempty"`
	LastError  string                     `json:"last_error,omitempty"`
}

// NewZAIPrimer reads the restart-safe dispatch journal without creating state.
// RunOnce serialises overlapping workers through one filesystem lock.
func NewZAIPrimer(fsys fsutil.FileSystem, stateDir string, client httputil.Doer, accounts func() ([]zaiprov.Account, error), models func() []modelregistry.Entry) (*ZAIPrimer, error) {
	if fsys == nil || stateDir == "" || client == nil || accounts == nil || models == nil {
		return nil, errors.New("Z.ai primer dependencies unavailable")
	}
	p := &ZAIPrimer{fs: fsys, path: filepath.Join(stateDir, "zai-window-primer.json"), client: client, accounts: accounts, models: models, records: make(map[string]ZAIPrimerRecord)}
	if err := p.reloadJournal(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *ZAIPrimer) reloadJournal() error {
	data, err := p.fs.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		p.mu.Lock()
		p.records = make(map[string]ZAIPrimerRecord)
		p.mu.Unlock()
		return nil
	}
	if err != nil {
		return errors.New("read Z.ai primer journal")
	}
	var journal zaiPrimerJournal
	if len(data) > httputil.MaxResponseBody || json.Unmarshal(data, &journal) != nil || journal.SchemaVersion != 1 || journal.Records == nil {
		return errors.New("invalid Z.ai primer journal")
	}
	for id, record := range journal.Records {
		if !zaiPrimerAccountID(id) || !zaiPrimerRecordValid(record) {
			return errors.New("invalid Z.ai primer journal")
		}
	}
	p.mu.Lock()
	p.records = journal.Records
	p.mu.Unlock()
	return nil
}

func (p *ZAIPrimer) Health(configured bool) ZAIPrimerHealth {
	health := ZAIPrimerHealth{Configured: configured, Owner: p != nil}
	if p == nil {
		return health
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	health.LastError = p.lastErr
	health.Accounts = make(map[string]ZAIPrimerRecord, len(p.records))
	for id, record := range p.records {
		health.Accounts[id] = record
	}
	return health
}

func (p *ZAIPrimer) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := p.RunOnce(ctx, time.Now())
		if err != nil && ctx.Err() == nil && p.OnError != nil {
			p.OnError(err)
		}
		wait := min(time.Until(next), p.pollInterval())
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RunOnce observes each account independently. A persisted intent precedes any
// POST, so a crash or ambiguous response cannot repeatedly consume quota.
func (p *ZAIPrimer) RunOnce(ctx context.Context, now time.Time) (next time.Time, runErr error) {
	if p == nil {
		return time.Time{}, errors.New("Z.ai primer unavailable")
	}
	p.runMu.Lock()
	defer p.runMu.Unlock()
	next = now.Add(p.pollInterval())
	defer func() {
		if recover() != nil {
			runErr = errors.New("Z.ai primer failed")
		}
		p.mu.Lock()
		p.lastErr = ""
		if runErr != nil {
			p.lastErr = runErr.Error()
		}
		p.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return next, err
	}
	if err := fsutil.EnsureSecureDirectory(p.fs, filepath.Dir(p.path)); err != nil {
		return next, errors.New("prepare Z.ai primer journal directory")
	}
	lock, err := fsutil.AcquireExclusiveLock(p.fs, filepath.Join(filepath.Dir(p.path), "zai-window-primer.lock"))
	if errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		return next, nil
	}
	if err != nil {
		return next, errors.New("lock Z.ai primer journal")
	}
	defer lock.Close()
	if err := p.reloadJournal(); err != nil {
		return next, err
	}
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
	p.mu.Lock()
	record := p.records[account.ID]
	p.mu.Unlock()
	if record.NextAttemptAtUnix > now.Unix() {
		return time.Unix(record.NextAttemptAtUnix, 0), nil
	}
	record = ZAIPrimerRecord{State: "claimed", AttemptedAtUnix: now.Unix(), NextAttemptAtUnix: now.Add(zaiPrimerWindow).Unix()}
	if err := p.saveRecord(account.ID, record); err != nil {
		return time.Time{}, err
	}
	state, err := p.send(ctx, account, model)
	record.State = state
	if state == "rejected" {
		record.NextAttemptAtUnix = now.Add(p.pollInterval()).Unix()
	}
	if state == "admitted" || state == "ambiguous" {
		// Reading usage is safe even if delivery was ambiguous. A future epoch
		// proves the window is active; an unavailable epoch never causes replay.
		verified, verifyErr := p.readUsage(ctx, account)
		if verifyErr == nil && verified.Windows[quota.Window5Hour].ResetAtUnix > now.Unix() {
			record.State = "verified"
			record.ResetAtUnix = verified.Windows[quota.Window5Hour].ResetAtUnix
			record.NextAttemptAtUnix = record.ResetAtUnix
			err = nil
		} else if err == nil {
			err = errors.New("Z.ai primer window activation unverified")
		}
	}
	if saveErr := p.saveRecord(account.ID, record); saveErr != nil {
		return time.Time{}, saveErr
	}
	return time.Unix(record.NextAttemptAtUnix, 0), err
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
	if err != nil {
		return "ambiguous", errors.New("Z.ai primer request delivery unknown")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		data, readErr := httputil.ReadBody(response.Body)
		if readErr != nil {
			data = nil
		}
		return "rejected", zaiPrimerRejection(response.StatusCode, data)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "ambiguous", errors.New("Z.ai primer request outcome unknown")
	}
	data, err := httputil.ReadBody(response.Body)
	if err != nil || !json.Valid(data) {
		return "ambiguous", errors.New("read Z.ai primer response")
	}
	var result struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Error) > 0 && string(result.Error) != "null" {
		return "ambiguous", errors.New("Z.ai primer response outcome unknown")
	}
	return "admitted", nil
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

func (p *ZAIPrimer) saveRecord(id string, record ZAIPrimerRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	records := make(map[string]ZAIPrimerRecord, len(p.records)+1)
	for key, value := range p.records {
		records[key] = value
	}
	records[id] = record
	data, err := json.Marshal(zaiPrimerJournal{SchemaVersion: 1, Records: records})
	if err != nil || fsutil.SecureAtomicWrite(p.fs, p.path, append(data, '\n')) != nil {
		return errors.New("save Z.ai primer journal")
	}
	p.records = records
	return nil
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

func zaiPrimerRecordValid(record ZAIPrimerRecord) bool {
	switch record.State {
	case "claimed", "rejected", "admitted", "ambiguous", "verified":
		return record.AttemptedAtUnix > 0 && record.NextAttemptAtUnix >= record.AttemptedAtUnix && record.ResetAtUnix >= 0
	default:
		return false
	}
}
