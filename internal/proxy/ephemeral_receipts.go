package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	admissionActive   bool
	dispatchActive    bool
}

type proxyEphemeralStateSnapshot struct {
	AdmissionReceipts uint64 `json:"admission_receipts"`
	DispatchReceipts  uint64 `json:"dispatch_receipts"`
	PrunedReceipts    uint64 `json:"pruned_receipts"`
	PruneErrors       uint64 `json:"prune_errors"`
	AdmissionFailed   bool   `json:"admission_prune_failed"`
	DispatchFailed    bool   `json:"dispatch_prune_failed"`
	AdmissionActive   bool   `json:"admission_prune_active"`
	DispatchActive    bool   `json:"dispatch_prune_active"`
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
		state.admissionActive = false
	case ephemeralReceiptDispatch:
		state.dispatchReceipts = count
		state.dispatchFailed = err != nil
		state.dispatchActive = false
	}
	state.prunedReceipts += pruned
	if err != nil {
		state.pruneErrors++
	}
}

func (state *proxyEphemeralStateObservability) recordPruneStart(kind ephemeralReceiptKind) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if kind == ephemeralReceiptAdmission {
		state.admissionActive = true
	} else if kind == ephemeralReceiptDispatch {
		state.dispatchActive = true
	}
}

func (state *proxyEphemeralStateObservability) recordPruneStopped(kind ephemeralReceiptKind, pruned uint64) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if kind == ephemeralReceiptAdmission {
		state.admissionActive = false
	} else if kind == ephemeralReceiptDispatch {
		state.dispatchActive = false
	}
	state.prunedReceipts += pruned
}

func (state *proxyEphemeralStateObservability) recordPruneFailure(kind ephemeralReceiptKind, err error) {
	if state == nil || err == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if kind == ephemeralReceiptAdmission {
		state.admissionActive = false
		state.admissionFailed = true
	} else if kind == ephemeralReceiptDispatch {
		state.dispatchActive = false
		state.dispatchFailed = true
	}
	state.pruneErrors++
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
		AdmissionActive:   state.admissionActive,
		DispatchActive:    state.dispatchActive,
	}
}

type ephemeralReceiptPruner struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	err    error
}

func startEphemeralReceiptPruner(fsys fsutil.FileSystem, path, prefix string, kind ephemeralReceiptKind, now func() time.Time) *ephemeralReceiptPruner {
	opener, openOK := fsys.(fsutil.SecureDirectoryOpener)
	inspector, inspectOK := fsys.(fsutil.SecurePathInspector)
	if !openOK || !inspectOK {
		proxyProcessEphemeralState.recordPruneFailure(kind, fsutil.ErrSecureCapabilityUnavailable)
		return nil
	}
	directory, err := opener.OpenSecureDirectory(path)
	if err != nil {
		proxyProcessEphemeralState.recordPruneFailure(kind, err)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	pruner := &ephemeralReceiptPruner{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(pruner.done)
		defer func() {
			closeErr := directory.Close()
			if recovered := recover(); recovered != nil {
				pruner.err = errors.Join(closeErr, fmt.Errorf("ephemeral receipt prune panic: %v", recovered))
				proxyProcessEphemeralState.recordPruneFailure(kind, pruner.err)
			} else if closeErr != nil && ctx.Err() == nil {
				pruner.err = closeErr
				proxyProcessEphemeralState.recordPruneFailure(kind, closeErr)
			} else {
				pruner.err = closeErr
			}
		}()
		ticker := time.NewTicker(ephemeralReceiptPruneInterval)
		defer ticker.Stop()
		for {
			proxyProcessEphemeralState.recordPruneStart(kind)
			remaining, pruned, scanErr := pruneEphemeralReceipts(ctx, inspector, directory, prefix, now())
			if ctx.Err() != nil {
				proxyProcessEphemeralState.recordPruneStopped(kind, pruned)
				return
			}
			proxyProcessEphemeralState.recordScan(kind, remaining, pruned, scanErr)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return pruner
}

func (pruner *ephemeralReceiptPruner) stop() error {
	if pruner == nil {
		return nil
	}
	pruner.once.Do(pruner.cancel)
	<-pruner.done
	return pruner.err
}

func pruneEphemeralReceipts(ctx context.Context, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, prefix string, now time.Time) (remaining, pruned uint64, returnErr error) {
	remover, removeOK := directory.(fsutil.IdentityBoundRemover)
	if ctx == nil || inspector == nil || !removeOK {
		return 0, 0, fsutil.ErrSecureCapabilityUnavailable
	}
	visit := func(entry os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
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
