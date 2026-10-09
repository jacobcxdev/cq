package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

var ErrRuntimeUpgradeReceipt = errors.New("invalid runtime upgrade receipt")
var runtimeUpgradeTransactionPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type RuntimeUpgradeReceiptV1 struct {
	SchemaVersion       int                       `json:"schema_version"`
	TransactionID       string                    `json:"transaction_id"`
	Generation          uint64                    `json:"generation"`
	Phase               string                    `json:"phase"`
	Previous            installer.RuntimeArtifact `json:"previous"`
	Candidate           installer.RuntimeArtifact `json:"candidate"`
	ListenerIdentity    string                    `json:"listener_identity"`
	SupervisorPID       int                       `json:"supervisor_pid"`
	AdmissionPauseNanos int64                     `json:"admission_pause_nanos"`
	ErrorCode           string                    `json:"error_code,omitempty"`
}

type RuntimeUpgradeStore struct {
	FS    fsutil.FileSystem
	Roots userdirs.Roots
}

func (store RuntimeUpgradeStore) Path() string {
	return filepath.Join(store.Roots.State, "runtime-upgrade.json")
}

func (receipt RuntimeUpgradeReceiptV1) terminal() bool {
	switch receipt.Phase {
	case "committed", "deferred", "rolled_back", "failed":
		return true
	}
	return false
}
func (receipt RuntimeUpgradeReceiptV1) Validate() error {
	if receipt.SchemaVersion != 1 || !runtimeUpgradeTransactionPattern.MatchString(receipt.TransactionID) || receipt.Generation == 0 || receipt.SupervisorPID <= 1 || receipt.ListenerIdentity == "" || receipt.AdmissionPauseNanos < 0 {
		return ErrRuntimeUpgradeReceipt
	}
	if err := receipt.Previous.Validate(); err != nil {
		return err
	}
	if err := receipt.Candidate.Validate(); err != nil {
		return err
	}
	switch receipt.Phase {
	case "prepared", "waiting", "handoff", "verifying", "committed", "deferred", "rolling_back", "rolled_back", "failed":
		return nil
	}
	return ErrRuntimeUpgradeReceipt
}
func (store RuntimeUpgradeStore) Load() (RuntimeUpgradeReceiptV1, error) {
	if store.FS == nil || !filepath.IsAbs(store.Roots.State) {
		return RuntimeUpgradeReceiptV1{}, ErrRuntimeUpgradeReceipt
	}
	data, err := fsutil.ReadSecureFile(store.FS, store.Path(), 64<<10)
	if err != nil {
		return RuntimeUpgradeReceiptV1{}, err
	}
	var receipt RuntimeUpgradeReceiptV1
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, ErrRuntimeUpgradeReceipt
	}
	if decoder.Decode(new(any)) != io.EOF {
		return receipt, ErrRuntimeUpgradeReceipt
	}
	return receipt, receipt.Validate()
}

// Save runs under the service mutation lock or the supervisor's upgrade mutex.
func (store RuntimeUpgradeStore) Save(receipt RuntimeUpgradeReceiptV1) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	previous, err := store.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if previous.TransactionID != receipt.TransactionID {
			if !previous.terminal() || receipt.Generation != previous.Generation+1 || receipt.Phase != "prepared" {
				return fmt.Errorf("%w: competing transaction", ErrRuntimeUpgradeReceipt)
			}
		} else {
			if previous.Generation != receipt.Generation || previous.Previous != receipt.Previous || previous.Candidate != receipt.Candidate || previous.ListenerIdentity != receipt.ListenerIdentity || previous.SupervisorPID != receipt.SupervisorPID {
				return ErrRuntimeUpgradeReceipt
			}
			if previous.Phase != receipt.Phase && !validRuntimeUpgradeTransition(previous.Phase, receipt.Phase) {
				return fmt.Errorf("%w: %s -> %s", ErrRuntimeUpgradeReceipt, previous.Phase, receipt.Phase)
			}
		}
	} else if receipt.Phase != "prepared" || receipt.Generation != 1 {
		return ErrRuntimeUpgradeReceipt
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return fsutil.SecureAtomicWrite(store.FS, store.Path(), append(data, '\n'))
}
func validRuntimeUpgradeTransition(from, to string) bool {
	switch from {
	case "prepared":
		return to == "waiting" || to == "deferred" || to == "failed"
	case "waiting":
		return to == "handoff" || to == "deferred" || to == "failed"
	case "handoff":
		return to == "verifying" || to == "rolling_back"
	case "verifying":
		return to == "committed" || to == "rolling_back"
	case "rolling_back":
		return to == "rolled_back" || to == "failed"
	}
	return false
}
