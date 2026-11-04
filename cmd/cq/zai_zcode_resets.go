package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	zaiprov "github.com/jacobcxdev/cq/internal/provider/zai"
	"github.com/jacobcxdev/cq/internal/provider/zai/zcode"
)

type ZAIZCodeResetsCmd struct {
	Issue ZAIZCodeResetIssueCmd `cmd:"" help:"Explicitly request an eligible reset-card opportunity"`
	Use   ZAIZCodeResetUseCmd   `cmd:"" help:"Consume a reset card after confirmation"`
}

type ZAIZCodeResetIssueCmd struct {
	Reference string `arg:"" help:"OAuth account ID or unique label"`
	New       bool   `help:"Start another eligibility check after a recorded result"`
	Yes       bool   `help:"Confirm a new eligibility check without an interactive prompt"`
}

type ZAIZCodeResetUseCmd struct {
	Reference string `arg:"" help:"OAuth account ID or unique label"`
	Window    string `default:"5h" enum:"5h,7d" help:"Quota to reset; weekly also restores 5-hour quota"`
	Yes       bool   `help:"Consume the selected reset type without an interactive prompt"`
	New       bool   `help:"Start another reset after a recorded successful action"`
}

type zcodeResetDependencies struct {
	Accounts  func() ([]zaiprov.Account, error)
	Client    zcode.Client
	FS        fsutil.FileSystem
	StateDir  func() (string, error)
	In        io.Reader
	Out       io.Writer
	ErrOut    io.Writer
	Now       func() time.Time
	RequestID func() string
}

type zcodeResetAttempt struct {
	SchemaVersion int        `json:"schema_version"`
	AccountID     string     `json:"account_id"`
	Operation     string     `json:"operation"`
	Window        string     `json:"window,omitempty"`
	RequestID     string     `json:"request_id"`
	State         string     `json:"state"`
	StartedAt     time.Time  `json:"started_at"`
	Granted       *bool      `json:"granted,omitempty"`
	NextTryAt     *time.Time `json:"next_try_at,omitempty"`
}

type zcodeResetCommandResult struct {
	AccountID       string     `json:"account_id"`
	Operation       string     `json:"operation"`
	Window          string     `json:"window,omitempty"`
	RequestID       string     `json:"request_id"`
	Accepted        bool       `json:"accepted"`
	AlreadyAccepted bool       `json:"already_accepted,omitempty"`
	Granted         *bool      `json:"granted,omitempty"`
	NextTryAt       *time.Time `json:"next_try_at,omitempty"`
}

func runZAIZCodeResetsCommand(ctx context.Context, command string, options ZAIZCodeResetsCmd, jsonOutput bool, deps zcodeResetDependencies) error {
	fields := strings.Fields(command)
	if len(fields) == 0 || (fields[0] != "issue" && fields[0] != "use") {
		return errors.New("unknown ZCode reset command")
	}
	operation := fields[0]
	reference, newAction, yes, window := options.Issue.Reference, options.Issue.New, options.Issue.Yes, ""
	if operation == "use" {
		reference, newAction, yes, window = options.Use.Reference, options.Use.New, options.Use.Yes, options.Use.Window
		if window != "5h" && window != "7d" {
			return errors.New("ZCode reset window must be 5h or 7d")
		}
	}
	if deps.Accounts == nil || deps.FS == nil || deps.StateDir == nil || deps.Now == nil || deps.RequestID == nil || deps.Out == nil || deps.ErrOut == nil {
		return errors.New("ZCode reset command unavailable")
	}
	account, err := selectZCodeResetAccount(deps.Accounts, reference)
	if err != nil {
		return err
	}
	dir, err := deps.StateDir()
	if err != nil || dir == "" {
		return errors.New("resolve ZCode reset attempt directory")
	}
	if err := fsutil.EnsureSecureDirectory(deps.FS, dir); err != nil {
		return errors.New("prepare ZCode reset attempt directory")
	}
	lock, err := fsutil.AcquireExclusiveLock(deps.FS, zaiprov.ResetAccountLockPath(dir, account.ID))
	if err != nil {
		return errors.New("ZCode reset account is busy or attempt storage unavailable")
	}
	defer lock.Close()
	// Re-resolve inside the account lock; a label must not redirect an action
	// after it has been selected, and newly rotated credentials must be used.
	account, err = selectZCodeResetAccount(deps.Accounts, account.ID)
	if err != nil {
		return err
	}
	if operation == "use" {
		if err := zaiprov.CheckExactResetUsePending(deps.FS, dir, account.ID); err != nil {
			return err
		}
	}
	path := zcodeResetAttemptPath(dir, account.ID, operation)
	attempt, err := readZCodeResetAttempt(deps.FS, path, account.ID, operation)
	if err != nil {
		return err
	}
	wasPending := attempt.State == "pending"
	if wasPending && (newAction || attempt.Window != window) {
		return errors.New("previous ZCode reset outcome unresolved; retry the same action without --new")
	}
	if attempt.State == "completed" && !newAction {
		if attempt.Window != window {
			return errors.New("previous reset already completed; use --new to confirm another window")
		}
		return writeZCodeResetResult(deps.Out, jsonOutput, attempt, true)
	}
	if attempt.State == "rejected" && attempt.Window != window && !newAction {
		return errors.New("use --new to confirm a different reset window")
	}
	if operation == "issue" && newAction && attempt.NextTryAt != nil && deps.Now().Before(*attempt.NextTryAt) {
		return errors.New("ZCode eligibility check is still in its server-provided cooldown")
	}
	client := deps.Client
	if account.OAuth.DeviceMid != "" {
		client.DeviceMid = account.OAuth.DeviceMid
	}
	if operation == "use" {
		status, err := client.ResetStatus(ctx, account.OAuth.AsSession())
		if err != nil {
			return err
		}
		if !wasPending && !zcodeResetAvailable(status, window, deps.Now()) {
			return errors.New("no unexpired ZCode reset card for this window; no card consumed")
		}
	}
	if operation == "use" || newAction {
		confirmed, err := confirmZCodeReset(deps, account, operation, window, yes, wasPending)
		if err != nil {
			return err
		}
		if !confirmed {
			return errors.New("ZCode reset action cancelled; no request sent")
		}
	}
	if operation == "use" {
		account, err = selectZCodeResetAccount(deps.Accounts, account.ID)
		if err != nil {
			return err
		}
		if account.OAuth.DeviceMid != "" {
			client.DeviceMid = account.OAuth.DeviceMid
		}
		status, err := client.ResetStatus(ctx, account.OAuth.AsSession())
		if err != nil {
			return err
		}
		if !wasPending && !zcodeResetAvailable(status, window, deps.Now()) {
			return errors.New("ZCode reset card expired or changed during confirmation; no card consumed")
		}
	}
	if ctx.Err() != nil {
		return errors.New("ZCode reset cancelled before dispatch")
	}
	if attempt.State == "" || newAction {
		id := deps.RequestID()
		if !zcodeResetUUID(id) {
			return errors.New("ZCode reset UUID unavailable")
		}
		attempt = zcodeResetAttempt{SchemaVersion: 1, AccountID: account.ID, Operation: operation, Window: window, RequestID: id, State: "pending", StartedAt: deps.Now().UTC()}
	} else {
		attempt.State = "pending"
	}
	if err := saveZCodeResetAttempt(deps.FS, path, attempt); err != nil {
		return err
	}
	if operation == "use" {
		resetType := "FIVE_HOUR"
		if window == "7d" {
			resetType = "WEEK"
		}
		err = client.UseReset(ctx, account.OAuth.AsSession(), resetType, attempt.RequestID)
	} else {
		var result zcode.OpportunityResult
		result, err = client.Opportunity(ctx, account.OAuth.AsSession(), attempt.RequestID)
		if err == nil {
			attempt.Granted = &result.Granted
			attempt.NextTryAt = result.NextTryAt
		}
	}
	if err != nil {
		var operationError *zcode.OperationError
		// A later preflight/rejection cannot disprove an earlier uncertain POST.
		if !wasPending && errors.As(err, &operationError) && !operationError.Ambiguous {
			attempt.State = "rejected"
			if saveErr := saveZCodeResetAttempt(deps.FS, path, attempt); saveErr != nil {
				return errors.Join(err, saveErr)
			}
		}
		return err
	}
	attempt.State = "completed"
	if err := saveZCodeResetAttempt(deps.FS, path, attempt); err != nil {
		return errors.New("ZCode accepted the action but its receipt could not be saved; retry the same action without --new")
	}
	return writeZCodeResetResult(deps.Out, jsonOutput, attempt, false)
}

func selectZCodeResetAccount(read func() ([]zaiprov.Account, error), reference string) (zaiprov.Account, error) {
	accounts, err := read()
	if err != nil {
		return zaiprov.Account{}, errors.New("read ZCode reset accounts")
	}
	var selected []zaiprov.Account
	for _, account := range accounts {
		if reference != "" && account.ID == reference {
			selected = []zaiprov.Account{account}
			break
		}
		if reference != "" && account.Label == reference {
			selected = append(selected, account)
		}
	}
	if len(selected) != 1 {
		return zaiprov.Account{}, errors.New("ZCode OAuth account not found or label ambiguous; use account ID")
	}
	if selected[0].ID == "" || selected[0].OAuth == nil || selected[0].OAuth.UserID == "" {
		return zaiprov.Account{}, errors.New("ZCode reset requires a browser OAuth account; run cq zai login --zcode")
	}
	return selected[0], nil
}

func zcodeResetAttemptPath(dir, accountID, operation string) string {
	digest := sha256.Sum256([]byte(accountID))
	name := hex.EncodeToString(digest[:])
	if operation != "" {
		name += "-" + operation + ".json"
	} else {
		name += ".json"
	}
	return filepath.Join(dir, name)
}

func readZCodeResetAttempt(fsys fsutil.FileSystem, path, accountID, operation string) (zcodeResetAttempt, error) {
	data, err := fsys.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return zcodeResetAttempt{}, nil
	}
	if err != nil {
		return zcodeResetAttempt{}, errors.New("read ZCode reset attempt")
	}
	var record zcodeResetAttempt
	if len(data) > 16<<10 || json.Unmarshal(data, &record) != nil || record.SchemaVersion != 1 || record.AccountID != accountID || record.Operation != operation || !zcodeResetUUID(record.RequestID) || record.StartedAt.IsZero() || (record.State != "pending" && record.State != "completed" && record.State != "rejected") || (operation == "use" && record.Window != "5h" && record.Window != "7d") || (operation == "issue" && record.Window != "") || (operation == "issue" && record.State == "completed" && (record.Granted == nil || !*record.Granted && record.NextTryAt == nil)) {
		return zcodeResetAttempt{}, errors.New("invalid ZCode reset attempt")
	}
	return record, nil
}

func saveZCodeResetAttempt(fsys fsutil.FileSystem, path string, attempt zcodeResetAttempt) error {
	data, err := json.Marshal(attempt)
	if err != nil {
		return errors.New("encode ZCode reset attempt")
	}
	if err := fsutil.SecureAtomicWrite(fsys, path, data); err != nil {
		return errors.New("save ZCode reset intent or receipt")
	}
	return nil
}

func zcodeResetUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range []byte(value) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func zcodeResetAvailable(status zcode.ResetStatus, window string, now time.Time) bool {
	available := status.AvailableFiveHourResets
	if window == "7d" {
		available = status.AvailableWeekResets
	}
	for _, card := range available {
		expires, err := card.ExpireAt.Int64()
		if err == nil && expires > now.UnixMilli() {
			return true
		}
	}
	return false
}

func confirmZCodeReset(deps zcodeResetDependencies, account zaiprov.Account, operation, window string, yes, pending bool) (bool, error) {
	if yes {
		return true, nil
	}
	if deps.In == nil {
		return false, errors.New("interactive ZCode reset confirmation unavailable; use --yes for explicit confirmation")
	}
	message := fmt.Sprintf("Request another reset-card eligibility evaluation for %s? [y/N] ", account.ID)
	if operation == "use" {
		message = fmt.Sprintf("Consume one %s reset card for %s and restart its quota timer? [y/N] ", window, account.ID)
		if window == "7d" {
			message = fmt.Sprintf("Consume one weekly reset card for %s and restore weekly and 5-hour quotas? [y/N] ", account.ID)
		}
		if pending {
			message = fmt.Sprintf("Retry unresolved %s reset for %s with its original request ID? [y/N] ", window, account.ID)
			if window == "7d" {
				message = fmt.Sprintf("Retry unresolved weekly and 5-hour reset for %s with its original request ID? [y/N] ", account.ID)
			}
		}
	}
	if _, err := fmt.Fprint(deps.ErrOut, message); err != nil {
		return false, errors.New("write ZCode reset confirmation")
	}
	answer, err := bufio.NewReader(io.LimitReader(deps.In, 32)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, errors.New("read ZCode reset confirmation")
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func writeZCodeResetResult(out io.Writer, jsonOutput bool, attempt zcodeResetAttempt, already bool) error {
	result := zcodeResetCommandResult{AccountID: attempt.AccountID, Operation: attempt.Operation, Window: attempt.Window, RequestID: attempt.RequestID, Accepted: true, AlreadyAccepted: already, Granted: attempt.Granted, NextTryAt: attempt.NextTryAt}
	if jsonOutput {
		if err := json.NewEncoder(out).Encode(result); err != nil {
			return errors.New("write ZCode reset result")
		}
		return nil
	}
	var err error
	if already {
		_, err = fmt.Fprintf(out, "ZCode already accepted this %s action for %s; no additional request sent.\n", attempt.Operation, attempt.AccountID)
	} else if attempt.Operation == "use" {
		_, err = fmt.Fprintf(out, "ZCode accepted the %s reset for %s.\n", attempt.Window, attempt.AccountID)
	} else if attempt.Granted != nil && *attempt.Granted {
		_, err = fmt.Fprintf(out, "ZCode issued a reset opportunity for %s; no card consumed.\n", attempt.AccountID)
	} else {
		_, err = fmt.Fprintf(out, "ZCode has no reset opportunity for %s; next eligibility check: %s.\n", attempt.AccountID, attempt.NextTryAt.UTC().Format(time.RFC3339))
	}
	if err != nil {
		return errors.New("write ZCode reset result")
	}
	return nil
}
