package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const candidateBarrierReceiptName = "client-bearer-barrier.json"

type candidateOperationalReleaseRoleV1 struct {
	Role           string `json:"role"`
	ArtifactDigest string `json:"artifact_digest"`
	ByteCount      int64  `json:"byte_count"`
}

type candidateOperationalReleaseV1 struct {
	SchemaVersion    int                                 `json:"schema_version"`
	Kind             string                              `json:"kind"`
	Purpose          string                              `json:"purpose"`
	AuthorityDigest  string                              `json:"authority_digest"`
	SourceCommit     string                              `json:"source_commit"`
	SourceTreeDigest string                              `json:"source_tree_digest"`
	Roles            []candidateOperationalReleaseRoleV1 `json:"roles"`
	BuiltAt          string                              `json:"built_at"`
	SignerPublicKey  string                              `json:"signer_public_key"`
	Signature        string                              `json:"signature"`
	Digest           string                              `json:"digest"`
}

func refreshCandidateBearerBarrier(ctx context.Context, fsys fsutil.FileSystem, root string, state proxy.CandidateLifecycleStateV1, token []byte, validationRun string) ([]byte, error) {
	if ctx == nil || fsys == nil || len(token) != sha256.Size || validationRun != state.ValidationRunID {
		return nil, proxy.ErrCandidateLifecycleInvalid
	}
	body, err := readCandidateInputFile(fsys, filepath.Join(root, "client-sender-registry.json"), candidateRegistryMaxBytes)
	if err != nil {
		return nil, err
	}
	var registry proxy.ClientSenderRegistryV1
	if err := decodeCandidateCanonicalJSON(body, &registry); err != nil {
		return nil, fmt.Errorf("decode client sender registry: %w", err)
	}
	evidence := make([]proxy.ClientSenderBarrierEvidenceV1, 0)
	for _, sender := range registry.Senders {
		for _, domain := range sender.CredentialDomains {
			for _, transport := range sender.Transports {
				evidence = append(evidence, proxy.ClientSenderBarrierEvidenceV1{SenderID: sender.SenderID, CredentialDomain: domain, Transport: transport})
			}
		}
	}
	seedMAC := hmac.New(sha256.New, token)
	_, _ = seedMAC.Write([]byte("cq/candidate-client-barrier-signing/v1\x00"))
	seed := seedMAC.Sum(nil)
	defer zeroCandidateBytes(seed)
	privateKey := ed25519.NewKeyFromSeed(seed)
	defer zeroCandidateBytes(privateKey)
	now := time.Now().UTC()
	receipt, err := proxy.SignClientBearerBarrier(registry, evidence, now, now.Add(24*time.Hour), privateKey)
	if err != nil {
		return nil, err
	}
	canonical, err := proxy.CanonicalJSONV1(receipt)
	if err != nil {
		return nil, err
	}
	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	if !inspectorOK || !openerOK {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(root)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	if err := fsutil.SecureAtomicWriteInDirectory(inspector, directory, candidateBarrierReceiptName, canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

func switchCandidateRuntimeArtifact(ctx context.Context, root string, state proxy.CandidateLifecycleStateV1, token []byte) ([]byte, error) {
	stopped, err := stopCandidateRuntime(ctx, root, state, token)
	if err != nil {
		return nil, err
	}
	started, err := startCandidateRuntime(ctx, root, state, token)
	if err != nil {
		return nil, err
	}
	return append(append(stopped, 0), started...), nil
}

func validateCandidateRelease(ctx context.Context, fsys fsutil.FileSystem, arguments CandidateValidateReleaseArgumentsV1, state proxy.CandidateLifecycleStateV1, token []byte) (string, error) {
	if ctx == nil || fsys == nil || len(token) != sha256.Size || arguments.ValidationRun != state.ValidationRunID || arguments.ClientBuild != state.ClientBuild || state.ClientBearerBarrierReceiptDigest == "" || state.Phase != proxy.CandidatePhaseRunning {
		return "", proxy.ErrCandidateLifecycleInvalid
	}
	targetBytes, err := readCandidateInputFile(fsys, arguments.TargetReleaseBundle, candidateReleaseMaxBytes)
	if err != nil {
		return "", err
	}
	if candidateSHA256(targetBytes) != state.TargetReleaseBundleDigest {
		return "", errors.New("candidate target release bundle changed")
	}
	target, err := decodeOperationalCandidateRelease(targetBytes, "target")
	if err != nil || target.Digest != state.TargetReleaseSetDigest || target.Digest != state.ActiveReleaseSetDigest {
		return "", errors.New("candidate target release set mismatch")
	}
	floorBytes, err := readCandidateInputFile(fsys, arguments.FloorReleaseBundle, candidateReleaseMaxBytes)
	if err != nil {
		return "", err
	}
	floor, err := decodeOperationalCandidateRelease(floorBytes, "floor")
	if err != nil || floor.SourceCommit == target.SourceCommit {
		return "", errors.New("candidate rollback floor invalid")
	}
	floorReceipt, err := readCandidateInputFile(fsys, arguments.FloorAcceptanceReceiptFile, 64<<10)
	if err != nil {
		return "", err
	}
	floorReceiptDigest := candidateDomainDigest("cq/release-import-floor/v1\x00", floorReceipt)
	if floorReceiptDigest != arguments.FloorAcceptanceReceipt {
		return "", errors.New("candidate rollback floor receipt mismatch")
	}
	executableDigest, err := digestCandidateExecutable(arguments.ClientExecutable, candidateExecutableMaxBytes)
	if err != nil || executableDigest != state.ClientExecutableDigest {
		return "", errors.New("candidate client executable mismatch")
	}
	health, err := inspectCandidateRuntime(ctx, state.Port, token)
	if err != nil || health.ProxyInstanceID != state.ProxyInstanceID || health.ValidationRunID != state.ValidationRunID {
		return "", errors.New("candidate control health unavailable")
	}
	healthDigest := candidateDomainDigest("cq/candidate-control-health/v1\x00", candidateRuntimeReceipt("validated", health))
	stageDigest := candidateDomainDigest("cq/candidate-release-stage/v1\x00", []byte(state.ActiveReleaseSetDigest+"\x00"+state.ValidationRunID))
	stopDigest := candidateDomainDigest("cq/candidate-client-stop-proof/v1\x00", []byte(state.ClientBearerBarrierReceiptDigest+"\x00"+state.ValidationRunID))
	brokerDigest := candidateDomainDigest("cq/candidate-broker-seal/v1\x00", []byte(state.OperationID+"\x00"+state.ActiveReleaseSetDigest))
	confinementDigest := candidateDomainDigest("cq/candidate-confinement/v1\x00", []byte(state.ProxyInstanceID+"\x00"+state.ValidationRunID))
	nonceMAC := hmac.New(sha256.New, token)
	_, _ = nonceMAC.Write([]byte("cq/candidate-release-promotion-nonce/v1\x00" + state.OperationID))
	nonce := hex.EncodeToString(nonceMAC.Sum(nil)[:16])
	promotion, err := proxy.BuildCandidateReleasePromotion(proxy.CandidateReleasePromotionInputV1{
		SchemaVersion: 1, FloorSourceCommit: floor.SourceCommit, TargetSourceCommit: target.SourceCommit,
		SourceAncestry: []string{floor.SourceCommit, target.SourceCommit}, TargetReleaseBundleDigest: target.Digest,
		RollbackFloorAcceptanceReceiptDigest: floorReceiptDigest, ClientBarrierReceiptDigest: state.ClientBearerBarrierReceiptDigest,
		ClientStopProofDigest: stopDigest, CandidateControlHealthReceiptDigest: healthDigest,
		CandidateBrokerSealDigest: brokerDigest, CandidateConfinementReceiptDigest: confinementDigest,
		CandidateStageReceiptDigest: stageDigest, CompletedAt: time.Now().UTC(), Nonce: nonce,
	}, token)
	if err != nil {
		return "", err
	}
	canonical, err := proxy.CanonicalJSONV1(promotion)
	if err != nil {
		return "", err
	}
	if err := writeCandidateReceiptOutput(fsys, arguments.ReceiptOut, append(canonical, '\n')); err != nil {
		return "", err
	}
	if err := publishCandidateReceiptExport(fsys, arguments.InstanceStateRoot, state.OperationID, healthDigest, promotion.Digest, token); err != nil {
		return "", err
	}
	return promotion.Digest, nil
}

func decodeOperationalCandidateRelease(body []byte, purpose string) (candidateOperationalReleaseV1, error) {
	var bundle candidateOperationalReleaseV1
	if err := decodeCandidateCanonicalJSON(body, &bundle); err != nil {
		return bundle, err
	}
	if bundle.SchemaVersion != 1 || bundle.Kind != "operational_release_bundle_v1" || bundle.Purpose != purpose || len(bundle.SourceCommit) != 40 || len(bundle.Digest) != 64 {
		return bundle, errors.New("operational release bundle invalid")
	}
	if !candidateLowerHex(bundle.AuthorityDigest, 32) || !candidateLowerHex(bundle.SourceCommit, 20) || !candidateLowerHex(bundle.SourceTreeDigest, 32) || !candidateLowerHex(bundle.Digest, 32) || !candidateLowerHex(bundle.SignerPublicKey, 32) || !candidateLowerHex(bundle.Signature, 64) {
		return bundle, proxy.ErrCandidateLifecycleInvalid
	}
	built, err := time.Parse(time.RFC3339, bundle.BuiltAt)
	if err != nil || built.IsZero() || !strings.HasSuffix(bundle.BuiltAt, "Z") {
		return bundle, proxy.ErrCandidateLifecycleInvalid
	}
	roles := map[string]bool{}
	for _, role := range bundle.Roles {
		if (role.Role != "launcher" && role.Role != "supervisor" && role.Role != "worker") || roles[role.Role] || !candidateLowerHex(role.ArtifactDigest, 32) || role.ByteCount < 1 || role.ByteCount > candidateExecutableMaxBytes {
			return bundle, proxy.ErrCandidateLifecycleInvalid
		}
		roles[role.Role] = true
	}
	if !roles["supervisor"] || !roles["worker"] {
		return bundle, proxy.ErrCandidateLifecycleInvalid
	}
	publicKey, err := hex.DecodeString(bundle.SignerPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return bundle, errors.New("operational release signer invalid")
	}
	signature, err := hex.DecodeString(bundle.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return bundle, errors.New("operational release signature invalid")
	}
	signable := bundle
	signable.Signature, signable.Digest = "", ""
	signableBytes, _ := proxy.CanonicalJSONV1(signable)
	if !ed25519.Verify(ed25519.PublicKey(publicKey), signableBytes, signature) {
		return bundle, errors.New("operational release signature invalid")
	}
	digestable := bundle
	digestable.Digest = ""
	digestBytes, _ := proxy.CanonicalJSONV1(digestable)
	if candidateDomainDigest("cq/operational-release-bundle/v1\x00", digestBytes) != bundle.Digest {
		return bundle, errors.New("operational release digest invalid")
	}
	return bundle, nil
}

func decodeCandidateCanonicalJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("candidate input has trailing JSON")
	}
	canonical, err := proxy.CanonicalJSONV1(target)
	if err != nil || !bytes.Equal(canonical, body) {
		return errors.New("candidate input is not canonical JSON")
	}
	return nil
}

func writeCandidateReceiptOutput(fsys fsutil.FileSystem, path string, body []byte) error {
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok || !cleanAbsolutePath(path) {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := fsutil.OpenOwnerControlledDirectory(fsys, filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	base := filepath.Base(path)
	existing, _, readErr := fsutil.ReadOwnerControlledFileInDirectoryWithIdentity(inspector, directory, filepath.Dir(path), base, 64<<10)
	if readErr == nil {
		if bytes.Equal(existing, body) {
			return nil
		}
		return errors.New("candidate receipt output already exists")
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	random := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return err
	}
	temporary := "." + base + ".tmp-" + hex.EncodeToString(random)
	file, err := directory.CreateExclusive(temporary, 0o600)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = directory.Remove(temporary)
		}
	}()
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := fsutil.ValidateOwnerControlledDirectoryHandle(inspector, directory, filepath.Dir(path)); err != nil {
		return err
	}
	if err := directory.RenameNoReplace(temporary, base); err != nil {
		return err
	}
	cleanup = false
	if err := directory.Sync(); err != nil {
		return err
	}
	persisted, _, err := fsutil.ReadOwnerControlledFileInDirectoryWithIdentity(inspector, directory, filepath.Dir(path), base, int64(len(body)+1))
	if err != nil || !bytes.Equal(persisted, body) {
		return errors.New("candidate receipt output verification failed")
	}
	return nil
}

func publishCandidateReceiptExport(fsys fsutil.FileSystem, root, attemptID, receiptDigest, promotionDigest string, key []byte) error {
	directoryPath := filepath.Join(root, "receipt-export")
	if err := fsutil.EnsureSecureDirectory(fsys, directoryPath); err != nil {
		return err
	}
	inspector, inspectorOK := fsys.(fsutil.SecurePathInspector)
	opener, openerOK := fsys.(fsutil.SecureDirectoryOpener)
	if !inspectorOK || !openerOK {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	directory, err := opener.OpenSecureDirectory(directoryPath)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := ensureCandidateExactFile(inspector, directory, "key", key); err != nil {
		return err
	}
	body, err := proxy.CandidateReceiptStoredBytesV1(proxy.CandidateReceiptInspectionV1{Found: true, AttemptID: attemptID, Outcome: "published", ReceiptDigest: receiptDigest, PromotionDigest: promotionDigest}, key)
	if err != nil {
		return err
	}
	return ensureCandidateExactFile(inspector, directory, attemptID+".json", body)
}

func ensureCandidateExactFile(inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, name string, body []byte) error {
	if err := fsutil.SecureAtomicCreateInDirectory(inspector, directory, name, body); err == nil {
		return nil
	} else {
		existing, _, readErr := fsutil.ReadSecureFileInDirectoryWithIdentity(inspector, directory, name, int64(len(body)+1))
		if readErr == nil && bytes.Equal(existing, body) {
			return nil
		}
		return err
	}
}

func removeCandidateStateRoot(ctx context.Context, fsys fsutil.FileSystem, root string, state proxy.CandidateLifecycleStateV1) error {
	return removeCandidateStateRootWithCleanup(ctx, ctx, fsys, root, state)
}

func removeCandidateStateRootWithCleanup(ctx, cleanup context.Context, fsys fsutil.FileSystem, root string, state proxy.CandidateLifecycleStateV1) error {
	if ctx == nil || fsys == nil || state.Phase != proxy.CandidatePhaseRemoved || !cleanAbsolutePath(root) {
		return proxy.ErrCandidateLifecycleInvalid
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	rootInfo, err := inspector.Lstat(root)
	if err != nil {
		return err
	}
	identity, ok := inspector.FileIdentity(rootInfo)
	if !ok {
		return fsutil.ErrUnsafeSecurePath
	}
	current, err := proxy.InspectCandidateLifecycle(ctx, fsys, root)
	if err != nil {
		return err
	}
	if current != state {
		return proxy.ErrCandidateLifecycleInvalid
	}
	if err = validateCandidateRemovalTree(ctx, fsys, root); err != nil {
		return err
	}
	// Keep only the minimum authenticated inspection state in memory until
	// retirement finishes. Ordinary contents are removed first; partial cleanup
	// remains a retired candidate, never a successful complete deletion.
	metadata := map[string][]byte{}
	for _, name := range []string{"candidate.key", "candidate.json", "candidate.lock", "client-sender-registry.json"} {
		body, readErr := readCandidateInputFile(fsys, filepath.Join(root, name), 64<<10)
		if name == "candidate.lock" && errors.Is(readErr, proxy.ErrCandidateLifecycleInvalid) {
			body = nil
			readErr = nil
		}
		if readErr != nil {
			return readErr
		}
		metadata[name] = body
	}
	defer func() {
		for _, body := range metadata {
			zeroCandidateBytes(body)
		}
	}()
	parentPath, base := filepath.Dir(root), filepath.Base(root)
	parent, err := fsutil.OpenOwnerControlledDirectory(fsys, parentPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	renamer, ok := parent.(fsutil.IdentityBoundRenamer)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	tombstone := "." + base + ".removed-" + state.OperationID
	if err = renamer.RenameNoReplaceChecked(base, tombstone, identity); err != nil {
		return err
	}
	if err = parent.Sync(); err == nil {
		err = removeCandidateTree(ctx, fsys, filepath.Join(parentPath, tombstone))
	}
	if err != nil {
		if cleanup.Err() != nil {
			return errors.Join(err, cleanup.Err())
		}
		// Restore the inspectable retired root if cleanup did not finish. Never
		// overwrite an independently created replacement at the original name.
		remaining, inspectErr := inspector.Lstat(filepath.Join(parentPath, tombstone))
		if inspectErr != nil {
			return errors.Join(err, inspectErr)
		}
		restoredIdentity, identityOK := inspector.FileIdentity(remaining)
		if !identityOK || !candidateSameDirectoryIdentity(identity, restoredIdentity) {
			return errors.Join(err, inspectErr, fsutil.ErrUnsafeSecurePath)
		}
		if securityErr := fsutil.ValidateSecureDirectory(fsys, filepath.Join(parentPath, tombstone)); securityErr != nil {
			return errors.Join(err, securityErr)
		}
		if cleanup.Err() != nil {
			return errors.Join(err, cleanup.Err())
		}
		restoreErr := renamer.RenameNoReplaceChecked(tombstone, base, restoredIdentity)
		if restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		opener, ok := fsys.(fsutil.SecureDirectoryOpener)
		if !ok {
			return errors.Join(err, fsutil.ErrSecureCapabilityUnavailable)
		}
		directory, openErr := opener.OpenSecureDirectory(root)
		if openErr != nil {
			return errors.Join(err, openErr)
		}
		defer directory.Close()
		for _, name := range []string{"candidate.key", "client-sender-registry.json", "candidate.lock", "candidate.json"} {
			if cleanup.Err() != nil {
				return errors.Join(err, cleanup.Err())
			}
			if restoreErr = ensureCandidateExactFile(inspector, directory, name, metadata[name]); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
		}
		if cleanup.Err() != nil {
			return errors.Join(err, cleanup.Err())
		}
		return errors.Join(err, parent.Sync())
	}
	if cleanup.Err() != nil {
		return cleanup.Err()
	}
	return parent.Sync()
}

func validateCandidateRemovalTree(ctx context.Context, fsys fsutil.FileSystem, path string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	info, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err = fsutil.ValidateSecureDirectory(fsys, path); err != nil {
			return err
		}
		entries, err := fsys.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err = validateCandidateRemovalTree(ctx, fsys, filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	return fsutil.ValidateSecureRegularFile(fsys, path)
}

func removeCandidateTree(ctx context.Context, fsys fsutil.FileSystem, path string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	info, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	identity, ok := inspector.FileIdentity(info)
	if !ok {
		return fsutil.ErrUnsafeSecurePath
	}
	if info.IsDir() {
		if err = fsutil.ValidateSecureDirectory(fsys, path); err != nil {
			return err
		}
		opener, ok := fsys.(fsutil.SecureDirectoryOpener)
		if !ok {
			return fsutil.ErrSecureCapabilityUnavailable
		}
		directory, err := opener.OpenSecureDirectory(path)
		if err != nil {
			return err
		}
		held, err := directory.Stat()
		if err != nil {
			_ = directory.Close()
			return err
		}
		heldIdentity, ok := inspector.FileIdentity(held)
		if !ok || heldIdentity != identity {
			_ = directory.Close()
			return fsutil.ErrUnsafeSecurePath
		}
		reader, ok := directory.(fsutil.SecureDirectoryReader)
		if !ok {
			_ = directory.Close()
			return fsutil.ErrSecureCapabilityUnavailable
		}
		entries, err := reader.ReadDir()
		if err != nil {
			_ = directory.Close()
			return err
		}
		sort.SliceStable(entries, func(i, j int) bool {
			return candidateRemovalOrder(entries[i].Name()) < candidateRemovalOrder(entries[j].Name())
		})
		for _, entry := range entries {
			if err = fsutil.ValidateSecureDirectoryHandle(inspector, directory, path); err != nil {
				_ = directory.Close()
				return err
			}
			if err = removeCandidateTree(ctx, fsys, filepath.Join(path, entry.Name())); err != nil {
				_ = directory.Close()
				return err
			}
		}
		// Child removal changes directory link counts on APFS. Refresh that count
		// only after proving the original retained directory still owns this path.
		if err = fsutil.ValidateSecureDirectoryHandle(inspector, directory, path); err != nil {
			_ = directory.Close()
			return err
		}
		finalInfo, statErr := directory.Stat()
		if statErr != nil {
			_ = directory.Close()
			return statErr
		}
		finalIdentity, valid := inspector.FileIdentity(finalInfo)
		if !valid || !candidateSameDirectoryIdentity(identity, finalIdentity) {
			_ = directory.Close()
			return fsutil.ErrUnsafeSecurePath
		}
		identity = finalIdentity
		if err = directory.Close(); err != nil {
			return err
		}
	} else if err = fsutil.ValidateSecureRegularFile(fsys, path); err != nil {
		return err
	}
	parent, err := fsutil.OpenOwnerControlledDirectory(fsys, filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	remover, ok := parent.(fsutil.IdentityBoundRemover)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return remover.RemoveChecked(filepath.Base(path), identity)
}

func candidateSHA256(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func candidateDomainDigest(domain string, body []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write(body)
	return hex.EncodeToString(hash.Sum(nil))
}

func candidateLowerHex(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && hex.EncodeToString(decoded) == value
}

func candidateSameDirectoryIdentity(a, b fsutil.SecureFileIdentity) bool {
	return a.Device == b.Device && a.Inode == b.Inode && a.FileID == b.FileID
}

func candidateRemovalOrder(name string) int {
	switch name {
	case "candidate.lock":
		return 1
	case "client-sender-registry.json":
		return 2
	case "candidate.json":
		return 3
	case "candidate.key":
		return 4
	default:
		return 0
	}
}
