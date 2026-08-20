//go:build !windows

package proxy

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"time"
)

type RollbackFloorRoleV1 struct {
	Role           string `json:"role"`
	ArtifactDigest string `json:"artifact_digest"`
	ManifestDigest string `json:"manifest_digest"`
}

type RollbackFloorBuildInputV1 struct {
	SchemaVersion                                 int                   `json:"schema_version"`
	AcceptanceRunID                               string                `json:"acceptance_run_id"`
	ReleaseBuildAuthorityDigest                   string                `json:"release_build_authority_digest"`
	SourceCommit                                  string                `json:"source_commit"`
	SourceTreeDigest                              string                `json:"source_tree_digest"`
	ToolchainIdentity                             string                `json:"toolchain_identity"`
	Roles                                         []RollbackFloorRoleV1 `json:"roles"`
	BuildReportDigest                             string                `json:"build_report_digest"`
	VetReportDigest                               string                `json:"vet_report_digest"`
	RaceTestReportDigest                          string                `json:"race_test_report_digest"`
	ConstructionUnitReportSetDigest               string                `json:"construction_unit_report_set_digest"`
	LegacyAtomicWriterReachabilityCatalogueDigest string                `json:"legacy_atomic_writer_reachability_catalogue_digest"`
	ConstructionUnits                             []string              `json:"construction_units"`
	ReleaseArtifactSetDigest                      string                `json:"release_artifact_set_digest"`
	RequiredLauncherABIDigest                     string                `json:"required_launcher_abi_digest"`
	RequiredFeatures                              []string              `json:"required_features"`
	InstalledCorpusDigest                         string                `json:"installed_corpus_digest"`
	RecoveryCorpusDigest                          string                `json:"recovery_corpus_digest"`
	RollbackCorpusDigest                          string                `json:"rollback_corpus_digest"`
	StartedAt                                     time.Time             `json:"started_at"`
	EndedAt                                       time.Time             `json:"ended_at"`
	Nonce                                         string                `json:"nonce"`
}

type RollbackFloorBundleV1 struct {
	SchemaVersion                                 int                   `json:"schema_version"`
	Kind                                          string                `json:"kind"`
	Purpose                                       string                `json:"purpose"`
	ReleaseBuildAuthorityDigest                   string                `json:"release_build_authority_digest"`
	SourceCommit                                  string                `json:"source_commit"`
	SourceTreeDigest                              string                `json:"source_tree_digest"`
	ToolchainIdentity                             string                `json:"toolchain_identity"`
	Roles                                         []RollbackFloorRoleV1 `json:"roles"`
	BuildReportDigest                             string                `json:"build_report_digest"`
	VetReportDigest                               string                `json:"vet_report_digest"`
	RaceTestReportDigest                          string                `json:"race_test_report_digest"`
	ConstructionUnitReportSetDigest               string                `json:"construction_unit_report_set_digest"`
	LegacyAtomicWriterReachabilityCatalogueDigest string                `json:"legacy_atomic_writer_reachability_catalogue_digest"`
	ConstructionUnits                             []string              `json:"construction_units"`
	ReleaseArtifactSetDigest                      string                `json:"release_artifact_set_digest"`
	RequiredLauncherABIDigest                     string                `json:"required_launcher_abi_digest"`
	RequiredFeatures                              []string              `json:"required_features"`
	InstalledCorpusDigest                         string                `json:"installed_corpus_digest"`
	RecoveryCorpusDigest                          string                `json:"recovery_corpus_digest"`
	RollbackCorpusDigest                          string                `json:"rollback_corpus_digest"`
	SignerPublicKey                               string                `json:"signer_public_key"`
	Signature                                     string                `json:"signature"`
}

type RollbackFloorAcceptanceReceiptV1 struct {
	SchemaVersion                                 int      `json:"schema_version"`
	Kind                                          string   `json:"kind"`
	AcceptanceRunID                               string   `json:"acceptance_run_id"`
	ReleaseBuildAuthorityDigest                   string   `json:"release_build_authority_digest"`
	FloorReleaseBundleDigest                      string   `json:"floor_release_bundle_digest"`
	FloorSourceCommit                             string   `json:"floor_source_commit"`
	FloorSourceTreeDigest                         string   `json:"floor_source_tree_digest"`
	FloorToolchainIdentity                        string   `json:"floor_toolchain_identity"`
	FloorReleaseArtifactSetDigest                 string   `json:"floor_release_artifact_set_digest"`
	FloorBuildReportDigest                        string   `json:"floor_build_report_digest"`
	FloorConstructionUnitReportSetDigest          string   `json:"floor_construction_unit_report_set_digest"`
	LegacyAtomicWriterReachabilityCatalogueDigest string   `json:"legacy_atomic_writer_reachability_catalogue_digest"`
	FloorSupervisorArtifactDigest                 string   `json:"floor_supervisor_artifact_digest"`
	FloorSupervisorArtifactManifestDigest         string   `json:"floor_supervisor_artifact_manifest_digest"`
	FloorWorkerArtifactDigest                     string   `json:"floor_worker_artifact_digest"`
	FloorWorkerArtifactManifestDigest             string   `json:"floor_worker_artifact_manifest_digest"`
	VetReportDigest                               string   `json:"vet_report_digest"`
	RaceTestReportDigest                          string   `json:"race_test_report_digest"`
	RequiredLauncherABIDigest                     string   `json:"required_launcher_abi_digest"`
	RequiredFeatures                              []string `json:"required_features"`
	InstalledCorpusDigest                         string   `json:"installed_corpus_digest"`
	RecoveryCorpusDigest                          string   `json:"recovery_corpus_digest"`
	RollbackCorpusDigest                          string   `json:"rollback_corpus_digest"`
	StartedAt                                     string   `json:"started_at"`
	EndedAt                                       string   `json:"ended_at"`
	Nonce                                         string   `json:"nonce"`
	IssuerPublicKey                               string   `json:"issuer_public_key"`
	Signature                                     string   `json:"signature"`
}

var exactRollbackFloorConstructionUnits = []string{"CU-0", "CU-1", "CU-2", "CU-3", "CU-4", "CU-5", "CU-6", "CU-7", "CU-8"}

func BuildRollbackFloorBundle(authority ReleaseBuildAuthorityV1, input RollbackFloorBuildInputV1, privateKey ed25519.PrivateKey) (RollbackFloorBundleV1, RollbackFloorAcceptanceReceiptV1, error) {
	authorityDigest, err := DigestReleaseBuildAuthority(authority)
	if err != nil {
		return RollbackFloorBundleV1{}, RollbackFloorAcceptanceReceiptV1{}, err
	}
	if len(privateKey) != ed25519.PrivateKeySize || !slices.Equal(privateKey.Public().(ed25519.PublicKey), mustDecodeReleasePublicKey(authority.Ed25519PublicKey)) {
		return RollbackFloorBundleV1{}, RollbackFloorAcceptanceReceiptV1{}, fmt.Errorf("release signer does not match build authority")
	}
	roles := append([]RollbackFloorRoleV1(nil), input.Roles...)
	sort.Slice(roles, func(i, j int) bool { return roles[i].Role < roles[j].Role })
	features := append([]string(nil), input.RequiredFeatures...)
	sort.Strings(features)
	input.Roles = roles
	input.RequiredFeatures = features
	if err := validateRollbackFloorInput(authority, authorityDigest, input); err != nil {
		return RollbackFloorBundleV1{}, RollbackFloorAcceptanceReceiptV1{}, err
	}

	bundle := RollbackFloorBundleV1{
		SchemaVersion: 1, Kind: "rollback_floor_bundle_v1", Purpose: "floor",
		ReleaseBuildAuthorityDigest: authorityDigest, SourceCommit: input.SourceCommit, SourceTreeDigest: input.SourceTreeDigest, ToolchainIdentity: input.ToolchainIdentity,
		Roles: roles, BuildReportDigest: input.BuildReportDigest, VetReportDigest: input.VetReportDigest, RaceTestReportDigest: input.RaceTestReportDigest,
		ConstructionUnitReportSetDigest: input.ConstructionUnitReportSetDigest, ConstructionUnits: append([]string(nil), input.ConstructionUnits...),
		LegacyAtomicWriterReachabilityCatalogueDigest: input.LegacyAtomicWriterReachabilityCatalogueDigest,
		ReleaseArtifactSetDigest:                      input.ReleaseArtifactSetDigest, RequiredLauncherABIDigest: input.RequiredLauncherABIDigest, RequiredFeatures: features,
		InstalledCorpusDigest: input.InstalledCorpusDigest, RecoveryCorpusDigest: input.RecoveryCorpusDigest, RollbackCorpusDigest: input.RollbackCorpusDigest,
		SignerPublicKey: authority.Ed25519PublicKey,
	}
	bundle.Signature, err = signRollbackFloorObject(privateKey, bundle, "signature")
	if err != nil {
		return RollbackFloorBundleV1{}, RollbackFloorAcceptanceReceiptV1{}, err
	}
	bundleDigest, err := releaseObjectDigestV1("cq/rollback-floor-bundle/v1\x00", bundle)
	if err != nil {
		return RollbackFloorBundleV1{}, RollbackFloorAcceptanceReceiptV1{}, err
	}
	supervisor, worker := roles[0], roles[1]
	receipt := RollbackFloorAcceptanceReceiptV1{
		SchemaVersion: 1, Kind: "rollback_floor_acceptance_v1", AcceptanceRunID: input.AcceptanceRunID,
		ReleaseBuildAuthorityDigest: authorityDigest, FloorReleaseBundleDigest: bundleDigest,
		FloorSourceCommit: input.SourceCommit, FloorSourceTreeDigest: input.SourceTreeDigest, FloorToolchainIdentity: input.ToolchainIdentity,
		FloorReleaseArtifactSetDigest: input.ReleaseArtifactSetDigest, FloorBuildReportDigest: input.BuildReportDigest,
		FloorConstructionUnitReportSetDigest:          input.ConstructionUnitReportSetDigest,
		LegacyAtomicWriterReachabilityCatalogueDigest: input.LegacyAtomicWriterReachabilityCatalogueDigest,
		FloorSupervisorArtifactDigest:                 supervisor.ArtifactDigest, FloorSupervisorArtifactManifestDigest: supervisor.ManifestDigest,
		FloorWorkerArtifactDigest: worker.ArtifactDigest, FloorWorkerArtifactManifestDigest: worker.ManifestDigest,
		VetReportDigest: input.VetReportDigest, RaceTestReportDigest: input.RaceTestReportDigest,
		RequiredLauncherABIDigest: input.RequiredLauncherABIDigest, RequiredFeatures: features,
		InstalledCorpusDigest: input.InstalledCorpusDigest, RecoveryCorpusDigest: input.RecoveryCorpusDigest, RollbackCorpusDigest: input.RollbackCorpusDigest,
		StartedAt: input.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), EndedAt: input.EndedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Nonce: input.Nonce, IssuerPublicKey: authority.Ed25519PublicKey,
	}
	receipt.Signature, err = signRollbackFloorObject(privateKey, receipt, "signature")
	if err != nil {
		return RollbackFloorBundleV1{}, RollbackFloorAcceptanceReceiptV1{}, err
	}
	return bundle, receipt, nil
}

func VerifyRollbackFloorBundle(authority ReleaseBuildAuthorityV1, bundle RollbackFloorBundleV1, receipt RollbackFloorAcceptanceReceiptV1, expected ReleaseAuthorityPinV1) error {
	if err := VerifyMinimalReleaseBuildAuthority(authority, expected); err != nil {
		return err
	}
	publicKey, err := decodeEd25519PublicKey(authority.Ed25519PublicKey)
	if err != nil {
		return err
	}
	authorityDigest, err := DigestReleaseBuildAuthority(authority)
	if err != nil {
		return err
	}
	startedAt, _ := time.Parse("2006-01-02T15:04:05Z", receipt.StartedAt)
	endedAt, _ := time.Parse("2006-01-02T15:04:05Z", receipt.EndedAt)
	input := RollbackFloorBuildInputV1{
		SchemaVersion: 1, AcceptanceRunID: receipt.AcceptanceRunID, ReleaseBuildAuthorityDigest: bundle.ReleaseBuildAuthorityDigest,
		SourceCommit: bundle.SourceCommit, SourceTreeDigest: bundle.SourceTreeDigest, ToolchainIdentity: bundle.ToolchainIdentity,
		Roles: bundle.Roles, BuildReportDigest: bundle.BuildReportDigest, VetReportDigest: bundle.VetReportDigest, RaceTestReportDigest: bundle.RaceTestReportDigest,
		ConstructionUnitReportSetDigest: bundle.ConstructionUnitReportSetDigest, ConstructionUnits: bundle.ConstructionUnits,
		LegacyAtomicWriterReachabilityCatalogueDigest: bundle.LegacyAtomicWriterReachabilityCatalogueDigest,
		ReleaseArtifactSetDigest:                      bundle.ReleaseArtifactSetDigest, RequiredLauncherABIDigest: bundle.RequiredLauncherABIDigest, RequiredFeatures: bundle.RequiredFeatures,
		InstalledCorpusDigest: bundle.InstalledCorpusDigest, RecoveryCorpusDigest: bundle.RecoveryCorpusDigest, RollbackCorpusDigest: bundle.RollbackCorpusDigest,
		StartedAt: startedAt, EndedAt: endedAt, Nonce: receipt.Nonce,
	}
	if bundle.SchemaVersion != 1 || bundle.Kind != "rollback_floor_bundle_v1" || bundle.Purpose != "floor" || bundle.SignerPublicKey != authority.Ed25519PublicKey {
		return fmt.Errorf("invalid rollback floor bundle identity")
	}
	if err := validateRollbackFloorInput(authority, authorityDigest, input); err != nil {
		return err
	}
	if err := verifyReleaseSignature(publicKey, bundle.Signature, releaseSignableWithoutFieldV1(bundle, "signature")); err != nil {
		return fmt.Errorf("verify rollback floor bundle signature: %w", err)
	}
	bundleDigest, err := releaseObjectDigestV1("cq/rollback-floor-bundle/v1\x00", bundle)
	if err != nil {
		return err
	}
	if err := validateRollbackFloorReceipt(receipt, authority, bundle, bundleDigest); err != nil {
		return err
	}
	if err := verifyReleaseSignature(publicKey, receipt.Signature, releaseSignableWithoutFieldV1(receipt, "signature")); err != nil {
		return fmt.Errorf("verify rollback floor receipt signature: %w", err)
	}
	return nil
}

func validateRollbackFloorInput(authority ReleaseBuildAuthorityV1, authorityDigest string, input RollbackFloorBuildInputV1) error {
	if input.SchemaVersion != 1 || input.AcceptanceRunID == "" || input.ReleaseBuildAuthorityDigest != authorityDigest || input.SourceCommit != authority.LineageRootCommit || input.SourceTreeDigest != authority.LineageRootTreeDigest || input.ToolchainIdentity != authority.ToolchainIdentity {
		return fmt.Errorf("rollback floor authority projection mismatch")
	}
	if len(input.Roles) != 2 || input.Roles[0].Role != "supervisor" || input.Roles[1].Role != "worker" {
		return fmt.Errorf("rollback floor roles must be exactly supervisor and worker")
	}
	for _, role := range input.Roles {
		if !lowerHex64Pattern.MatchString(role.ArtifactDigest) || !lowerHex64Pattern.MatchString(role.ManifestDigest) {
			return fmt.Errorf("rollback floor role digest is invalid")
		}
	}
	for _, digest := range []string{input.BuildReportDigest, input.VetReportDigest, input.RaceTestReportDigest, input.ConstructionUnitReportSetDigest, input.LegacyAtomicWriterReachabilityCatalogueDigest, input.ReleaseArtifactSetDigest, input.RequiredLauncherABIDigest, input.InstalledCorpusDigest, input.RecoveryCorpusDigest, input.RollbackCorpusDigest} {
		if !lowerHex64Pattern.MatchString(digest) {
			return fmt.Errorf("rollback floor digest is invalid")
		}
	}
	if !slices.Equal(input.ConstructionUnits, exactRollbackFloorConstructionUnits) {
		return fmt.Errorf("rollback floor construction units are not the exact CU-0 through CU-8 set")
	}
	if err := validateReleaseFeatureList(input.RequiredFeatures, "rollback floor required features"); err != nil {
		return err
	}
	if input.StartedAt.IsZero() != input.EndedAt.IsZero() {
		return fmt.Errorf("rollback floor time range is incomplete")
	}
	if !input.StartedAt.IsZero() {
		started := input.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
		ended := input.EndedAt.UTC().Format("2006-01-02T15:04:05Z")
		if err := validateReleaseTimestamp(started, "rollback floor started_at"); err != nil {
			return err
		}
		if err := validateReleaseTimestamp(ended, "rollback floor ended_at"); err != nil || !input.StartedAt.Before(input.EndedAt) {
			return fmt.Errorf("rollback floor time range is invalid")
		}
	}
	if len(input.Nonce) != 32 || input.Nonce != stringsToLower(input.Nonce) {
		return fmt.Errorf("rollback floor nonce is invalid")
	}
	if _, err := hex.DecodeString(input.Nonce); err != nil {
		return fmt.Errorf("rollback floor nonce is invalid")
	}
	return nil
}

func validateRollbackFloorReceipt(receipt RollbackFloorAcceptanceReceiptV1, authority ReleaseBuildAuthorityV1, bundle RollbackFloorBundleV1, bundleDigest string) error {
	if receipt.SchemaVersion != 1 || receipt.Kind != "rollback_floor_acceptance_v1" || receipt.AcceptanceRunID == "" || receipt.ReleaseBuildAuthorityDigest != bundle.ReleaseBuildAuthorityDigest || receipt.FloorReleaseBundleDigest != bundleDigest || receipt.FloorSourceCommit != bundle.SourceCommit || receipt.FloorSourceTreeDigest != bundle.SourceTreeDigest || receipt.FloorToolchainIdentity != bundle.ToolchainIdentity || receipt.IssuerPublicKey != authority.Ed25519PublicKey {
		return fmt.Errorf("rollback floor receipt authority projection mismatch")
	}
	if len(bundle.Roles) != 2 || receipt.FloorSupervisorArtifactDigest != bundle.Roles[0].ArtifactDigest || receipt.FloorSupervisorArtifactManifestDigest != bundle.Roles[0].ManifestDigest || receipt.FloorWorkerArtifactDigest != bundle.Roles[1].ArtifactDigest || receipt.FloorWorkerArtifactManifestDigest != bundle.Roles[1].ManifestDigest {
		return fmt.Errorf("rollback floor receipt role projection mismatch")
	}
	if receipt.FloorBuildReportDigest != bundle.BuildReportDigest || receipt.VetReportDigest != bundle.VetReportDigest || receipt.RaceTestReportDigest != bundle.RaceTestReportDigest || receipt.FloorConstructionUnitReportSetDigest != bundle.ConstructionUnitReportSetDigest || receipt.LegacyAtomicWriterReachabilityCatalogueDigest != bundle.LegacyAtomicWriterReachabilityCatalogueDigest || receipt.FloorReleaseArtifactSetDigest != bundle.ReleaseArtifactSetDigest || receipt.RequiredLauncherABIDigest != bundle.RequiredLauncherABIDigest || !slices.Equal(receipt.RequiredFeatures, bundle.RequiredFeatures) || receipt.InstalledCorpusDigest != bundle.InstalledCorpusDigest || receipt.RecoveryCorpusDigest != bundle.RecoveryCorpusDigest || receipt.RollbackCorpusDigest != bundle.RollbackCorpusDigest {
		return fmt.Errorf("rollback floor receipt evidence projection mismatch")
	}
	if err := validateReleaseTimestamp(receipt.StartedAt, "rollback floor receipt started_at"); err != nil {
		return err
	}
	if err := validateReleaseTimestamp(receipt.EndedAt, "rollback floor receipt ended_at"); err != nil {
		return err
	}
	started, _ := time.Parse("2006-01-02T15:04:05Z", receipt.StartedAt)
	ended, _ := time.Parse("2006-01-02T15:04:05Z", receipt.EndedAt)
	if !started.Before(ended) || len(receipt.Nonce) != 32 {
		return fmt.Errorf("rollback floor receipt time or nonce is invalid")
	}
	return nil
}

func signRollbackFloorObject(privateKey ed25519.PrivateKey, value any, signatureField string) (string, error) {
	signable := releaseSignableWithoutFieldV1(value, signatureField)
	canonical, err := CanonicalJSONV1(signable)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(ed25519.Sign(privateKey, canonical)), nil
}

func mustDecodeReleasePublicKey(encoded string) ed25519.PublicKey {
	decoded, _ := decodeEd25519PublicKey(encoded)
	return decoded
}

func stringsToLower(value string) string {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(decoded)
}
