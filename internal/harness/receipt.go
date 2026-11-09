package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

// Receipt retains the baseline needed to restore a chat's harness state.
type Receipt struct {
	Version         int                 `json:"version"`
	ChatID          string              `json:"chat_id"`
	Mode            string              `json:"mode"`
	Profile         Profile             `json:"profile"`
	Source          string              `json:"source"`
	State           string              `json:"state"`
	BaselinePlugins []string            `json:"baseline_plugins"`
	AppliedPlugins  []string            `json:"applied_plugins"`
	PendingPlugins  []string            `json:"pending_plugins,omitempty"`
	Fingerprint     string              `json:"fingerprint,omitempty"`
	Model           string              `json:"model,omitempty"`
	Cwd             string              `json:"cwd,omitempty"`
	Unsupported     []string            `json:"unsupported,omitempty"`
	Issues          []string            `json:"issues,omitempty"`
	Failure         *ApplicationFailure `json:"failure,omitempty"`
	RetrySupported  bool                `json:"retry_supported,omitempty"`
	UpdatedAt       time.Time           `json:"updated_at"`
}

// ApplicationFailure contains classifications only, never native messages or
// request data. Rollback reports recovery separately from the original failure.
type ApplicationFailure struct {
	Stage              string `json:"stage"`
	Reason             string `json:"reason"`
	NativeCode         *int   `json:"native_code,omitempty"`
	Rollback           string `json:"rollback"`
	RollbackStage      string `json:"rollback_stage,omitempty"`
	RollbackReason     string `json:"rollback_reason,omitempty"`
	RollbackNativeCode *int   `json:"rollback_native_code,omitempty"`
}

// Receipts stores private per-chat receipts and ownership leases.
type Receipts struct {
	FS  fsutil.FileSystem
	Dir string
}

// Load reads one receipt without taking ownership of its chat. Missing state
// returns an error matching os.ErrNotExist and does not create any files.
func (receipts Receipts) Load(chatID string) (Receipt, error) {
	path, err := receipts.path(chatID, ".json")
	if err != nil {
		return Receipt{}, err
	}
	data, err := fsutil.ReadSecureFile(receipts.FS, path, maxPolicyBytes)
	if err != nil {
		return Receipt{}, fmt.Errorf("read harness receipt: %w", err)
	}
	var receipt Receipt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return Receipt{}, fmt.Errorf("decode harness receipt: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Receipt{}, fmt.Errorf("harness receipt contains trailing data")
	}
	if err := validateReceipt(receipt); err != nil {
		return Receipt{}, err
	}
	if receipt.ChatID != chatID {
		return Receipt{}, fmt.Errorf("harness receipt chat does not match its filename")
	}
	return receipt, nil
}

// Save atomically records a receipt with the current UTC timestamp. Callers must
// hold the chat's lease across runtime changes and their corresponding Save.
func (receipts Receipts) Save(receipt Receipt) error {
	path, err := receipts.path(receipt.ChatID, ".json")
	if err != nil {
		return err
	}
	receipt.UpdatedAt = time.Now().UTC()
	if err := validateReceipt(receipt); err != nil {
		return err
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return fmt.Errorf("encode harness receipt: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxPolicyBytes {
		return fsutil.ErrSecureFileTooLarge
	}
	if err := fsutil.SecureAtomicWrite(receipts.FS, path, data); err != nil {
		return fmt.Errorf("write harness receipt: %w", err)
	}
	return nil
}

// Acquire takes a non-blocking per-chat lease shared by launcher and controller
// processes. Ownership lasts until Close; contention matches ErrExclusiveLockHeld.
func (receipts Receipts) Acquire(chatID string) (fsutil.ExclusiveLock, error) {
	path, err := receipts.path(chatID, ".lock")
	if err != nil {
		return nil, err
	}
	if err := fsutil.EnsureSecureDirectory(receipts.FS, receipts.Dir); err != nil {
		return nil, fmt.Errorf("secure harness receipt directory: %w", err)
	}
	lease, err := fsutil.AcquireExclusiveLock(receipts.FS, path)
	if err != nil {
		return nil, fmt.Errorf("acquire harness chat lease: %w", err)
	}
	return lease, nil
}

func (receipts Receipts) path(chatID, suffix string) (string, error) {
	if !filepath.IsAbs(receipts.Dir) || filepath.Clean(receipts.Dir) != receipts.Dir || strings.ContainsRune(receipts.Dir, '\x00') {
		return "", fmt.Errorf("harness receipt directory must be absolute and clean")
	}
	if !canonicalChatID(chatID) {
		return "", fmt.Errorf("harness receipt chat ID must be a canonical lower-case UUID")
	}
	return filepath.Join(receipts.Dir, chatID+suffix), nil
}

func validateReceipt(receipt Receipt) error {
	if receipt.RetrySupported && receipt.Mode != "full" {
		return fmt.Errorf("harness retry requires full-mode ownership")
	}
	if receipt.Version != 1 {
		return fmt.Errorf("unsupported harness receipt version %d", receipt.Version)
	}
	if !canonicalChatID(receipt.ChatID) {
		return fmt.Errorf("harness receipt chat ID must be a canonical lower-case UUID")
	}
	if receipt.Mode != "plugins" && receipt.Mode != "full" {
		return fmt.Errorf("invalid harness receipt mode %q", receipt.Mode)
	}
	if receipt.Profile != Minimal && receipt.Profile != Full && receipt.Profile != Custom {
		return fmt.Errorf("invalid harness receipt profile %q", receipt.Profile)
	}
	if strings.TrimSpace(receipt.Source) == "" {
		return fmt.Errorf("harness receipt source is required")
	}
	switch receipt.State {
	case "prepared", "applied", "partial", "pending", "failed", "diverged", "restored":
	default:
		return fmt.Errorf("invalid harness receipt state %q", receipt.State)
	}
	if receipt.UpdatedAt.IsZero() {
		return fmt.Errorf("harness receipt timestamp is required")
	}
	if failure := receipt.Failure; failure != nil {
		stage := func(value string) bool {
			switch value {
			case "settings", "release_subscriptions", "resume", "restore_subscriptions":
				return true
			}
			return false
		}
		reason := func(value string) bool {
			switch value {
			case "rpc_rejected", "rpc_timeout", "transport", "cancelled", "invalid_response", "subscription_missing", "internal":
				return true
			}
			return false
		}
		if receipt.State != "failed" || !stage(failure.Stage) || !reason(failure.Reason) {
			return fmt.Errorf("invalid harness failure classification")
		}
		switch failure.Rollback {
		case "not_needed", "restored":
			if failure.RollbackStage != "" || failure.RollbackReason != "" || failure.RollbackNativeCode != nil {
				return fmt.Errorf("unexpected harness rollback failure")
			}
		case "failed":
			if !stage(failure.RollbackStage) || !reason(failure.RollbackReason) {
				return fmt.Errorf("invalid harness rollback classification")
			}
		default:
			return fmt.Errorf("invalid harness rollback outcome")
		}
	}
	if len(receipt.Model) > 1024 || strings.ContainsRune(receipt.Model, '\x00') || (receipt.Cwd != "" && (!filepath.IsAbs(receipt.Cwd) || filepath.Clean(receipt.Cwd) != receipt.Cwd || strings.ContainsRune(receipt.Cwd, '\x00'))) {
		return fmt.Errorf("invalid harness receipt context")
	}
	for _, items := range [][]string{receipt.Unsupported, receipt.Issues} {
		for _, item := range items {
			if item == "" || len(item) > 4096 || strings.ContainsRune(item, '\x00') {
				return fmt.Errorf("invalid harness receipt application detail")
			}
		}
	}
	for _, plugins := range [][]string{receipt.BaselinePlugins, receipt.AppliedPlugins, receipt.PendingPlugins} {
		seen := make(map[string]bool, len(plugins))
		for _, plugin := range plugins {
			if plugin == "" || strings.ContainsRune(plugin, '\x00') || seen[plugin] {
				return fmt.Errorf("harness receipt plugins must be nonempty, unique identifiers")
			}
			seen[plugin] = true
		}
	}
	return nil
}
