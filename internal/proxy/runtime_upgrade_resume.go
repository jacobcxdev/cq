package proxy

import (
	"crypto/sha256"
	"errors"
	"net"
	"os"
)

// RuntimeUpgradeResumeV1 is independent of worker role manifests. It carries
// ownership evidence only; credentials and admitted requests never cross exec.
type RuntimeUpgradeResumeV1 struct {
	SchemaVersion            int                     `json:"schema_version"`
	Receipt                  RuntimeUpgradeReceiptV1 `json:"receipt"`
	Release                  RuntimeWorkerReleaseV1  `json:"release"`
	SupervisorHolder         LifecycleHolderProof    `json:"supervisor_holder"`
	LifecycleIdentity        [sha256.Size]byte       `json:"lifecycle_identity"`
	JobTarget                string                  `json:"job_target"`
	PauseStartedUnixNano     int64                   `json:"pause_started_unix_nano"`
	Recovery                 bool                    `json:"recovery"`
	WorkerSequence           uint64                  `json:"worker_sequence"`
	PreviousCheckpointDigest string                  `json:"previous_checkpoint_digest"`
}

func (resume RuntimeUpgradeResumeV1) ValidateFiles(listener, lifecycle *os.File) error {
	if resume.SchemaVersion != 1 || resume.Receipt.Validate() != nil || !resume.Release.valid() || resume.LifecycleIdentity == ([sha256.Size]byte{}) {
		return ErrRuntimeUpgradeReceipt
	}
	if resume.Receipt.Phase != "handoff" && resume.Receipt.Phase != "verifying" && resume.Receipt.Phase != "rolling_back" {
		return ErrRuntimeUpgradeReceipt
	}
	if err := ValidateRuntimeUpgradeListener(listener, resume.Receipt.ListenerIdentity); err != nil {
		return errors.Join(ErrRuntimeUpgradeReceipt, err)
	}
	digest, err := RuntimeDescriptorIdentityDigest(lifecycle)
	if err != nil || digest != resume.LifecycleIdentity {
		return ErrRuntimeUpgradeReceipt
	}
	holder, err := RuntimeLifecycleHolder(lifecycle, resume.SupervisorHolder.DescriptionID)
	if err != nil || holder != resume.SupervisorHolder {
		return ErrRuntimeUpgradeReceipt
	}
	return nil
}

func ValidateRuntimeUpgradeListener(file *os.File, identity string) error {
	if file == nil {
		return ErrRuntimeUpgradeReceipt
	}
	listener, err := net.FileListener(file)
	if err != nil {
		return ErrRuntimeUpgradeReceipt
	}
	defer listener.Close()
	tcp, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() || listener.Addr().Network()+"|"+listener.Addr().String() != identity {
		return ErrRuntimeUpgradeReceipt
	}
	return nil
}
