package zai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func resetAttemptPath(dir, accountID string) string {
	digest := sha256.Sum256([]byte(accountID))
	return filepath.Join(dir, hex.EncodeToString(digest[:])+".json")
}

// ResetAccountLockPath is shared by exact-card use, scheduled use and ZCode's
// window-selected reset contract. Every card-use journal is read under it.
func ResetAccountLockPath(dir, accountID string) string {
	return resetAttemptPath(dir, accountID) + ".lock"
}

// CheckExactResetUsePending guards a ZCode window reset against an unresolved
// exact-card request. Callers must hold ResetAccountLockPath while checking and
// through any following dispatch.
func CheckExactResetUsePending(fsys fsutil.FileSystem, dir, accountID string) error {
	attempt, err := readResetAttempt(fsys, resetAttemptPath(dir, accountID), accountID)
	if err != nil {
		return err
	}
	if attempt.State == "pending" {
		return ErrResetPending
	}
	return nil
}

func readResetAttempt(fsys fsutil.FileSystem, path, accountID string) (resetAttempt, error) {
	data, err := fsys.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return resetAttempt{}, nil
	}
	if err != nil {
		return resetAttempt{}, errors.New("read Z.ai reset attempt record")
	}
	var attempt resetAttempt
	if len(data) > 16<<10 || json.Unmarshal(data, &attempt) != nil || attempt.AccountID != accountID || attempt.CreditID == "" || attempt.RequestID == "" || attempt.StartedAt.IsZero() || attempt.Window != "5h" && attempt.Window != "7d" || attempt.State != "pending" && attempt.State != "completed" && attempt.State != "rejected" && attempt.State != "unavailable" {
		return resetAttempt{}, errors.New("invalid Z.ai reset attempt record")
	}
	return attempt, nil
}

func resetUseInteropPath(dir, accountID string) string {
	return strings.TrimSuffix(resetAttemptPath(dir, accountID), ".json") + "-use.json"
}

func checkZCodeResetUsePending(fsys fsutil.FileSystem, dir, accountID string) error {
	data, err := fsys.ReadFile(resetUseInteropPath(dir, accountID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("read ZCode reset-use attempt")
	}
	var attempt struct {
		SchemaVersion int       `json:"schema_version"`
		AccountID     string    `json:"account_id"`
		Operation     string    `json:"operation"`
		Window        string    `json:"window"`
		RequestID     string    `json:"request_id"`
		State         string    `json:"state"`
		StartedAt     time.Time `json:"started_at"`
	}
	if len(data) > 16<<10 || json.Unmarshal(data, &attempt) != nil || attempt.SchemaVersion != 1 || attempt.AccountID != accountID || attempt.Operation != "use" || attempt.Window != "5h" && attempt.Window != "7d" || !resetUseInteropUUID(attempt.RequestID) || attempt.StartedAt.IsZero() || attempt.State != "pending" && attempt.State != "completed" && attempt.State != "rejected" {
		return errors.New("invalid ZCode reset-use attempt")
	}
	if attempt.State == "pending" {
		return ErrResetPending
	}
	return nil
}

func resetUseInteropUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil && len(decoded) == 16
}
