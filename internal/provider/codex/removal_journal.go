package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type RemovalCandidate struct {
	CandidateID CandidateID `json:"candidate_id"`
	Revision    Revision    `json:"revision"`
}

type RemovalPlan struct {
	Version                int                `json:"version"`
	OperationID            string             `json:"operation_id"`
	AccountKey             AccountKey         `json:"account_key"`
	Candidates             []RemovalCandidate `json:"candidates"`
	ExpectedSystemRevision Revision           `json:"expected_system_revision,omitempty"`
	RegistryKeys           []string           `json:"registry_keys,omitempty"`
	Force                  bool               `json:"force"`
}

type RemovalResult struct {
	ManagedDeleted    int
	SystemDeactivated bool
	ProjectionError   error
	PendingRecovery   bool
}

type RemovalJournal struct {
	FS    fsutil.DurableFileSystem
	Home  string
	Store *ManagedStore
}

func (j RemovalJournal) path() string {
	return filepath.Join(j.Home, ".config", "cq", "state", "codex_removal.json")
}

func (j RemovalJournal) Save(plan RemovalPlan) error {
	if plan.Version != 1 || plan.OperationID == "" || plan.AccountKey == "" {
		return errors.New("invalid removal plan")
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if j.Store == nil {
		return errors.New("durable removal store unavailable")
	}
	return j.Store.durableReplace(j.path(), data)
}

func (j RemovalJournal) Load() (RemovalPlan, bool, error) {
	data, err := j.FS.ReadFile(j.path())
	if errors.Is(err, os.ErrNotExist) {
		return RemovalPlan{}, false, nil
	}
	if err != nil {
		return RemovalPlan{}, false, err
	}
	var plan RemovalPlan
	if err := json.Unmarshal(data, &plan); err != nil || plan.Version != 1 || plan.OperationID == "" || plan.AccountKey == "" {
		return RemovalPlan{}, false, fmt.Errorf("invalid removal journal")
	}
	return plan, true, nil
}

func (j RemovalJournal) Clear() error {
	err := j.FS.Remove(j.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return j.FS.SyncDir(filepath.Dir(j.path()))
}
