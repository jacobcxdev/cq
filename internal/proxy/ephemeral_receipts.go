package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	ephemeralReceiptPruneInterval = time.Minute
	ephemeralReceiptExpiryGrace   = time.Minute
	ephemeralReceiptCorruptMaxAge = 24 * time.Hour
	ephemeralReceiptMaxBytes      = 64 << 10
	ephemeralReceiptScanBatch     = 256
)

type ephemeralReceiptKind uint8

const (
	ephemeralReceiptAdmission ephemeralReceiptKind = iota + 1
	ephemeralReceiptDispatch
)

type ephemeralReceiptDocument struct {
	AdmissionID string    `json:"admission_id"`
	PermitID    string    `json:"permit_id"`
	ValidUntil  time.Time `json:"valid_until"`
}

type proxyEphemeralStateObservability struct {
	mu sync.Mutex

	admissionReceipts uint64
	dispatchReceipts  uint64
	prunedReceipts    uint64
	pruneErrors       uint64
	admissionFailed   bool
	dispatchFailed    bool
}

type proxyEphemeralStateSnapshot struct {
	AdmissionReceipts uint64 `json:"admission_receipts"`
	DispatchReceipts  uint64 `json:"dispatch_receipts"`
	PrunedReceipts    uint64 `json:"pruned_receipts"`
	PruneErrors       uint64 `json:"prune_errors"`
	AdmissionFailed   bool   `json:"admission_prune_failed"`
	DispatchFailed    bool   `json:"dispatch_prune_failed"`
}

var proxyProcessEphemeralState proxyEphemeralStateObservability

func (state *proxyEphemeralStateObservability) recordScan(kind ephemeralReceiptKind, count, pruned uint64, err error) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	switch kind {
	case ephemeralReceiptAdmission:
		state.admissionReceipts = count
		state.admissionFailed = err != nil
	case ephemeralReceiptDispatch:
		state.dispatchReceipts = count
		state.dispatchFailed = err != nil
	}
	state.prunedReceipts += pruned
	if err != nil {
		state.pruneErrors++
	}
}

func (state *proxyEphemeralStateObservability) recordCreate(kind ephemeralReceiptKind) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if kind == ephemeralReceiptAdmission {
		state.admissionReceipts++
	} else if kind == ephemeralReceiptDispatch {
		state.dispatchReceipts++
	}
}

func (state *proxyEphemeralStateObservability) snapshot() proxyEphemeralStateSnapshot {
	if state == nil {
		return proxyEphemeralStateSnapshot{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return proxyEphemeralStateSnapshot{
		AdmissionReceipts: state.admissionReceipts,
		DispatchReceipts:  state.dispatchReceipts,
		PrunedReceipts:    state.prunedReceipts,
		PruneErrors:       state.pruneErrors,
		AdmissionFailed:   state.admissionFailed,
		DispatchFailed:    state.dispatchFailed,
	}
}

func pruneEphemeralReceipts(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, prefix string, now time.Time) (remaining, pruned uint64, returnErr error) {
	remover, removeOK := directory.(fsutil.IdentityBoundRemover)
	if inspector == nil || !removeOK {
		return 0, 0, fsutil.ErrSecureCapabilityUnavailable
	}
	visit := func(entry os.DirEntry) error {
		name := entry.Name()
		id, recognised := ephemeralReceiptID(name, prefix)
		if !recognised {
			return nil
		}
		remaining++
		file, openErr := directory.OpenNoFollow(name)
		if openErr != nil {
			returnErr = errors.Join(returnErr, openErr)
			return nil
		}
		info, statErr := file.Stat()
		remove := false
		if statErr == nil && info.Mode().IsRegular() {
			body, readErr := io.ReadAll(io.LimitReader(file, ephemeralReceiptMaxBytes+1))
			var document ephemeralReceiptDocument
			decodeErr := json.Unmarshal(body, &document)
			matchingID := (prefix == "consumed-" && document.AdmissionID == id) ||
				(prefix == "dispatch-permit-" && document.PermitID == id)
			remove = readErr == nil && len(body) <= ephemeralReceiptMaxBytes && decodeErr == nil && matchingID && !document.ValidUntil.IsZero() && !now.Before(document.ValidUntil.Add(ephemeralReceiptExpiryGrace))
			if !remove && !now.Before(info.ModTime().Add(ephemeralReceiptCorruptMaxAge)) {
				remove = true
			}
		}
		closeErr := file.Close()
		if statErr != nil || closeErr != nil {
			returnErr = errors.Join(returnErr, statErr, closeErr)
			return nil
		}
		if !remove {
			return nil
		}
		identity, identityOK := inspector.FileIdentity(info)
		if !identityOK {
			returnErr = errors.Join(returnErr, fsutil.ErrSecureCapabilityUnavailable)
			return nil
		}
		if removeErr := remover.RemoveChecked(name, identity); removeErr != nil {
			returnErr = errors.Join(returnErr, removeErr)
			return nil
		}
		remaining--
		pruned++
		return nil
	}
	if visitor, ok := directory.(fsutil.SecureDirectoryVisitor); ok {
		returnErr = errors.Join(returnErr, visitor.VisitEntries(ephemeralReceiptScanBatch, visit))
	} else if reader, ok := directory.(fsutil.SecureDirectoryReader); ok {
		entries, err := reader.ReadDir()
		if err != nil {
			return 0, 0, err
		}
		for _, entry := range entries {
			if err := visit(entry); err != nil {
				returnErr = errors.Join(returnErr, err)
			}
		}
	} else {
		return 0, 0, fsutil.ErrSecureCapabilityUnavailable
	}
	if pruned != 0 {
		returnErr = errors.Join(returnErr, directory.Sync())
	}
	return remaining, pruned, returnErr
}

func ephemeralReceiptID(name, prefix string) (string, bool) {
	if len(name) != len(prefix)+32+len(".json") || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
	for _, character := range id {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "", false
		}
	}
	return id, true
}
