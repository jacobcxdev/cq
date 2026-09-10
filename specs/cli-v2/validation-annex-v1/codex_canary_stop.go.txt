package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

const (
	codexCanaryStopRequestVersion = 1
	codexCanaryStopOperation      = "stop_codex_canary"
	codexCanaryStopRequestTTL     = 5 * time.Minute
	codexCanaryStopDirectory      = "codex-canary-stop"
	codexCanaryStopRequestName    = "request.json"
	codexCanaryStopInflightName   = "inflight.json"
)

var (
	ErrCodexCanaryStopAlreadyRequested = errors.New("Codex canary stop is already requested")
	ErrCodexCanaryStopUnavailable      = errors.New("Codex canary stop request is unavailable")
)

type codexCanaryStopRequest struct {
	Version              int       `json:"version"`
	Operation            string    `json:"operation"`
	RunID                string    `json:"run_id"`
	Nonce                string    `json:"nonce"`
	RequestedAt          time.Time `json:"requested_at"`
	ExpiresAt            time.Time `json:"expires_at"`
	ObservedGeneration   uint64    `json:"observed_generation"`
	ReadinessFingerprint string    `json:"readiness_fingerprint"`
	MAC                  string    `json:"mac"`
}

// codexCanaryClaimedStop is process-local evidence only. It contains no path,
// account, session, turn, endpoint, or credential value.
type codexCanaryClaimedStop struct {
	request codexCanaryStopRequest
	digest  [sha256.Size]byte
}

type codexCanaryFinalEnvelope struct {
	generation           uint64
	envelopeDigest       [sha256.Size]byte
	countersDigest       [sha256.Size]byte
	stopRequestDigest    [sha256.Size]byte
	processBindingDigest [sha256.Size]byte
	endedAt              time.Time
}

// CodexCanaryStopRuntime carries only exact serving-generation authority.
type CodexCanaryStopRuntime struct {
	ListenerAddress string
	ServingAttestor *ServingAttestor
}

// CodexCanaryStopFunc waits for one signed stop intent, closes native
// admission, drains every admitted request, then performs the sole final write.
type CodexCanaryStopFunc func(context.Context, CodexCanaryStopRuntime) error

type codexCanaryNativeHTTPQuiescer interface {
	CloseAndDrain(context.Context) error
}

// NewCodexCanaryStopFunc binds stop authority to the active recorder, durable
// admission runtime, and exact native handler owned by this serving process.
func NewCodexCanaryStopFunc(recorder *CodexCanaryRecorder, runtime *CodexLeaseRuntime, native CodexNativeHTTPRoutingHandler) (CodexCanaryStopFunc, error) {
	quiescer, ok := native.(codexCanaryNativeHTTPQuiescer)
	if recorder == nil || runtime == nil || !ok || quiescer == nil {
		return nil, ErrCodexCanaryStopUnavailable
	}
	return func(ctx context.Context, serving CodexCanaryStopRuntime) error {
		if ctx == nil || serving.ListenerAddress == "" || serving.ServingAttestor == nil {
			return ErrCodexCanaryStopUnavailable
		}
		claimed, err := waitAndClaimCodexCanaryStop(ctx, recorder)
		if err != nil {
			return err
		}
		bindingInput := append([]byte("cq-codex-canary-stop-process-v1\x00"), claimed.digest[:]...)
		processBindingDigest := sha256.Sum256(bindingInput)
		servingLease, err := acquireCodexInstalledServingProof(
			ctx,
			serving.ListenerAddress,
			serving.ServingAttestor,
			processBindingDigest,
			(&net.Dialer{}).DialContext,
		)
		if err != nil {
			return ErrCodexCanaryStopUnavailable
		}
		defer servingLease.Release()
		if err := quiescer.CloseAndDrain(ctx); err != nil {
			return ErrCodexCanaryStopUnavailable
		}
		if runtime.nativeHTTPAdmissionPromotionBlocked() {
			return ErrCodexCanaryStopUnavailable
		}
		_, err = recorder.finaliseCodexCanaryStop(time.Now(), claimed, processBindingDigest, 0)
		return err
	}, nil
}

func waitAndClaimCodexCanaryStop(ctx context.Context, recorder *CodexCanaryRecorder) (codexCanaryClaimedStop, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending, err := codexCanaryStopRequestPending(recorder)
		if err != nil {
			return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
		}
		if pending {
			claimed, err := claimCodexCanaryStopRequest(recorder, time.Now())
			if err != nil {
				return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
			}
			return claimed, nil
		}
		select {
		case <-ctx.Done():
			return codexCanaryClaimedStop{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func codexCanaryStopRequestPending(recorder *CodexCanaryRecorder) (bool, error) {
	if recorder == nil || recorder.fs == nil {
		return false, ErrCodexCanaryStopUnavailable
	}
	inspector, inspectorOK := recorder.fs.(fsutil.SecurePathInspector)
	opener, openerOK := recorder.fs.(fsutil.SecureDirectoryOpener)
	if !inspectorOK || !openerOK {
		return false, ErrCodexCanaryStopUnavailable
	}
	directory, err := opener.OpenSecureDirectory(codexCanaryStopDirectoryPath(recorder.path))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer directory.Close()
	for _, name := range []string{codexCanaryStopInflightName, codexCanaryStopRequestName} {
		_, err := fsutil.ReadSecureFileInDirectory(inspector, directory, name, codexCanaryStateMaxBytes)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
	}
	return false, nil
}

// RequestCodexCanaryStop publishes intent only. The installed serving process
// remains the sole authority that may drain sessions and finalise the canary.
func RequestCodexCanaryStop(fsys fsutil.DurableFileSystem, path string, protected []CodexCanaryProtection, now time.Time) error {
	if fsys == nil || path == "" || now.IsZero() {
		return ErrCodexCanaryStopUnavailable
	}
	recorder, err := OpenCodexCanary(fsys, path, protected)
	if err != nil {
		return ErrCodexCanaryStopUnavailable
	}
	state := recorder.State()
	if !state.Active || !validCodexCanaryRandomID(state.RunID) || !completeCodexCanaryTuple(state.Tuple) {
		return ErrCodexCanaryStopUnavailable
	}
	nonce, err := newCodexCanaryRandomID()
	if err != nil {
		return ErrCodexCanaryStopUnavailable
	}
	now = now.UTC()
	request := codexCanaryStopRequest{
		Version:              codexCanaryStopRequestVersion,
		Operation:            codexCanaryStopOperation,
		RunID:                state.RunID,
		Nonce:                nonce,
		RequestedAt:          now,
		ExpiresAt:            now.Add(codexCanaryStopRequestTTL),
		ObservedGeneration:   recorder.generation,
		ReadinessFingerprint: state.Tuple.ReadinessFingerprint,
	}
	request.MAC, err = codexCanaryStopRequestMAC(recorder.key, request)
	if err != nil {
		return ErrCodexCanaryStopUnavailable
	}
	data, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return ErrCodexCanaryStopUnavailable
	}

	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	if !inspectorOK || !openerOK {
		return ErrCodexCanaryStopUnavailable
	}
	directoryPath := codexCanaryStopDirectoryPath(path)
	if err := fsutil.EnsureSecureDirectory(fsys, directoryPath); err != nil {
		return ErrCodexCanaryStopUnavailable
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return ErrCodexCanaryStopUnavailable
	}
	defer directory.Close()
	err = fsutil.SecureAtomicCreateInDirectoryChecked(inspector, directory, codexCanaryStopRequestName, data, func() error {
		current, currentErr := OpenCodexCanary(fsys, path, protected)
		if currentErr != nil {
			return ErrCodexCanaryStopUnavailable
		}
		currentState := current.State()
		if !currentState.Active || currentState.RunID != request.RunID || currentState.Tuple.ReadinessFingerprint != request.ReadinessFingerprint || current.generation < request.ObservedGeneration {
			return ErrCodexCanaryStopUnavailable
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrCodexCanaryStopAlreadyRequested
		}
		return ErrCodexCanaryStopUnavailable
	}
	return nil
}

func codexCanaryStopDirectoryPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), codexCanaryStopDirectory)
}

// claimCodexCanaryStopRequest is called only by the installed serving owner.
// A claimed inflight request resumes after restart even when its publication
// TTL has elapsed; an unclaimed request must still be fresh.
func claimCodexCanaryStopRequest(recorder *CodexCanaryRecorder, now time.Time) (codexCanaryClaimedStop, error) {
	if recorder == nil || recorder.fs == nil || now.IsZero() {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if err := recorder.requireOwnerLocked(); err != nil {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	if !recorder.state.Active {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	inspector, inspectorOK := recorder.fs.(fsutil.SecurePathInspector)
	opener, openerOK := recorder.fs.(fsutil.SecureDirectoryOpener)
	if !inspectorOK || !openerOK {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	directory, err := opener.OpenSecureDirectory(codexCanaryStopDirectoryPath(recorder.path))
	if err != nil {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	defer directory.Close()

	claimed, err := readCodexCanaryStopRequest(inspector, directory, codexCanaryStopInflightName, recorder, now.UTC(), true)
	if err == nil {
		return claimed, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	claimed, err = readCodexCanaryStopRequest(inspector, directory, codexCanaryStopRequestName, recorder, now.UTC(), false)
	if err != nil {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	if err := directory.RenameNoReplace(codexCanaryStopRequestName, codexCanaryStopInflightName); err != nil {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	if err := directory.Sync(); err != nil {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	confirmed, err := readCodexCanaryStopRequest(inspector, directory, codexCanaryStopInflightName, recorder, now.UTC(), true)
	if err != nil || confirmed.digest != claimed.digest {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	return confirmed, nil
}

// finaliseCodexCanaryStop is the single terminal state writer. Its caller must
// hold the installed serving-process authority, close native admission, and
// drain the outer HTTP relay before passing the sealed process binding.
func (recorder *CodexCanaryRecorder) finaliseCodexCanaryStop(now time.Time, claimed codexCanaryClaimedStop, processBindingDigest [sha256.Size]byte, activeSessions uint64) (codexCanaryFinalEnvelope, error) {
	if recorder == nil || now.IsZero() || claimed.digest == ([sha256.Size]byte{}) || processBindingDigest == ([sha256.Size]byte{}) || activeSessions != 0 {
		return codexCanaryFinalEnvelope{}, ErrCodexCanaryStopUnavailable
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if err := recorder.requireOwnerLocked(); err != nil {
		return codexCanaryFinalEnvelope{}, ErrCodexCanaryStopUnavailable
	}
	if !recorder.state.Active || claimed.request.RunID != recorder.state.RunID || claimed.request.ReadinessFingerprint != recorder.state.Tuple.ReadinessFingerprint || recorder.generation < claimed.request.ObservedGeneration {
		return codexCanaryFinalEnvelope{}, ErrCodexCanaryStopUnavailable
	}
	now = now.UTC()
	if now.Before(claimed.request.RequestedAt) || now.Before(recorder.state.StartedAt.UTC()) {
		return codexCanaryFinalEnvelope{}, ErrCodexCanaryStopUnavailable
	}
	confirmed, err := confirmCodexCanaryClaimLocked(recorder, now)
	if err != nil || confirmed.digest != claimed.digest || confirmed.request.Nonce != claimed.request.Nonce {
		return codexCanaryFinalEnvelope{}, ErrCodexCanaryStopUnavailable
	}

	previous := recorder.state
	previous.ProtectedDigests = append([]CodexCanaryProtectedDigest(nil), recorder.state.ProtectedDigests...)
	if recorder.state.Finalisation != nil {
		finalisation := *recorder.state.Finalisation
		previous.Finalisation = &finalisation
	}
	recorder.checkProtectedLocked()
	recorder.state.Active = false
	recorder.state.EndedAt = now
	countersDigest := codexCanaryCountersDigest(recorder.state)
	recorder.state.Finalisation = &codexCanaryFinalisation{
		StopRequestDigest:    hex.EncodeToString(claimed.digest[:]),
		ProcessBindingDigest: hex.EncodeToString(processBindingDigest[:]),
		CountersDigest:       hex.EncodeToString(countersDigest[:]),
		ActiveSessions:       activeSessions,
	}
	persisted, err := recorder.persistEnvelopeLocked()
	if err != nil {
		recorder.state = previous
		return codexCanaryFinalEnvelope{}, err
	}
	return codexCanaryFinalEnvelope{
		generation:           persisted.generation,
		envelopeDigest:       sha256.Sum256(persisted.data),
		countersDigest:       countersDigest,
		stopRequestDigest:    claimed.digest,
		processBindingDigest: processBindingDigest,
		endedAt:              now,
	}, nil
}

func confirmCodexCanaryClaimLocked(recorder *CodexCanaryRecorder, now time.Time) (codexCanaryClaimedStop, error) {
	inspector, inspectorOK := recorder.fs.(fsutil.SecurePathInspector)
	opener, openerOK := recorder.fs.(fsutil.SecureDirectoryOpener)
	if !inspectorOK || !openerOK {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	directory, err := opener.OpenSecureDirectory(codexCanaryStopDirectoryPath(recorder.path))
	if err != nil {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	defer directory.Close()
	return readCodexCanaryStopRequest(inspector, directory, codexCanaryStopInflightName, recorder, now, true)
}

func readCodexCanaryStopRequest(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, name string, recorder *CodexCanaryRecorder, now time.Time, inflight bool) (codexCanaryClaimedStop, error) {
	data, err := fsutil.ReadSecureFileInDirectory(inspector, directory, name, codexCanaryStateMaxBytes)
	if err != nil {
		return codexCanaryClaimedStop{}, err
	}
	var request codexCanaryStopRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	canonical, err := json.MarshalIndent(request, "", "  ")
	if err != nil || !bytes.Equal(data, canonical) {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	if !validCodexCanaryStopRequest(request, recorder, now, inflight) || !validCodexCanaryStopRequestMAC(recorder.key, request) {
		return codexCanaryClaimedStop{}, ErrCodexCanaryStopUnavailable
	}
	digestInput := append([]byte("cq-codex-canary-stop-request-v1\x00"), data...)
	return codexCanaryClaimedStop{request: request, digest: sha256.Sum256(digestInput)}, nil
}

func validCodexCanaryStopRequest(request codexCanaryStopRequest, recorder *CodexCanaryRecorder, now time.Time, inflight bool) bool {
	if request.Version != codexCanaryStopRequestVersion || request.Operation != codexCanaryStopOperation ||
		!validCodexCanaryRandomID(request.RunID) || !validCodexCanaryRandomID(request.Nonce) ||
		request.RequestedAt.IsZero() || request.ExpiresAt.IsZero() || request.RequestedAt.Location() != time.UTC || request.ExpiresAt.Location() != time.UTC ||
		request.ExpiresAt.Sub(request.RequestedAt) != codexCanaryStopRequestTTL || request.ObservedGeneration == 0 ||
		request.RunID != recorder.state.RunID || request.ReadinessFingerprint != recorder.state.Tuple.ReadinessFingerprint || recorder.generation < request.ObservedGeneration {
		return false
	}
	return inflight || now.Before(request.ExpiresAt)
}

func codexCanaryStopRequestMAC(key []byte, request codexCanaryStopRequest) (string, error) {
	request.MAC = ""
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq-codex-canary-stop-request-v1\x00"))
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func validCodexCanaryStopRequestMAC(key []byte, request codexCanaryStopRequest) bool {
	want, err := codexCanaryStopRequestMAC(key, request)
	if err != nil {
		return false
	}
	wantBytes, wantErr := base64.RawURLEncoding.DecodeString(want)
	gotBytes, gotErr := base64.RawURLEncoding.DecodeString(request.MAC)
	return wantErr == nil && gotErr == nil && len(gotBytes) == sha256.Size &&
		base64.RawURLEncoding.EncodeToString(gotBytes) == request.MAC && hmac.Equal(gotBytes, wantBytes)
}

func validCodexCanaryFinalisation(state CodexCanaryState) bool {
	if state.Active {
		return state.EndedAt.IsZero() && state.Finalisation == nil
	}
	if state.EndedAt.IsZero() || state.Finalisation == nil || state.Finalisation.ActiveSessions != 0 {
		return false
	}
	stopRequestDigest, stopErr := hex.DecodeString(state.Finalisation.StopRequestDigest)
	processBindingDigest, processErr := hex.DecodeString(state.Finalisation.ProcessBindingDigest)
	countersDigest, countersErr := hex.DecodeString(state.Finalisation.CountersDigest)
	wantCountersDigest := codexCanaryCountersDigest(state)
	return stopErr == nil && processErr == nil && countersErr == nil &&
		len(stopRequestDigest) == sha256.Size && len(processBindingDigest) == sha256.Size && len(countersDigest) == sha256.Size &&
		hex.EncodeToString(stopRequestDigest) == state.Finalisation.StopRequestDigest &&
		hex.EncodeToString(processBindingDigest) == state.Finalisation.ProcessBindingDigest &&
		hex.EncodeToString(countersDigest) == state.Finalisation.CountersDigest &&
		hmac.Equal(countersDigest, wantCountersDigest[:])
}
