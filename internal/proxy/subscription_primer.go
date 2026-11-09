package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/httputil"
)

// subscriptionPrimerJournal shares dispatch ownership, not provider eligibility
// or credentials. Each provider has its own file, lock and account validator.
type subscriptionPrimerJournal struct {
	fs          fsutil.FileSystem
	path        string
	lockPath    string
	displayName string
	validID     func(string) bool

	runMu   sync.Mutex
	mu      sync.Mutex
	records map[string]subscriptionPrimerRecord
	lastErr string
}

type subscriptionPrimerRecord struct {
	State             string `json:"state"`
	AttemptedAtUnix   int64  `json:"attempted_at_unix"`
	NextAttemptAtUnix int64  `json:"next_attempt_at_unix"`
	ResetAtUnix       int64  `json:"reset_at_unix,omitempty"`
}

type subscriptionPrimerEnvelope struct {
	SchemaVersion int                                 `json:"schema_version"`
	Records       map[string]subscriptionPrimerRecord `json:"records"`
}

type subscriptionPrimerHealth struct {
	Configured bool                                `json:"configured"`
	Owner      bool                                `json:"owner"`
	Accounts   map[string]subscriptionPrimerRecord `json:"accounts,omitempty"`
	LastError  string                              `json:"last_error,omitempty"`
}

// Construction only reads existing state. Enabling or inspecting a primer
// never creates its journal directory or consumes subscription allowance.
func newSubscriptionPrimerJournal(fsys fsutil.FileSystem, stateDir, stem, displayName string, validID func(string) bool) (*subscriptionPrimerJournal, error) {
	if fsys == nil || stateDir == "" || validID == nil {
		return nil, fmt.Errorf("%s primer dependencies unavailable", displayName)
	}
	p := &subscriptionPrimerJournal{
		fs: fsys, path: filepath.Join(stateDir, stem+".json"), lockPath: filepath.Join(stateDir, stem+".lock"),
		displayName: displayName, validID: validID, records: make(map[string]subscriptionPrimerRecord),
	}
	if err := p.reloadJournal(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *subscriptionPrimerJournal) reloadJournal() error {
	data, err := p.fs.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		p.mu.Lock()
		p.records = make(map[string]subscriptionPrimerRecord)
		p.mu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s primer journal", p.displayName)
	}
	var journal subscriptionPrimerEnvelope
	if len(data) > httputil.MaxResponseBody || json.Unmarshal(data, &journal) != nil || journal.SchemaVersion != 1 || journal.Records == nil {
		return fmt.Errorf("invalid %s primer journal", p.displayName)
	}
	for id, record := range journal.Records {
		if !p.validID(id) || !subscriptionPrimerRecordValid(record) {
			return fmt.Errorf("invalid %s primer journal", p.displayName)
		}
	}
	p.mu.Lock()
	p.records = journal.Records
	p.mu.Unlock()
	return nil
}

func (p *subscriptionPrimerJournal) snapshot(configured bool) subscriptionPrimerHealth {
	health := subscriptionPrimerHealth{Configured: configured, Owner: p != nil}
	if p == nil {
		return health
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	health.LastError = p.lastErr
	health.Accounts = make(map[string]subscriptionPrimerRecord, len(p.records))
	for id, record := range p.records {
		health.Accounts[id] = record
	}
	return health
}

func runSubscriptionPrimer(ctx context.Context, poll func() time.Duration, once func(context.Context, time.Time) (time.Time, error), onError func(error)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := once(ctx, time.Now())
		if err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		wait := min(time.Until(next), poll())
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

// withJournal serialises overlapping workers and reloads the durable dispatch
// facts while holding the provider's filesystem lock before reading accounts.
func (p *subscriptionPrimerJournal) withJournal(ctx context.Context, now time.Time, poll time.Duration, run func() (time.Time, error)) (next time.Time, runErr error) {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	next = now.Add(poll)
	defer func() {
		if recover() != nil {
			runErr = fmt.Errorf("%s primer failed", p.displayName)
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
		return next, fmt.Errorf("prepare %s primer journal directory", p.displayName)
	}
	lock, err := fsutil.AcquireExclusiveLock(p.fs, p.lockPath)
	if errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		return next, nil
	}
	if err != nil {
		return next, fmt.Errorf("lock %s primer journal", p.displayName)
	}
	defer lock.Close()
	if err := p.reloadJournal(); err != nil {
		return next, err
	}
	return run()
}

// dispatch requires the caller's withJournal lock and proven provider-specific
// eligibility. A persisted intent precedes send; uncertain outcomes retain the
// full window cooldown even across process restarts.
func (p *subscriptionPrimerJournal) dispatch(ctx context.Context, now time.Time, id string, window, poll time.Duration, send func() (string, error), verify func() (int64, error)) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	p.mu.Lock()
	record := p.records[id]
	p.mu.Unlock()
	if record.NextAttemptAtUnix > now.Unix() {
		return time.Unix(record.NextAttemptAtUnix, 0), nil
	}
	record = subscriptionPrimerRecord{State: "claimed", AttemptedAtUnix: now.Unix(), NextAttemptAtUnix: now.Add(window).Unix()}
	if err := p.saveRecord(id, record); err != nil {
		return time.Time{}, err
	}
	state, err := send()
	record.State = state
	if state == "rejected" {
		record.NextAttemptAtUnix = now.Add(poll).Unix()
	}
	if state == "admitted" || state == "ambiguous" {
		reset, verifyErr := verify()
		if verifyErr == nil && reset > now.Unix() {
			record.State = "verified"
			record.ResetAtUnix = reset
			record.NextAttemptAtUnix = reset
			err = nil
		} else if err == nil {
			err = fmt.Errorf("%s primer window activation unverified", p.displayName)
		}
	}
	if saveErr := p.saveRecord(id, record); saveErr != nil {
		return time.Time{}, saveErr
	}
	return time.Unix(record.NextAttemptAtUnix, 0), err
}

func (p *subscriptionPrimerJournal) saveRecord(id string, record subscriptionPrimerRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	records := make(map[string]subscriptionPrimerRecord, len(p.records)+1)
	for key, value := range p.records {
		records[key] = value
	}
	records[id] = record
	data, err := json.Marshal(subscriptionPrimerEnvelope{SchemaVersion: 1, Records: records})
	if err != nil || fsutil.SecureAtomicWrite(p.fs, p.path, append(data, '\n')) != nil {
		return fmt.Errorf("save %s primer journal", p.displayName)
	}
	p.records = records
	return nil
}

func subscriptionPrimerRecordValid(record subscriptionPrimerRecord) bool {
	switch record.State {
	case "claimed", "rejected", "admitted", "ambiguous", "verified":
		return record.AttemptedAtUnix > 0 && record.NextAttemptAtUnix >= record.AttemptedAtUnix && record.ResetAtUnix >= 0
	default:
		return false
	}
}

// subscriptionPrimerResponse classifies delivery conservatively. Only known
// pre-admission failures may retry before the claimed window elapses.
func subscriptionPrimerResponse(response *http.Response, err error, displayName string, rejection func(int, []byte) error) (string, error) {
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return "ambiguous", fmt.Errorf("%s primer request delivery unknown", displayName)
	}
	if response == nil || response.Body == nil {
		return "ambiguous", fmt.Errorf("%s primer response unavailable", displayName)
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
		if rejection != nil {
			return "rejected", rejection(response.StatusCode, data)
		}
		return "rejected", fmt.Errorf("%s primer request rejected (HTTP %d)", displayName, response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "ambiguous", fmt.Errorf("%s primer request outcome unknown", displayName)
	}
	data, err := httputil.ReadBody(response.Body)
	if err != nil || !json.Valid(data) {
		return "ambiguous", fmt.Errorf("read %s primer response", displayName)
	}
	var result struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Error) > 0 && string(result.Error) != "null" {
		return "ambiguous", fmt.Errorf("%s primer response outcome unknown", displayName)
	}
	return "admitted", nil
}
