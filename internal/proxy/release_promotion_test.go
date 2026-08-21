package proxy

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestReleasePromotionRequiresDescendantAndEveryAdmissionProof(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	input := candidateReleasePromotionInputForTest()
	receipt, err := BuildCandidateReleasePromotion(input, key)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FloorSourceCommit == receipt.TargetSourceCommit || receipt.Digest == "" {
		t.Fatalf("receipt = %#v", receipt)
	}

	for name, mutate := range map[string]func(*CandidateReleasePromotionInputV1){
		"same source":       func(input *CandidateReleasePromotionInputV1) { input.TargetSourceCommit = input.FloorSourceCommit },
		"no barrier":        func(input *CandidateReleasePromotionInputV1) { input.ClientBarrierReceiptDigest = "" },
		"no control health": func(input *CandidateReleasePromotionInputV1) { input.CandidateControlHealthReceiptDigest = "" },
		"no broker":         func(input *CandidateReleasePromotionInputV1) { input.CandidateBrokerSealDigest = "" },
		"no confinement":    func(input *CandidateReleasePromotionInputV1) { input.CandidateConfinementReceiptDigest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := input
			candidate.SourceAncestry = append([]string(nil), input.SourceAncestry...)
			mutate(&candidate)
			if _, err := BuildCandidateReleasePromotion(candidate, key); err == nil {
				t.Fatal("invalid promotion accepted")
			}
		})
	}
}

func TestReleaseImportPublishesFloorBeforePromotionAndResumes(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	receipt, err := BuildCandidateReleasePromotion(candidateReleasePromotionInputForTest(), key)
	if err != nil {
		t.Fatal(err)
	}
	floor := []byte(`{"kind":"rollback_floor_acceptance_receipt_v1"}`)
	floorDigest := releasePromotionDigest("cq/release-import-floor/v1\x00", floor)
	receipt.RollbackFloorAcceptanceReceiptDigest = floorDigest
	receipt.MAC = releasePromotionMAC(key, receipt)
	receipt.Digest = releasePromotionReceiptDigest(receipt)
	promotion, err := canonicalReleasePromotion(receipt)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "canonical")
	hookErr := errors.New("crash after floor")
	store, err := OpenReleaseImportStore(fsutil.OSFileSystem{}, path, key, func(phase string) error {
		if phase == "floor_durable" {
			return hookErr
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Import(context.Background(), floor, promotion); !errors.Is(err, hookErr) {
		t.Fatalf("first import error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenReleaseImportStore(fsutil.OSFileSystem{}, path, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	imported, err := reopened.Import(context.Background(), floor, promotion)
	if err != nil {
		t.Fatal(err)
	}
	if imported.RollbackFloorAcceptanceReceiptDigest != floorDigest || imported.CandidateReleasePromotionReceiptDigest != receipt.Digest || imported.SelectorDigest == "" {
		t.Fatalf("imported = %#v", imported)
	}
}

func candidateReleasePromotionInputForTest() CandidateReleasePromotionInputV1 {
	return CandidateReleasePromotionInputV1{
		SchemaVersion:                        1,
		FloorSourceCommit:                    "0123456789abcdef0123456789abcdef01234567",
		TargetSourceCommit:                   "1123456789abcdef0123456789abcdef01234567",
		SourceAncestry:                       []string{"0123456789abcdef0123456789abcdef01234567", "1123456789abcdef0123456789abcdef01234567"},
		TargetReleaseBundleDigest:            strings.Repeat("1", 64),
		RollbackFloorAcceptanceReceiptDigest: strings.Repeat("2", 64),
		ClientBarrierReceiptDigest:           strings.Repeat("3", 64),
		ClientStopProofDigest:                strings.Repeat("4", 64),
		CandidateControlHealthReceiptDigest:  strings.Repeat("5", 64),
		CandidateBrokerSealDigest:            strings.Repeat("6", 64),
		CandidateConfinementReceiptDigest:    strings.Repeat("7", 64),
		CandidateStageReceiptDigest:          strings.Repeat("8", 64),
		CompletedAt:                          time.Unix(1_700_000_000, 0).UTC(),
		Nonce:                                strings.Repeat("a", 32),
	}
}
