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
	if ctx == nil || fsys == nil || ctx.Err() != nil || state.Phase != proxy.CandidatePhaseRemoved || !cleanAbsolutePath(root) {
		return proxy.ErrCandidateLifecycleInvalid
	}
	parentPath, base := filepath.Dir(root), filepath.Base(root)
	parent, err := fsutil.OpenOwnerControlledDirectory(fsys, parentPath)
	if err != nil {
		return err
	}
	tombstone := "." + base + ".removed-" + state.OperationID
	if err := parent.RenameNoReplace(base, tombstone); err != nil {
		_ = parent.Close()
		return err
	}
	if err := parent.Sync(); err != nil {
		_ = parent.Close()
		return err
	}
	if err := parent.Close(); err != nil {
		return err
	}
	return removeCandidateTree(ctx, fsys, filepath.Join(parentPath, tombstone))
}

func removeCandidateTree(ctx context.Context, fsys fsutil.FileSystem, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	info, err := inspector.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fsutil.ErrUnsafeSecurePath
	}
	if info.IsDir() {
		entries, err := fsys.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := removeCandidateTree(ctx, fsys, filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
	} else if !info.Mode().IsRegular() {
		return fsutil.ErrUnsafeSecurePath
	}
	return fsys.Remove(path)
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
