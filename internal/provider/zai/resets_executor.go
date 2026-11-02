package zai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// ResetExecutor is shared by manual and explicitly accepted scheduled resets.
// An account lock covers selection, confirmation, durable intent and dispatch.
type ResetExecutor struct {
	Client    ResetClient
	FS        fsutil.FileSystem
	StateDir  string
	Now       func() time.Time
	RequestID func() string
}

type ResetUseResult struct {
	AccountID       string `json:"account_id"`
	CreditID        string `json:"credit_id"`
	Window          string `json:"window"`
	Accepted        bool   `json:"accepted"`
	AlreadyAccepted bool   `json:"already_accepted,omitempty"`
	// Dispatched marks entry into the mutation boundary, conservatively.
	Dispatched bool `json:"-"`
}

var ErrResetPending = errors.New("previous Z.ai reset outcome unresolved; explicit manual retry required")

type resetAttempt struct {
	AccountID string    `json:"account_id"`
	CreditID  string    `json:"credit_id"`
	Window    string    `json:"window"`
	RequestID string    `json:"request_id"`
	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
}

func (e ResetExecutor) Use(ctx context.Context, account Account, window, creditID string, confirm func(ResetCredit) (bool, error)) (ResetUseResult, error) {
	return e.use(ctx, account, window, creditID, confirm, true)
}

// UseScheduled executes an accepted exact-card schedule without automatically
// replaying a prior manual or scheduled attempt whose outcome is unresolved.
func (e ResetExecutor) UseScheduled(ctx context.Context, account Account, window, creditID string) (ResetUseResult, error) {
	if creditID == "" {
		return ResetUseResult{}, errors.New("scheduled Z.ai reset requires an exact card")
	}
	return e.use(ctx, account, window, creditID, func(ResetCredit) (bool, error) { return true, nil }, false)
}

func (e ResetExecutor) use(ctx context.Context, account Account, window, creditID string, confirm func(ResetCredit) (bool, error), resumePending bool) (ResetUseResult, error) {
	result := ResetUseResult{AccountID: account.ID, Window: window}
	if window != "5h" && window != "7d" {
		return result, errors.New("invalid Z.ai reset window")
	}
	if e.FS == nil || e.StateDir == "" || e.Now == nil || e.RequestID == nil || confirm == nil || account.ID == "" {
		return result, errors.New("Z.ai reset executor unavailable")
	}
	digest := sha256.Sum256([]byte(account.ID))
	path := filepath.Join(e.StateDir, hex.EncodeToString(digest[:])+".json")
	if err := fsutil.EnsureSecureDirectory(e.FS, e.StateDir); err != nil {
		return result, errors.New("prepare Z.ai reset attempt directory")
	}
	lock, err := fsutil.AcquireExclusiveLock(e.FS, path+".lock")
	if err != nil {
		return result, errors.New("Z.ai reset account is busy or attempt storage unavailable")
	}
	defer lock.Close()
	var attempt resetAttempt
	data, err := e.FS.ReadFile(path)
	if err == nil {
		if len(data) > 16<<10 || json.Unmarshal(data, &attempt) != nil || attempt.AccountID != account.ID || attempt.CreditID == "" || attempt.RequestID == "" || attempt.StartedAt.IsZero() || attempt.Window != "5h" && attempt.Window != "7d" || attempt.State != "pending" && attempt.State != "completed" && attempt.State != "rejected" && attempt.State != "unavailable" {
			return result, errors.New("invalid Z.ai reset attempt record")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("read Z.ai reset attempt record")
	}
	if attempt.State == "pending" {
		// A prior dispatch remains unresolved even when this call fails before a new POST.
		result.Dispatched = true
		if !resumePending {
			return result, ErrResetPending
		}
	}
	if attempt.State == "completed" && attempt.CreditID == creditID && attempt.Window == window {
		result.CreditID, result.Accepted, result.AlreadyAccepted = attempt.CreditID, true, true
		return result, nil
	}
	inventory, err := e.Client.List(ctx, account.APIKey, e.Now())
	if err != nil {
		return result, err
	}
	if attempt.State == "pending" {
		if creditID != "" && creditID != attempt.CreditID || window != attempt.Window {
			return result, errors.New("previous Z.ai reset unresolved; retry its exact card and window first")
		}
		creditID = attempt.CreditID
	}
	credit, err := SelectResetCredit(inventory, window, creditID, e.Now())
	if err != nil {
		if attempt.State == "pending" {
			attempt.State = "unavailable"
			if saveErr := saveResetAttempt(e.FS, path, attempt); saveErr != nil {
				return result, saveErr
			}
			return result, errors.New("previous Z.ai reset card no longer available; no additional card sent; check usage before requesting another reset")
		}
		return result, err
	}
	result.CreditID = credit.ID
	if attempt.State == "completed" && attempt.CreditID == credit.ID {
		result.Accepted, result.AlreadyAccepted = true, true
		return result, nil
	}
	accepted, err := confirm(credit)
	if err != nil {
		return result, err
	}
	if !accepted {
		return result, errors.New("Z.ai reset cancelled; no card consumed")
	}
	// A card may expire or be consumed elsewhere during manual confirmation.
	inventory, err = e.Client.List(ctx, account.APIKey, e.Now())
	if err != nil {
		return result, err
	}
	credit, err = SelectResetCredit(inventory, credit.Window, credit.ID, e.Now())
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, errors.New("Z.ai reset cancelled before dispatch")
	}
	if attempt.State != "pending" {
		attempt = resetAttempt{AccountID: account.ID, CreditID: credit.ID, Window: credit.Window, RequestID: e.RequestID(), State: "pending", StartedAt: e.Now().UTC()}
	}
	if attempt.RequestID == "" {
		return result, errors.New("Z.ai reset request identity unavailable")
	}
	if err := saveResetAttempt(e.FS, path, attempt); err != nil {
		return result, err
	}
	result.Dispatched = true
	if err := e.Client.Use(ctx, account.APIKey, credit, attempt.RequestID, e.Now()); err != nil {
		var resetErr *ResetError
		if errors.As(err, &resetErr) && !resetErr.Ambiguous {
			attempt.State = "rejected"
			if saveErr := saveResetAttempt(e.FS, path, attempt); saveErr != nil {
				return result, errors.Join(err, saveErr)
			}
		}
		return result, err
	}
	attempt.State = "completed"
	if err := saveResetAttempt(e.FS, path, attempt); err != nil {
		return result, errors.New("Z.ai accepted the reset but its local receipt could not be saved; check usage before continuing")
	}
	result.Accepted = true
	return result, nil
}

func saveResetAttempt(fsys fsutil.FileSystem, path string, attempt resetAttempt) error {
	data, err := json.Marshal(attempt)
	if err != nil {
		return errors.New("encode Z.ai reset attempt")
	}
	if err := fsutil.SecureAtomicWrite(fsys, path, data); err != nil {
		return errors.New("save Z.ai reset attempt")
	}
	return nil
}
