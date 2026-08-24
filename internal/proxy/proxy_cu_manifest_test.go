package proxy

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/unix"
)

func TestReleaseCanonicalStoresPublishAndAdoptSixCurrentSchemas(t *testing.T) {
	root := newReleaseCanonicalStoreRootForTest(t)
	store, err := openReleaseCanonicalStoresV1(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	floor := signedFloorReleaseGraph(t)
	target := signedTargetReleaseGraph(t)

	setDigest, err := store.publishReleaseArtifactSet(floor.ArtifactSet)
	if err != nil {
		t.Fatal(err)
	}
	setBytes, _ := CanonicalJSONV1(floor.ArtifactSet)
	if _, adopted, err := store.adoptReleaseArtifactSet(bytes.NewReader(setBytes)); err != nil || adopted != setDigest {
		t.Fatalf("adopt artifact set = %q, %v; want %q", adopted, err, setDigest)
	}

	ancestryDigest, err := store.publishSourceAncestryReceipt(*target.Ancestry)
	if err != nil {
		t.Fatal(err)
	}
	ancestryBytes, _ := CanonicalJSONV1(*target.Ancestry)
	if _, adopted, err := store.adoptSourceAncestryReceipt(bytes.NewReader(ancestryBytes)); err != nil || adopted != ancestryDigest {
		t.Fatalf("adopt ancestry = %q, %v; want %q", adopted, err, ancestryDigest)
	}

	reportDigest, err := store.publishReleaseBuildReport(floor.BuildReport)
	if err != nil {
		t.Fatal(err)
	}
	reportBytes, _ := CanonicalJSONV1(floor.BuildReport)
	if _, adopted, err := store.adoptReleaseBuildReport(bytes.NewReader(reportBytes)); err != nil || adopted != reportDigest {
		t.Fatalf("adopt report = %q, %v; want %q", adopted, err, reportDigest)
	}

	cuDigest, err := store.publishConstructionUnitReportSet(floor.CUReportSet)
	if err != nil {
		t.Fatal(err)
	}
	cuBytes, _ := CanonicalJSONV1(floor.CUReportSet)
	if _, adopted, err := store.adoptConstructionUnitReportSet(bytes.NewReader(cuBytes)); err != nil || adopted != cuDigest {
		t.Fatalf("adopt CU set = %q, %v; want %q", adopted, err, cuDigest)
	}

	authorityDigest, err := store.publishReleaseBuildAuthority(floor.Authority)
	if err != nil {
		t.Fatal(err)
	}
	authorityBytes, _ := CanonicalJSONV1(floor.Authority)
	if _, adopted, err := store.adoptReleaseBuildAuthority(bytes.NewReader(authorityBytes)); err != nil || adopted != authorityDigest {
		t.Fatalf("adopt authority = %q, %v; want %q", adopted, err, authorityDigest)
	}

	bundleDigest, err := store.publishReleaseBundle(floor.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundleBytes, _ := CanonicalJSONV1(floor.Bundle)
	if _, adopted, err := store.adoptReleaseBundle(bytes.NewReader(bundleBytes)); err != nil || adopted != bundleDigest {
		t.Fatalf("adopt bundle = %q, %v; want %q", adopted, err, bundleDigest)
	}

	for relative, digest := range map[string]string{
		"release-sets/" + setDigest + ".json":                         setDigest,
		"release-reports/" + ancestryDigest + ".json":                 ancestryDigest,
		"release-reports/" + reportDigest + ".json":                   reportDigest,
		"release-reports/" + cuDigest + ".json":                       cuDigest,
		"release-provenance/authorities/" + authorityDigest + ".json": authorityDigest,
		"release-provenance/bundles/" + bundleDigest + ".json":        bundleDigest,
	} {
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil || len(data) == 0 || !strings.Contains(relative, digest) {
			t.Fatalf("stored %s = %d bytes, %v", relative, len(data), err)
		}
	}
}

func TestReleaseCanonicalStoresSerialiseConcurrentAuthorityBoundary(t *testing.T) {
	store := openReleaseCanonicalStoresForTest(t)
	start := make(chan struct{})
	var wait sync.WaitGroup
	errorsByIndex := make([]error, 32)
	authorities := make([]ReleaseBuildAuthorityV1, len(errorsByIndex))
	for index := range authorities {
		authorities[index] = signedFloorReleaseGraph(t).Authority
		authorities[index].AuthorityID = fmt.Sprintf("concurrent-authority-%02d", index)
	}
	for index := range errorsByIndex {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, errorsByIndex[index] = store.publishReleaseBuildAuthority(authorities[index])
		}()
	}
	close(start)
	wait.Wait()
	succeeded := 0
	for _, err := range errorsByIndex {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 8 {
		t.Fatalf("concurrent authority successes = %d, want 8", succeeded)
	}
	counts, err := store.scanReleaseCanonicalStoresV1()
	if err != nil {
		t.Fatal(err)
	}
	if counts.authorities != 8 || counts.provenanceTemps != 0 {
		t.Fatalf("concurrent authority counts = %+v, want 8 authorities and no temps", counts)
	}
}

func TestReleaseCanonicalStoresSerialiseIndependentStoreInstances(t *testing.T) {
	root := newReleaseCanonicalStoreRootForTest(t)
	stores := make([]*releaseCanonicalStoresV1, 4)
	for index := range stores {
		store, err := openReleaseCanonicalStoresV1(root)
		if err != nil {
			t.Fatal(err)
		}
		stores[index] = store
	}
	t.Cleanup(func() {
		for _, store := range stores {
			_ = store.close()
		}
	})

	start := make(chan struct{})
	var wait sync.WaitGroup
	errorsByIndex := make([]error, 32)
	authorities := make([]ReleaseBuildAuthorityV1, len(errorsByIndex))
	for index := range authorities {
		authorities[index] = signedFloorReleaseGraph(t).Authority
		authorities[index].AuthorityID = fmt.Sprintf("independent-store-authority-%02d", index)
	}
	for index := range errorsByIndex {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, errorsByIndex[index] = stores[index%len(stores)].publishReleaseBuildAuthority(authorities[index])
		}()
	}
	close(start)
	wait.Wait()
	succeeded := 0
	for _, err := range errorsByIndex {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 8 {
		t.Fatalf("independent store authority successes = %d, want 8", succeeded)
	}
	counts, err := stores[0].scanReleaseCanonicalStoresV1()
	if err != nil {
		t.Fatal(err)
	}
	if counts.authorities != 8 || counts.provenanceTemps != 0 {
		t.Fatalf("independent store counts = %+v, want 8 authorities and no temps", counts)
	}
}

func TestReleaseCanonicalStoresCloseCrossTypeDurabilityBeforeUnrelatedRetry(t *testing.T) {
	floor := signedFloorReleaseGraph(t)
	target := signedTargetReleaseGraph(t)
	for _, fixture := range []struct {
		name      string
		relative  string
		tag       string
		domain    string
		value     any
		install   func(*releaseCanonicalStoresV1, fsutil.SecureDirectory)
		unrelated func(*releaseCanonicalStoresV1) error
	}{
		{
			name: "sets", relative: "release-sets", tag: "release-artifact-set-v1", domain: "cq/release-artifact-set/v1\x00", value: floor.ArtifactSet,
			install: func(store *releaseCanonicalStoresV1, directory fsutil.SecureDirectory) { store.sets = directory },
			unrelated: func(store *releaseCanonicalStoresV1) error {
				_, err := store.publishReleaseBuildAuthority(floor.Authority)
				return err
			},
		},
		{
			name: "reports", relative: "release-reports", tag: "source-ancestry-receipt-v1", domain: "cq/source-ancestry-receipt/v1\x00", value: *target.Ancestry,
			install: func(store *releaseCanonicalStoresV1, directory fsutil.SecureDirectory) { store.reports = directory },
			unrelated: func(store *releaseCanonicalStoresV1) error {
				_, err := store.publishReleaseBuildAuthority(floor.Authority)
				return err
			},
		},
		{
			name: "authorities", relative: "release-provenance/authorities", tag: "release-build-authority-v1", domain: "cq/release-build-authority/v1\x00", value: floor.Authority,
			install: func(store *releaseCanonicalStoresV1, directory fsutil.SecureDirectory) { store.authorities = directory },
			unrelated: func(store *releaseCanonicalStoresV1) error {
				_, err := store.publishReleaseArtifactSet(floor.ArtifactSet)
				return err
			},
		},
		{
			name: "bundles", relative: "release-provenance/bundles", tag: "release-bundle-v1", domain: "cq/release-bundle/v1\x00", value: floor.Bundle,
			install: func(store *releaseCanonicalStoresV1, directory fsutil.SecureDirectory) { store.bundles = directory },
			unrelated: func(store *releaseCanonicalStoresV1) error {
				_, err := store.publishReleaseArtifactSet(floor.ArtifactSet)
				return err
			},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := newReleaseCanonicalStoreRootForTest(t)
			store, err := openReleaseCanonicalStoresV1(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.close() })
			canonical := mustCanonicalForTest(t, fixture.value)
			digest := releaseObjectDigestForTest(t, fixture.domain, fixture.value)
			temp := ".cq-" + fixture.tag + "-" + digest + "-aaaaaaaaaaaaaaaa.tmp"
			if err := os.WriteFile(filepath.Join(root, fixture.relative, temp), canonical, 0o600); err != nil {
				t.Fatal(err)
			}
			var retained fsutil.SecureDirectory
			switch fixture.name {
			case "sets":
				retained = store.sets
			case "reports":
				retained = store.reports
			case "authorities":
				retained = store.authorities
			case "bundles":
				retained = store.bundles
			}
			fault := &durabilityRetryReleaseCanonicalDirectory{SecureDirectory: retained, finalName: digest + ".json"}
			fixture.install(store, fault)

			if err := fixture.unrelated(store); err == nil {
				t.Fatal("temporary promotion with failed leaf directory sync returned success")
			}
			if _, err := os.Stat(filepath.Join(root, fixture.relative, digest+".json")); err != nil {
				t.Fatalf("rename did not complete before injected sync failure: %v", err)
			}
			if err := fixture.unrelated(store); err != nil {
				t.Fatal(err)
			}
			if fault.recoverySyncs == 0 || fault.reopensAfterRecovery == 0 {
				t.Fatalf("unrelated retry recovery = %d syncs, %d reopens; want both", fault.recoverySyncs, fault.reopensAfterRecovery)
			}
		})
	}
}

type durabilityRetryReleaseCanonicalDirectory struct {
	fsutil.SecureDirectory
	finalName            string
	renameCompleted      bool
	injectedFailure      bool
	recoveryCompleted    bool
	recoverySyncs        int
	reopensAfterRecovery int
}

func (directory *durabilityRetryReleaseCanonicalDirectory) ReadDir() ([]os.DirEntry, error) {
	return directory.SecureDirectory.(fsutil.SecureDirectoryReader).ReadDir()
}

func (directory *durabilityRetryReleaseCanonicalDirectory) RenameNoReplace(oldName, newName string) error {
	err := directory.SecureDirectory.RenameNoReplace(oldName, newName)
	if err == nil && newName == directory.finalName {
		directory.renameCompleted = true
	}
	return err
}

func (directory *durabilityRetryReleaseCanonicalDirectory) Sync() error {
	if directory.renameCompleted && !directory.injectedFailure {
		directory.injectedFailure = true
		return errors.New("injected report directory sync failure")
	}
	err := directory.SecureDirectory.Sync()
	if err == nil && directory.injectedFailure {
		directory.recoveryCompleted = true
		directory.recoverySyncs++
	}
	return err
}

func (directory *durabilityRetryReleaseCanonicalDirectory) OpenNoFollow(name string) (fsutil.SecureReadFile, error) {
	if directory.recoveryCompleted && name == directory.finalName {
		directory.reopensAfterRecovery++
	}
	return directory.SecureDirectory.OpenNoFollow(name)
}

func TestReleaseCanonicalRecoveryRejectsGlobalOverCardinalityBeforeMutation(t *testing.T) {
	for _, fixture := range []struct {
		name string
		seed func(*testing.T, string)
		act  func(*testing.T, *releaseCanonicalStoresV1, string) error
	}{
		{
			name: "nine existing sets block unrelated publish",
			seed: func(t *testing.T, root string) {
				for index := 0; index < 9; index++ {
					value := signedFloorReleaseGraph(t).ArtifactSet
					value.SupportedFeatures = []string{fmt.Sprintf("global-set-%02d", index)}
					resignReleaseObjectForTest(t, &value)
					seedReleaseCanonicalFinalForTest(t, root, "release-sets", "cq/release-artifact-set/v1\x00", value)
				}
			},
			act: func(t *testing.T, store *releaseCanonicalStoresV1, _ string) error {
				_, err := store.publishReleaseBuildAuthority(signedFloorReleaseGraph(t).Authority)
				return err
			},
		},
		{
			name: "forty-one existing reports block unrelated adopt",
			seed: func(t *testing.T, root string) {
				for index := 0; index < 41; index++ {
					value := signedFloorReleaseGraph(t).BuildReport
					value.StartedAt = fmt.Sprintf("2026-08-18T00:%02d:00Z", index)
					value.EndedAt = fmt.Sprintf("2026-08-18T00:%02d:01Z", index)
					resignReleaseObjectForTest(t, &value)
					seedReleaseCanonicalFinalForTest(t, root, "release-reports", "cq/release-build-report/v1\x00", value)
				}
			},
			act: func(t *testing.T, store *releaseCanonicalStoresV1, _ string) error {
				canonical := mustCanonicalForTest(t, signedFloorReleaseGraph(t).Authority)
				_, _, err := store.adoptReleaseBuildAuthority(bytes.NewReader(canonical))
				return err
			},
		},
		{
			name: "nine existing authorities block unrelated temp promotion",
			seed: func(t *testing.T, root string) {
				for index := 0; index < 9; index++ {
					value := signedFloorReleaseGraph(t).Authority
					value.AuthorityID = fmt.Sprintf("global-authority-%02d", index)
					seedReleaseCanonicalFinalForTest(t, root, "release-provenance/authorities", "cq/release-build-authority/v1\x00", value)
				}
				value := *signedTargetReleaseGraph(t).Ancestry
				seedReleaseCanonicalTempForTest(t, root, "release-reports", "source-ancestry-receipt-v1", "cq/source-ancestry-receipt/v1\x00", value)
			},
			act: func(t *testing.T, store *releaseCanonicalStoresV1, _ string) error {
				_, err := store.publishReleaseBundle(signedFloorReleaseGraph(t).Bundle)
				return err
			},
		},
		{
			name: "nine existing bundles block unrelated adopt and temp promotion",
			seed: func(t *testing.T, root string) {
				for index := 0; index < 9; index++ {
					value := signedFloorReleaseGraph(t).Bundle
					value.ReleaseArtifactSetDigest = fmt.Sprintf("%064x", index+1)
					resignReleaseObjectForTest(t, &value)
					seedReleaseCanonicalFinalForTest(t, root, "release-provenance/bundles", "cq/release-bundle/v1\x00", value)
				}
				value := signedFloorReleaseGraph(t).BuildReport
				seedReleaseCanonicalTempForTest(t, root, "release-reports", "release-build-report-v1", "cq/release-build-report/v1\x00", value)
			},
			act: func(t *testing.T, store *releaseCanonicalStoresV1, _ string) error {
				canonical := mustCanonicalForTest(t, signedFloorReleaseGraph(t).ArtifactSet)
				_, _, err := store.adoptReleaseArtifactSet(bytes.NewReader(canonical))
				return err
			},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := newReleaseCanonicalStoreRootForTest(t)
			fixture.seed(t, root)
			store, err := openReleaseCanonicalStoresV1(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.close() })
			before := releaseCanonicalLeafSnapshotForTest(t, root)
			if err := fixture.act(t, store, root); err == nil {
				t.Fatal("unrelated operation accepted globally over-cardinality recovery state")
			}
			after := releaseCanonicalLeafSnapshotForTest(t, root)
			if !slices.Equal(before, after) {
				t.Fatalf("globally over-cardinality operation mutated store\nbefore: %v\nafter:  %v", before, after)
			}
		})
	}
}

func TestReleaseCanonicalRecoveryLeavesMalformedTempsUntouchedWhenStoreIsOverCapacity(t *testing.T) {
	root := newReleaseCanonicalStoreRootForTest(t)
	for index := 0; index < 9; index++ {
		value := signedFloorReleaseGraph(t).ArtifactSet
		value.SupportedFeatures = []string{fmt.Sprintf("malformed-overcount-set-%02d", index)}
		resignReleaseObjectForTest(t, &value)
		seedReleaseCanonicalFinalForTest(t, root, "release-sets", "cq/release-artifact-set/v1\x00", value)
	}
	name := ".cq-release-build-report-v1-" + strings.Repeat("a", 64) + "-aaaaaaaaaaaaaaaa.tmp"
	if err := os.WriteFile(filepath.Join(root, "release-reports", name), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openReleaseCanonicalStoresV1(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	before := releaseCanonicalLeafSnapshotForTest(t, root)
	if _, err := store.publishReleaseBuildAuthority(signedFloorReleaseGraph(t).Authority); err == nil {
		t.Fatal("globally over-capacity store cleaned malformed temporary")
	}
	after := releaseCanonicalLeafSnapshotForTest(t, root)
	if !slices.Equal(before, after) {
		t.Fatalf("over-capacity malformed-temp recovery mutated store\nbefore: %v\nafter:  %v", before, after)
	}
}

func TestReleaseCanonicalRecoveryExcludesStagedInertTempsFromProjection(t *testing.T) {
	t.Run("oversized build report", func(t *testing.T) {
		root := newReleaseCanonicalStoreRootForTest(t)
		name := ".cq-release-build-report-v1-" + strings.Repeat("a", 64) + "-aaaaaaaaaaaaaaaa.tmp"
		if err := os.WriteFile(filepath.Join(root, "release-reports", name), bytes.Repeat([]byte("x"), (64<<10)+1), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := openReleaseCanonicalStoresV1(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.close() })
		tracker := &cleanupTrackingReleaseCanonicalDirectory{SecureDirectory: store.reports}
		store.reports = tracker
		if _, err := store.publishReleaseBuildAuthority(signedFloorReleaseGraph(t).Authority); err != nil {
			t.Fatal(err)
		}
		assertReleaseCanonicalTempsCleanedAndSynced(t, root, "release-reports", tracker, []string{name})
	})

	t.Run("five malformed registered temps", func(t *testing.T) {
		root := newReleaseCanonicalStoreRootForTest(t)
		var names []string
		for index := 0; index < 5; index++ {
			name := fmt.Sprintf(".cq-release-artifact-set-v1-%064x-aaaaaaaaaaaaaaaa.tmp", index+1)
			names = append(names, name)
			if err := os.WriteFile(filepath.Join(root, "release-sets", name), []byte("malformed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		store, err := openReleaseCanonicalStoresV1(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.close() })
		tracker := &cleanupTrackingReleaseCanonicalDirectory{SecureDirectory: store.sets}
		store.sets = tracker
		if _, err := store.publishReleaseBuildAuthority(signedFloorReleaseGraph(t).Authority); err != nil {
			t.Fatal(err)
		}
		assertReleaseCanonicalTempsCleanedAndSynced(t, root, "release-sets", tracker, names)
	})

	t.Run("unknown oversized residue", func(t *testing.T) {
		root := newReleaseCanonicalStoreRootForTest(t)
		name := "unregistered-oversized.tmp"
		if err := os.WriteFile(filepath.Join(root, "release-reports", name), bytes.Repeat([]byte("x"), (64<<10)+1), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := openReleaseCanonicalStoresV1(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.close() })
		before := releaseCanonicalLeafSnapshotForTest(t, root)
		if _, err := store.publishReleaseBuildAuthority(signedFloorReleaseGraph(t).Authority); err == nil {
			t.Fatal("unknown oversized residue did not block publication")
		}
		after := releaseCanonicalLeafSnapshotForTest(t, root)
		if !slices.Equal(before, after) {
			t.Fatalf("unknown oversized residue was mutated\nbefore: %v\nafter:  %v", before, after)
		}
	})
}

type cleanupTrackingReleaseCanonicalDirectory struct {
	fsutil.SecureDirectory
	removed          []string
	removePending    bool
	syncsAfterRemove int
}

func (directory *cleanupTrackingReleaseCanonicalDirectory) ReadDir() ([]os.DirEntry, error) {
	return directory.SecureDirectory.(fsutil.SecureDirectoryReader).ReadDir()
}

func (directory *cleanupTrackingReleaseCanonicalDirectory) Remove(name string) error {
	if err := directory.SecureDirectory.Remove(name); err != nil {
		return err
	}
	directory.removed = append(directory.removed, name)
	directory.removePending = true
	return nil
}

func (directory *cleanupTrackingReleaseCanonicalDirectory) Sync() error {
	if err := directory.SecureDirectory.Sync(); err != nil {
		return err
	}
	if directory.removePending {
		directory.syncsAfterRemove++
		directory.removePending = false
	}
	return nil
}

func assertReleaseCanonicalTempsCleanedAndSynced(t *testing.T, root, relative string, tracker *cleanupTrackingReleaseCanonicalDirectory, names []string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(root, relative, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staged inert temporary %q remains: %v", name, err)
		}
	}
	removed := slices.Clone(tracker.removed)
	wantNames := slices.Clone(names)
	sort.Strings(removed)
	sort.Strings(wantNames)
	if !slices.Equal(removed, wantNames) {
		t.Fatalf("removed temporaries = %v, want %v", removed, wantNames)
	}
	if tracker.syncsAfterRemove != len(names) {
		t.Fatalf("directory syncs after removal = %d, want %d", tracker.syncsAfterRemove, len(names))
	}
}

func seedReleaseCanonicalFinalForTest(t *testing.T, root, relative, domain string, value any) {
	t.Helper()
	canonical := mustCanonicalForTest(t, value)
	digest := releaseObjectDigestForTest(t, domain, value)
	if err := os.WriteFile(filepath.Join(root, relative, digest+".json"), canonical, 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedReleaseCanonicalTempForTest(t *testing.T, root, relative, tag, domain string, value any) {
	t.Helper()
	canonical := mustCanonicalForTest(t, value)
	digest := releaseObjectDigestForTest(t, domain, value)
	name := ".cq-" + tag + "-" + digest + "-aaaaaaaaaaaaaaaa.tmp"
	if err := os.WriteFile(filepath.Join(root, relative, name), canonical, 0o600); err != nil {
		t.Fatal(err)
	}
}

func releaseCanonicalLeafSnapshotForTest(t *testing.T, root string) []string {
	t.Helper()
	var snapshot []string
	for _, relative := range []string{"release-sets", "release-reports", "release-provenance/authorities", "release-provenance/bundles"} {
		entries, err := os.ReadDir(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(root, relative, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			snapshot = append(snapshot, relative+"/"+entry.Name()+":"+hex.EncodeToString(data))
		}
	}
	sort.Strings(snapshot)
	return snapshot
}

func TestReleaseCanonicalStoresReconcileEveryPolicyBeforeUnrelatedPublish(t *testing.T) {
	root := newReleaseCanonicalStoreRootForTest(t)
	store, err := openReleaseCanonicalStoresV1(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	ancestry := *signedTargetReleaseGraph(t).Ancestry
	canonical := mustCanonicalForTest(t, ancestry)
	digest := releaseObjectDigestForTest(t, "cq/source-ancestry-receipt/v1\x00", ancestry)
	temp := ".cq-source-ancestry-receipt-v1-" + digest + "-aaaaaaaaaaaaaaaa.tmp"
	if err := os.WriteFile(filepath.Join(root, "release-reports", temp), canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishReleaseBuildAuthority(signedFloorReleaseGraph(t).Authority); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "release-reports", digest+".json")); err != nil {
		t.Fatalf("unrelated publish did not reconcile ancestry temp: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "release-reports", temp)); !os.IsNotExist(err) {
		t.Fatalf("reconciled ancestry temp remains: %v", err)
	}
}

func TestReleaseCanonicalStoresRefuseValidOverCapacityTempsBeforePromotion(t *testing.T) {
	t.Run("ninth set", func(t *testing.T) {
		root := newReleaseCanonicalStoreRootForTest(t)
		store, err := openReleaseCanonicalStoresV1(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.close() })
		for index := 0; index < 8; index++ {
			set := signedFloorReleaseGraph(t).ArtifactSet
			set.SupportedFeatures = []string{fmt.Sprintf("feature-%02d", index)}
			resignReleaseObjectForTest(t, &set)
			if _, err := store.publishReleaseArtifactSet(set); err != nil {
				t.Fatal(err)
			}
		}
		set := signedFloorReleaseGraph(t).ArtifactSet
		set.SupportedFeatures = []string{"feature-09"}
		resignReleaseObjectForTest(t, &set)
		canonical := mustCanonicalForTest(t, set)
		digest := releaseObjectDigestForTest(t, "cq/release-artifact-set/v1\x00", set)
		temp := ".cq-release-artifact-set-v1-" + digest + "-aaaaaaaaaaaaaaaa.tmp"
		if err := os.WriteFile(filepath.Join(root, "release-sets", temp), canonical, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.publishReleaseBuildAuthority(signedFloorReleaseGraph(t).Authority); err == nil {
			t.Fatal("unrelated publish promoted a valid ninth set temp")
		}
		if _, err := os.Stat(filepath.Join(root, "release-sets", temp)); err != nil {
			t.Fatalf("refused ninth set temp was mutated: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "release-sets", digest+".json")); !os.IsNotExist(err) {
			t.Fatalf("valid ninth set temp was promoted: %v", err)
		}
	})

	t.Run("forty-first report", func(t *testing.T) {
		root := newReleaseCanonicalStoreRootForTest(t)
		store, err := openReleaseCanonicalStoresV1(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.close() })
		for index := 0; index < 40; index++ {
			report := signedFloorReleaseGraph(t).BuildReport
			report.StartedAt = fmt.Sprintf("2026-08-18T00:%02d:00Z", index)
			report.EndedAt = fmt.Sprintf("2026-08-18T00:%02d:01Z", index)
			resignReleaseObjectForTest(t, &report)
			if _, err := store.publishReleaseBuildReport(report); err != nil {
				t.Fatal(err)
			}
		}
		report := signedFloorReleaseGraph(t).BuildReport
		report.StartedAt = "2026-08-18T01:00:00Z"
		report.EndedAt = "2026-08-18T01:00:01Z"
		resignReleaseObjectForTest(t, &report)
		canonical := mustCanonicalForTest(t, report)
		digest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", report)
		temp := ".cq-release-build-report-v1-" + digest + "-bbbbbbbbbbbbbbbb.tmp"
		if err := os.WriteFile(filepath.Join(root, "release-reports", temp), canonical, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.publishReleaseBuildAuthority(signedFloorReleaseGraph(t).Authority); err == nil {
			t.Fatal("unrelated publish promoted a valid forty-first report temp")
		}
		if _, err := os.Stat(filepath.Join(root, "release-reports", temp)); err != nil {
			t.Fatalf("refused forty-first report temp was mutated: %v", err)
		}
	})
}

func TestReleaseCanonicalStoresRefuseAggregatePlusOneWithValidTempsBeforePromotion(t *testing.T) {
	store := openReleaseCanonicalStoresForTest(t)
	floor := signedFloorReleaseGraph(t)
	for _, fixture := range []struct {
		name   string
		counts releaseCanonicalStoreCountsV1
		temp   releaseCanonicalTempV1
	}{
		{
			name:   "set report aggregate",
			counts: releaseCanonicalStoreCountsV1{setReportTemps: 1, setReportBytes: releaseSetReportStoreMaxBytes + 1},
			temp: releaseCanonicalTempV1{
				policy: store.artifactSetPolicy(), data: mustCanonicalForTest(t, floor.ArtifactSet),
			},
		},
		{
			name:   "provenance aggregate",
			counts: releaseCanonicalStoreCountsV1{provenanceTemps: 1, provenanceBytes: releaseProvenanceMaxBytes + 1},
			temp: releaseCanonicalTempV1{
				policy: store.authorityPolicy(), data: mustCanonicalForTest(t, floor.Authority),
			},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if err := store.validateReleaseCanonicalRecoveryCapacityV1(fixture.counts, []releaseCanonicalTempV1{fixture.temp}); err == nil {
				t.Fatal("aggregate cap plus one with a valid temporary was accepted")
			}
		})
	}
}

func TestReleaseCanonicalStoresRequireIntrinsicValidityForEveryCurrentSchema(t *testing.T) {
	floor := signedFloorReleaseGraph(t)
	target := signedTargetReleaseGraph(t)
	fixtures := []struct {
		name    string
		publish func(*releaseCanonicalStoresV1) error
	}{
		{name: "authority encoding", publish: func(store *releaseCanonicalStoresV1) error {
			value := floor.Authority
			value.RepositoryIdentityDigest = "not-a-digest"
			_, err := store.publishReleaseBuildAuthority(value)
			return err
		}},
		{name: "ancestry lineage", publish: func(store *releaseCanonicalStoresV1) error {
			value := *target.Ancestry
			value.MergeBaseCommit = strings.Repeat("f", 40)
			resignReleaseObjectForTest(t, &value)
			_, err := store.publishSourceAncestryReceipt(value)
			return err
		}},
		{name: "build role order", publish: func(store *releaseCanonicalStoresV1) error {
			value := floor.BuildReport
			slices.Reverse(value.RoleExecutions)
			resignReleaseObjectForTest(t, &value)
			_, err := store.publishReleaseBuildReport(value)
			return err
		}},
		{name: "build self signature", publish: func(store *releaseCanonicalStoresV1) error {
			value := floor.BuildReport
			value.Signature = "00" + value.Signature[2:]
			_, err := store.publishReleaseBuildReport(value)
			return err
		}},
		{name: "empty report roles must be an array", publish: func(store *releaseCanonicalStoresV1) error {
			value := floor.VetReport
			value.RoleExecutions = nil
			resignReleaseObjectForTest(t, &value)
			_, err := store.publishReleaseBuildReport(value)
			return err
		}},
		{name: "CU evidence digest", publish: func(store *releaseCanonicalStoresV1) error {
			value := floor.CUReportSet
			value.Reports[0].InvocationDigest = "bad"
			resignReleaseObjectForTest(t, &value)
			_, err := store.publishConstructionUnitReportSet(value)
			return err
		}},
		{name: "artifact role order", publish: func(store *releaseCanonicalStoresV1) error {
			value := floor.ArtifactSet
			slices.Reverse(value.Roles)
			resignReleaseObjectForTest(t, &value)
			_, err := store.publishReleaseArtifactSet(value)
			return err
		}},
		{name: "bundle entry size", publish: func(store *releaseCanonicalStoresV1) error {
			value := floor.Bundle
			value.Entries[0].Size = 0
			resignReleaseObjectForTest(t, &value)
			_, err := store.publishReleaseBundle(value)
			return err
		}},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			store := openReleaseCanonicalStoresForTest(t)
			if err := fixture.publish(store); err == nil {
				t.Fatal("intrinsically invalid current-schema object was admitted")
			}
		})
	}
}

func TestReleaseCanonicalStoresRejectMissingRequiredSignedMembers(t *testing.T) {
	floor := signedFloorReleaseGraph(t)
	target := signedTargetReleaseGraph(t)
	for _, fixture := range []struct {
		name       string
		value      any
		signedName string
		adopt      func(*releaseCanonicalStoresV1, io.Reader) error
	}{
		{name: "ancestry", value: *target.Ancestry, signedName: "signature", adopt: func(store *releaseCanonicalStoresV1, reader io.Reader) error {
			_, _, err := store.adoptSourceAncestryReceipt(reader)
			return err
		}},
		{name: "build report", value: floor.BuildReport, signedName: "signature", adopt: func(store *releaseCanonicalStoresV1, reader io.Reader) error {
			_, _, err := store.adoptReleaseBuildReport(reader)
			return err
		}},
		{name: "CU report set", value: floor.CUReportSet, signedName: "signature", adopt: func(store *releaseCanonicalStoresV1, reader io.Reader) error {
			_, _, err := store.adoptConstructionUnitReportSet(reader)
			return err
		}},
		{name: "artifact set", value: floor.ArtifactSet, signedName: "set_signature", adopt: func(store *releaseCanonicalStoresV1, reader io.Reader) error {
			_, _, err := store.adoptReleaseArtifactSet(reader)
			return err
		}},
		{name: "bundle", value: floor.Bundle, signedName: "bundle_signature", adopt: func(store *releaseCanonicalStoresV1, reader io.Reader) error {
			_, _, err := store.adoptReleaseBundle(reader)
			return err
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			encoded, err := json.Marshal(fixture.value)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatal(err)
			}
			delete(object, fixture.signedName)
			canonical, err := CanonicalJSONV1(object)
			if err != nil {
				t.Fatal(err)
			}
			store := openReleaseCanonicalStoresForTest(t)
			if err := fixture.adopt(store, bytes.NewReader(canonical)); err == nil {
				t.Fatal("adopted canonical object with omitted required signed member")
			}
		})
	}
}

func TestReleaseCanonicalStoresRejectProvenanceRootResidue(t *testing.T) {
	root := newReleaseCanonicalStoreRootForTest(t)
	if err := os.WriteFile(filepath.Join(root, "release-provenance", "residue"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if store, err := openReleaseCanonicalStoresV1(root); err == nil {
		_ = store.close()
		t.Fatal("accepted residue in retained provenance root")
	}
}

func TestOpenReleaseCanonicalStoresRejectsRootReplacementDuringRetainedOpen(t *testing.T) {
	root := newReleaseCanonicalStoreRootForTest(t)
	original := openReleaseSecureDirectoryV1
	t.Cleanup(func() { openReleaseSecureDirectoryV1 = original })
	openReleaseSecureDirectoryV1 = func(path string) (fsutil.SecureDirectory, error) {
		held := path + ".held"
		if err := os.Rename(path, held); err != nil {
			return nil, err
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return nil, err
		}
		return (fsutil.OSFileSystem{}).OpenSecureDirectory(path)
	}
	if store, err := openReleaseCanonicalStoresV1(root); err == nil {
		_ = store.close()
		t.Fatal("accepted root replacement between APFS validation and retained open")
	}
}

func TestReleaseCanonicalTempCleanupJoinsUnlinkAndDirectorySyncFailures(t *testing.T) {
	root := newReleaseCanonicalStoreRootForTest(t)
	store, err := openReleaseCanonicalStoresV1(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	removeErr := errors.New("injected remove failure")
	syncErr := errors.New("injected directory sync failure")
	store.sets = &faultingReleaseCanonicalDirectory{
		SecureDirectory: store.sets,
		removeErr:       removeErr,
		syncErr:         syncErr,
	}
	name := ".cq-release-artifact-set-v1-" + strings.Repeat("a", 64) + "-aaaaaaaaaaaaaaaa.tmp"
	if err := os.WriteFile(filepath.Join(root, "release-sets", name), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.publishReleaseBuildAuthority(signedFloorReleaseGraph(t).Authority)
	if !errors.Is(err, removeErr) || !errors.Is(err, syncErr) {
		t.Fatalf("cleanup error = %v, want joined remove and sync failures", err)
	}
}

type faultingReleaseCanonicalDirectory struct {
	fsutil.SecureDirectory
	removeErr error
	syncErr   error
	removed   bool
}

func (directory *faultingReleaseCanonicalDirectory) ReadDir() ([]os.DirEntry, error) {
	return directory.SecureDirectory.(fsutil.SecureDirectoryReader).ReadDir()
}

func (directory *faultingReleaseCanonicalDirectory) Remove(string) error {
	directory.removed = true
	return directory.removeErr
}

func (directory *faultingReleaseCanonicalDirectory) Sync() error {
	if directory.removed {
		return directory.syncErr
	}
	return directory.SecureDirectory.Sync()
}

func TestReleaseCanonicalStoresRejectMismatchedCollisionForEveryCurrentSchema(t *testing.T) {
	floor := signedFloorReleaseGraph(t)
	target := signedTargetReleaseGraph(t)
	for _, fixture := range []struct {
		name      string
		directory string
		domain    string
		value     any
		publish   func(*releaseCanonicalStoresV1) error
	}{
		{name: "artifact set", directory: "release-sets", domain: "cq/release-artifact-set/v1\x00", value: floor.ArtifactSet, publish: func(store *releaseCanonicalStoresV1) error {
			_, err := store.publishReleaseArtifactSet(floor.ArtifactSet)
			return err
		}},
		{name: "ancestry", directory: "release-reports", domain: "cq/source-ancestry-receipt/v1\x00", value: *target.Ancestry, publish: func(store *releaseCanonicalStoresV1) error {
			_, err := store.publishSourceAncestryReceipt(*target.Ancestry)
			return err
		}},
		{name: "build report", directory: "release-reports", domain: "cq/release-build-report/v1\x00", value: floor.BuildReport, publish: func(store *releaseCanonicalStoresV1) error {
			_, err := store.publishReleaseBuildReport(floor.BuildReport)
			return err
		}},
		{name: "CU report set", directory: "release-reports", domain: "cq/construction-unit-report-set/v1\x00", value: floor.CUReportSet, publish: func(store *releaseCanonicalStoresV1) error {
			_, err := store.publishConstructionUnitReportSet(floor.CUReportSet)
			return err
		}},
		{name: "authority", directory: "release-provenance/authorities", domain: "cq/release-build-authority/v1\x00", value: floor.Authority, publish: func(store *releaseCanonicalStoresV1) error {
			_, err := store.publishReleaseBuildAuthority(floor.Authority)
			return err
		}},
		{name: "bundle", directory: "release-provenance/bundles", domain: "cq/release-bundle/v1\x00", value: floor.Bundle, publish: func(store *releaseCanonicalStoresV1) error {
			_, err := store.publishReleaseBundle(floor.Bundle)
			return err
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := newReleaseCanonicalStoreRootForTest(t)
			store, err := openReleaseCanonicalStoresV1(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.close() })
			digest := releaseObjectDigestForTest(t, fixture.domain, fixture.value)
			if err := os.WriteFile(filepath.Join(root, fixture.directory, digest+".json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := fixture.publish(store); err == nil {
				t.Fatalf("mismatched collision error = %v", err)
			}
		})
	}
}

func TestReleaseCanonicalStoresRejectDescriptorTypeCanonicalAndCapDrift(t *testing.T) {
	t.Run("retained descriptor", func(t *testing.T) {
		root := newReleaseCanonicalStoreRootForTest(t)
		store, err := openReleaseCanonicalStoresV1(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.close() })
		held := filepath.Join(root, "held-release-sets")
		if err := os.Rename(filepath.Join(root, "release-sets"), held); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "release-sets"), 0o700); err != nil {
			t.Fatal(err)
		}
		digest, err := store.publishReleaseArtifactSet(signedFloorReleaseGraph(t).ArtifactSet)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(held, digest+".json")); err != nil {
			t.Fatalf("publication escaped retained descriptor: %v", err)
		}
		if entries, _ := os.ReadDir(filepath.Join(root, "release-sets")); len(entries) != 0 {
			t.Fatalf("replacement directory received publication: %v", entries)
		}
	})

	t.Run("unsafe category", func(t *testing.T) {
		root := newReleaseCanonicalStoreRootForTest(t)
		if err := os.Chmod(filepath.Join(root, "release-reports"), 0o755); err != nil {
			t.Fatal(err)
		}
		if store, err := openReleaseCanonicalStoresV1(root); err == nil {
			_ = store.close()
			t.Fatal("accepted non-owner-only category")
		}
	})

	t.Run("wrong type and noncanonical", func(t *testing.T) {
		store := openReleaseCanonicalStoresForTest(t)
		reportBytes, _ := CanonicalJSONV1(signedFloorReleaseGraph(t).BuildReport)
		if _, _, err := store.adoptReleaseArtifactSet(bytes.NewReader(reportBytes)); err == nil {
			t.Fatal("adopted build report as artifact set")
		}
		setBytes, _ := CanonicalJSONV1(signedFloorReleaseGraph(t).ArtifactSet)
		if _, _, err := store.adoptReleaseArtifactSet(bytes.NewReader(append(setBytes, '\n'))); err == nil {
			t.Fatal("adopted noncanonical artifact set")
		}
	})

	t.Run("exact caps", func(t *testing.T) {
		store := openReleaseCanonicalStoresForTest(t)
		set := signedFloorReleaseGraph(t).ArtifactSet
		set.SupportedFeatures = []string{strings.Repeat("x", (1<<20)-len(mustCanonicalForTest(t, set)))}
		for len(mustCanonicalForTest(t, set)) < 1<<20 {
			set.SupportedFeatures[0] += "x"
		}
		for len(mustCanonicalForTest(t, set)) > 1<<20 {
			set.SupportedFeatures[0] = set.SupportedFeatures[0][:len(set.SupportedFeatures[0])-1]
		}
		resignReleaseObjectForTest(t, &set)
		if _, err := store.publishReleaseArtifactSet(set); err != nil {
			t.Fatalf("exact 1 MiB set refused: %v", err)
		}
		set.SupportedFeatures[0] += "x"
		resignReleaseObjectForTest(t, &set)
		if _, err := store.publishReleaseArtifactSet(set); err == nil {
			t.Fatal("1 MiB + 1 set accepted")
		}
		authority := signedFloorReleaseGraph(t).Authority
		authority.AuthorityID = strings.Repeat("x", (64<<10)-len(mustCanonicalForTest(t, authority)))
		for len(mustCanonicalForTest(t, authority)) < 64<<10 {
			authority.AuthorityID += "x"
		}
		for len(mustCanonicalForTest(t, authority)) > 64<<10 {
			authority.AuthorityID = authority.AuthorityID[:len(authority.AuthorityID)-1]
		}
		if _, err := store.publishReleaseBuildAuthority(authority); err != nil {
			t.Fatalf("exact 64 KiB authority refused: %v", err)
		}
		authority.AuthorityID += "x"
		if _, err := store.publishReleaseBuildAuthority(authority); err == nil {
			t.Fatal("64 KiB + 1 authority accepted")
		}
	})
}

func TestReleaseCanonicalStoresEnforceCardinalityTemporaryAndAggregateBounds(t *testing.T) {
	t.Run("ninth set", func(t *testing.T) {
		store := openReleaseCanonicalStoresForTest(t)
		for index := 0; index < 9; index++ {
			set := signedFloorReleaseGraph(t).ArtifactSet
			set.SupportedFeatures = []string{fmt.Sprintf("feature-%d", index)}
			resignReleaseObjectForTest(t, &set)
			_, err := store.publishReleaseArtifactSet(set)
			if (index < 8) != (err == nil) {
				t.Fatalf("set %d error = %v", index+1, err)
			}
		}
	})
	t.Run("forty-first report", func(t *testing.T) {
		store := openReleaseCanonicalStoresForTest(t)
		for index := 0; index < 41; index++ {
			report := signedFloorReleaseGraph(t).BuildReport
			report.StartedAt = fmt.Sprintf("2026-08-18T00:%02d:00Z", index)
			report.EndedAt = fmt.Sprintf("2026-08-18T00:%02d:01Z", index)
			resignReleaseObjectForTest(t, &report)
			_, err := store.publishReleaseBuildReport(report)
			if (index < 40) != (err == nil) {
				t.Fatalf("report %d error = %v", index+1, err)
			}
		}
	})
	t.Run("fifth temporary", func(t *testing.T) {
		root := newReleaseCanonicalStoreRootForTest(t)
		store, err := openReleaseCanonicalStoresV1(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.close() })
		for index := 0; index < 5; index++ {
			report := signedFloorReleaseGraph(t).BuildReport
			report.StartedAt = fmt.Sprintf("2026-08-18T01:%02d:00Z", index)
			report.EndedAt = fmt.Sprintf("2026-08-18T01:%02d:01Z", index)
			resignReleaseObjectForTest(t, &report)
			canonical := mustCanonicalForTest(t, report)
			digest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", report)
			name := fmt.Sprintf(".cq-release-build-report-v1-%s-%016x.tmp", digest, index+1)
			if err := os.WriteFile(filepath.Join(root, "release-reports", name), canonical, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.publishReleaseArtifactSet(signedFloorReleaseGraph(t).ArtifactSet); err == nil {
			t.Fatal("fifth set/report temporary accepted")
		}
	})
	t.Run("ninth provenance authority", func(t *testing.T) {
		store := openReleaseCanonicalStoresForTest(t)
		for index := 0; index < 9; index++ {
			authority := signedFloorReleaseGraph(t).Authority
			authority.AuthorityID = fmt.Sprintf("authority-%d", index)
			_, err := store.publishReleaseBuildAuthority(authority)
			if (index < 8) != (err == nil) {
				t.Fatalf("authority %d error = %v", index+1, err)
			}
		}
	})
	t.Run("ninth provenance bundle", func(t *testing.T) {
		store := openReleaseCanonicalStoresForTest(t)
		for index := 0; index < 9; index++ {
			bundle := signedFloorReleaseGraph(t).Bundle
			bundle.ReleaseArtifactSetDigest = fmt.Sprintf("%064x", index+1)
			resignReleaseObjectForTest(t, &bundle)
			_, err := store.publishReleaseBundle(bundle)
			if (index < 8) != (err == nil) {
				t.Fatalf("bundle %d error = %v", index+1, err)
			}
		}
	})
	t.Run("fifth provenance temporary", func(t *testing.T) {
		counts := releaseCanonicalStoreCountsV1{provenanceTemps: 4}
		if err := validateReleaseCanonicalCapacityV1(releaseAuthorityClassV1, counts, 1); err == nil {
			t.Fatal("fifth provenance temporary accepted")
		}
	})
	t.Run("aggregate plus one", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			class  releaseCanonicalStoreClassV1
			counts releaseCanonicalStoreCountsV1
		}{
			{name: "set reports", class: releaseSetClassV1, counts: releaseCanonicalStoreCountsV1{setReportBytes: 64 << 20}},
			{name: "provenance", class: releaseAuthorityClassV1, counts: releaseCanonicalStoreCountsV1{provenanceBytes: 3 << 20}},
		} {
			t.Run(test.name, func(t *testing.T) {
				if err := validateReleaseCanonicalCapacityV1(test.class, test.counts, 0); err != nil {
					t.Fatalf("exact aggregate cap refused: %v", err)
				}
				if err := validateReleaseCanonicalCapacityV1(test.class, test.counts, 1); err == nil {
					t.Fatal("aggregate cap + 1 accepted")
				}
			})
		}
	})
}

func TestReleaseCanonicalStoresReconcileTypedTempsAndKeepFutureAndGCInactive(t *testing.T) {
	root := newReleaseCanonicalStoreRootForTest(t)
	store, err := openReleaseCanonicalStoresV1(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	set := signedFloorReleaseGraph(t).ArtifactSet
	canonical := mustCanonicalForTest(t, set)
	digest := releaseObjectDigestForTest(t, "cq/release-artifact-set/v1\x00", set)
	validTemp := ".cq-release-artifact-set-v1-" + digest + "-aaaaaaaaaaaaaaaa.tmp"
	badTemp := ".cq-release-artifact-set-v1-" + strings.Repeat("b", 64) + "-bbbbbbbbbbbbbbbb.tmp"
	if err := os.WriteFile(filepath.Join(root, "release-sets", validTemp), canonical, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-sets", badTemp), []byte("not canonical"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.publishReleaseArtifactSet(set); err != nil || got != digest {
		t.Fatalf("publish after temp reconciliation = %q, %v", got, err)
	}
	for _, name := range []string{validTemp, badTemp} {
		if _, err := os.Lstat(filepath.Join(root, "release-sets", name)); !os.IsNotExist(err) {
			t.Fatalf("temporary %q survived reconciliation: %v", name, err)
		}
	}

	for name, operation := range map[string]func() error{
		"floor acceptance":   store.publishRollbackFloorAcceptanceReceipt,
		"outer validation":   store.publishCandidateReleasePromotionReceipt,
		"inner validation":   store.publishRollbackFloorValidationReceipt,
		"reference aware gc": store.garbageCollectReferencedObjects,
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); err == nil || !strings.Contains(err.Error(), "feature inactive") {
				t.Fatalf("operation error = %v, want feature inactive", err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "release-sets", digest+".json")); err != nil {
		t.Fatalf("inactive GC removed current object: %v", err)
	}
}

func newReleaseCanonicalStoreRootForTest(t *testing.T) string {
	t.Helper()
	requireDarwinReleaseFilesystemForTest(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"release-sets", "release-reports", "release-provenance", "release-provenance/authorities", "release-provenance/bundles"} {
		if err := os.Mkdir(filepath.Join(root, relative), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func requireDarwinReleaseFilesystemForTest(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("requires Darwin APFS release filesystem")
	}
}

func openReleaseCanonicalStoresForTest(t *testing.T) *releaseCanonicalStoresV1 {
	t.Helper()
	store, err := openReleaseCanonicalStoresV1(newReleaseCanonicalStoreRootForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	return store
}

func mustCanonicalForTest(t *testing.T, value any) []byte {
	t.Helper()
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestBuildEnvironmentDigestV1MatchesLiteralFraming(t *testing.T) {
	environment := []BuildEnvironmentEntryV1{
		{Key: "CGO_ENABLED", Value: "0"},
		{Key: "GOAMD64", Value: "v1"},
		{Key: "GOARCH", Value: "amd64"},
		{Key: "GOARM", Value: ""},
		{Key: "GOARM64", Value: ""},
		{Key: "GOEXPERIMENT", Value: ""},
		{Key: "GOFLAGS", Value: "-trimpath"},
		{Key: "GOOS", Value: "linux"},
		{Key: "GOTOOLCHAIN", Value: "go1.26.1"},
		{Key: "LC_ALL", Value: "C"},
		{Key: "SOURCE_DATE_EPOCH", Value: "0"},
		{Key: "TZ", Value: "UTC"},
	}
	got, err := BuildEnvironmentDigestV1(environment)
	if err != nil {
		t.Fatal(err)
	}
	const want = "568514f3afdc4789ac1591a7a649c3d19d522727d8b8a4c21c55a16d14b503f8"
	if got != want {
		t.Fatalf("BuildEnvironmentDigestV1() = %s, want %s", got, want)
	}
	for name, mutate := range map[string]func([]BuildEnvironmentEntryV1) []BuildEnvironmentEntryV1{
		"missing": func(entries []BuildEnvironmentEntryV1) []BuildEnvironmentEntryV1 { return entries[:len(entries)-1] },
		"extra": func(entries []BuildEnvironmentEntryV1) []BuildEnvironmentEntryV1 {
			return append(entries, BuildEnvironmentEntryV1{Key: "TOKEN", Value: "forbidden"})
		},
		"reordered": func(entries []BuildEnvironmentEntryV1) []BuildEnvironmentEntryV1 {
			entries[0], entries[1] = entries[1], entries[0]
			return entries
		},
	} {
		t.Run(name, func(t *testing.T) {
			copyEntries := append([]BuildEnvironmentEntryV1(nil), environment...)
			if _, err := BuildEnvironmentDigestV1(mutate(copyEntries)); err == nil {
				t.Fatal("accepted invalid build environment")
			}
		})
	}
}

func TestBuildEnvironmentDigestV1RejectsToolchainAndSecretDrift(t *testing.T) {
	base := []BuildEnvironmentEntryV1{
		{Key: "CGO_ENABLED", Value: "0"}, {Key: "GOAMD64", Value: "v1"},
		{Key: "GOARCH", Value: "amd64"}, {Key: "GOARM", Value: ""},
		{Key: "GOARM64", Value: ""}, {Key: "GOEXPERIMENT", Value: ""},
		{Key: "GOFLAGS", Value: "-trimpath"}, {Key: "GOOS", Value: "linux"},
		{Key: "GOTOOLCHAIN", Value: "go1.26.1"}, {Key: "LC_ALL", Value: "C"},
		{Key: "SOURCE_DATE_EPOCH", Value: "0"}, {Key: "TZ", Value: "UTC"},
	}
	for name, mutate := range map[string]func([]BuildEnvironmentEntryV1){
		"toolchain": func(entries []BuildEnvironmentEntryV1) { entries[8].Value = "go1.26.5" },
		"secret":    func(entries []BuildEnvironmentEntryV1) { entries[6].Value = "-trimpath token=secret" },
		"locale":    func(entries []BuildEnvironmentEntryV1) { entries[9].Value = "en_GB.UTF-8" },
		"timezone":  func(entries []BuildEnvironmentEntryV1) { entries[11].Value = "Europe/London" },
	} {
		t.Run(name, func(t *testing.T) {
			entries := append([]BuildEnvironmentEntryV1(nil), base...)
			mutate(entries)
			if _, err := BuildEnvironmentDigestV1(entries); err == nil {
				t.Fatal("accepted release environment drift")
			}
		})
	}
}

func TestCommandDigestV1RejectsOpenPurposeAndWorkingDirectory(t *testing.T) {
	for name, call := range map[string]func() error{
		"purpose": func() error {
			_, err := CommandDigestV1("arbitrary", ".", []string{"go", "test"})
			return err
		},
		"absolute cwd": func() error {
			_, err := CommandDigestV1("release-target-race", "/repo", []string{"go", "test"})
			return err
		},
		"traversal cwd": func() error {
			_, err := CommandDigestV1("release-target-race", "../repo", []string{"go", "test"})
			return err
		},
		"empty argv": func() error {
			_, err := CommandDigestV1("release-target-race", ".", nil)
			return err
		},
		"nul argv": func() error {
			_, err := CommandDigestV1("release-target-race", ".", []string{"go\x00test"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("accepted open command digest input")
			}
		})
	}
	if _, err := CommandDigestV1("role-worker-build", ".", []string{"go", "build", "./cmd/cq"}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandDigestV1ArgvSubstitutionMatrix(t *testing.T) {
	base := []string{"/opt/homebrew/bin/go", "test", "-race", "-count=1", "./..."}
	want, err := CommandDigestV1("release-target-race", ".", base)
	if err != nil {
		t.Fatal(err)
	}
	for name, argv := range map[string][]string{
		"tool":           {"/tmp/go", "test", "-race", "-count=1", "./..."},
		"subcommand":     {"/opt/homebrew/bin/go", "vet", "-race", "-count=1", "./..."},
		"flag_removed":   {"/opt/homebrew/bin/go", "test", "-count=1", "./..."},
		"flag_reordered": {"/opt/homebrew/bin/go", "test", "-count=1", "-race", "./..."},
		"empty_argument": {"/opt/homebrew/bin/go", "test", "-race", "", "-count=1", "./..."},
		"package":        {"/opt/homebrew/bin/go", "test", "-race", "-count=1", "./internal/proxy"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := CommandDigestV1("release-target-race", ".", argv)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatal("argv substitution preserved command digest")
			}
		})
	}
}

func TestValidateCUReportSetV1EnforcesFloorAndTargetCardinality(t *testing.T) {
	floor := makeCUReports(9)
	if err := ValidateCUReportSetV1("floor", floor); err != nil {
		t.Fatal(err)
	}
	target := makeCUReports(10)
	if err := ValidateCUReportSetV1("target", target); err != nil {
		t.Fatal(err)
	}
	for name, reports := range map[string][]CUReportV1{
		"floor missing":  floor[:8],
		"floor plus one": append(append([]CUReportV1(nil), floor...), CUReportV1{SchemaVersion: 1, CUID: "CU-9", Kind: "construction_unit_report_v1", Outcome: "passed", RaceEnabled: true}),
		"target reordered": func() []CUReportV1 {
			copyReports := append([]CUReportV1(nil), target...)
			copyReports[0], copyReports[1] = copyReports[1], copyReports[0]
			return copyReports
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			purpose := "target"
			if strings.HasPrefix(name, "floor") {
				purpose = "floor"
			}
			if err := ValidateCUReportSetV1(purpose, reports); err == nil {
				t.Fatal("accepted invalid CU report set")
			}
		})
	}
}

func TestValidateReleaseBundleEntriesV1EnforcesExactTreeCardinality(t *testing.T) {
	floor := makeReleaseBundleEntries("floor")
	if len(floor) != 10 {
		t.Fatalf("floor entries = %d", len(floor))
	}
	if err := ValidateReleaseBundleEntriesV1("floor", floor); err != nil {
		t.Fatal(err)
	}
	target := makeReleaseBundleEntries("target")
	if len(target) != 13 {
		t.Fatalf("target entries = %d", len(target))
	}
	if err := ValidateReleaseBundleEntriesV1("target", target); err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string][]ReleaseBundleEntryV1{
		"missing":   target[:12],
		"plus one":  append(append([]ReleaseBundleEntryV1(nil), target...), ReleaseBundleEntryV1{RelativePath: "bundle.json", Kind: "file", Digest: strings.Repeat("1", 64), Size: 1}),
		"directory": append(append([]ReleaseBundleEntryV1(nil), target[:12]...), ReleaseBundleEntryV1{RelativePath: "reports", Kind: "file", Digest: strings.Repeat("1", 64), Size: 1}),
		"reordered": func() []ReleaseBundleEntryV1 {
			copyEntries := append([]ReleaseBundleEntryV1(nil), target...)
			copyEntries[0], copyEntries[1] = copyEntries[1], copyEntries[0]
			return copyEntries
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateReleaseBundleEntriesV1("target", entries); err == nil {
				t.Fatal("accepted invalid release bundle entries")
			}
		})
	}
}

func TestVerifyReleaseGraphV1RejectsIncompleteGraph(t *testing.T) {
	if err := VerifyReleaseGraphAgainstApprovedAuthorityV1(ReleaseGraphV1{}, ReleaseAuthorityPinV1{}, ReleaseVerificationEvidenceV1{}); err == nil {
		t.Fatal("accepted an incomplete release graph")
	}
}

func TestVerifyReleaseGraphV1RejectsUnavailableConstructionUnits(t *testing.T) {
	graph := signedFloorReleaseGraph(t)
	err := VerifyReleaseGraphAgainstApprovedAuthorityV1(graph, releaseAuthorityPinForTest(t, graph), releaseEvidenceForTest(t, graph))
	if err == nil || !strings.Contains(err.Error(), "feature inactive") || !strings.Contains(err.Error(), "CU-3") {
		t.Fatalf("release verification error = %v, want feature-inactive CU-3", err)
	}
}

func TestReleaseReachabilityCatalogueRemainsInactiveUntilCU2Regeneration(t *testing.T) {
	err := verifyAvailableReleaseReachabilityCatalogueV1()
	if err == nil || !strings.Contains(err.Error(), "feature inactive") || !strings.Contains(err.Error(), "CU-2") {
		t.Fatalf("reachability availability = %v, want feature-inactive CU-2", err)
	}
}

func TestReleaseObjectDigestV1UsesPerTypeCaps(t *testing.T) {
	large := strings.Repeat("x", 70<<10)
	if _, err := releaseObjectDigestV1("cq/release-artifact-set/v1\x00", ReleaseArtifactSetV1{SupportedFeatures: []string{large}}); err != nil {
		t.Fatalf("1 MiB artifact-set cap rejected 70 KiB object: %v", err)
	}
	if _, err := releaseObjectDigestV1("cq/source-ancestry-receipt/v1\x00", SourceAncestryReceiptV1{Kind: large}); err != nil {
		t.Fatalf("1 MiB ancestry cap rejected 70 KiB object: %v", err)
	}
	if _, err := releaseObjectDigestV1("cq/release-build-report/v1\x00", ReleaseBuildReportV1{Kind: large}); err == nil {
		t.Fatal("64 KiB build-report cap accepted 70 KiB object")
	}
}

func TestReleaseRoleEvidenceSizeCaps(t *testing.T) {
	for _, fixture := range []struct {
		name                    string
		payload, signature, ABI int
		wantError               bool
	}{
		{name: "exact payload", payload: 268435456},
		{name: "payload plus one", payload: 268435457, wantError: true},
		{name: "exact signature", signature: 1 << 20},
		{name: "signature plus one", signature: 1<<20 + 1, wantError: true},
		{name: "exact ABI", ABI: 1 << 20},
		{name: "ABI plus one", ABI: 1<<20 + 1, wantError: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			err := validateReleaseRoleEvidenceSizesV1(fixture.payload, fixture.signature, fixture.ABI, fixture.ABI)
			if (err != nil) != fixture.wantError {
				t.Fatalf("size validation error = %v, wantError %t", err, fixture.wantError)
			}
		})
	}
}

func TestReleaseDescendantOpenFlagsAreNonblocking(t *testing.T) {
	if releaseDescendantOpenFlagsV1()&unix.O_NONBLOCK == 0 {
		t.Fatal("release descendant open can block on an untrusted FIFO or device")
	}
}

func TestVerifyReleaseBuildAuthorityV1RejectsSelfAuthorisedBundle(t *testing.T) {
	graph := signedFloorReleaseGraph(t)
	pin := releaseAuthorityPinForTest(t, graph)
	if err := VerifyReleaseBuildAuthorityV1(graph.Authority, pin); err != nil {
		t.Fatal(err)
	}
	for name, changed := range map[string]ReleaseAuthorityPinV1{
		"digest": {Digest: strings.Repeat("9", 64), Ed25519PublicKey: pin.Ed25519PublicKey},
		"key":    {Digest: pin.Digest, Ed25519PublicKey: strings.Repeat("8", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := VerifyReleaseBuildAuthorityV1(graph.Authority, changed); err == nil {
				t.Fatal("accepted bundle authority not named by external trust")
			}
		})
	}
}

func TestVerifyPairedReleaseGraphsV1RemainsInactiveBeforeCU9(t *testing.T) {
	floor := signedFloorReleaseGraph(t)
	target := signedTargetReleaseGraph(t)
	pin := ReleaseAuthorityPinV1{
		Digest:           releaseObjectDigestForTest(t, "cq/release-build-authority/v1\x00", floor.Authority),
		Ed25519PublicKey: floor.Authority.Ed25519PublicKey,
	}
	if err := VerifyPairedReleaseGraphsAgainstApprovedAuthorityV1(floor, target, pin, releaseEvidenceForTest(t, floor), releaseEvidenceForTest(t, target)); err == nil || !strings.Contains(err.Error(), "feature inactive") {
		t.Fatalf("paired release verification = %v, want feature inactive", err)
	}
}

func TestVerifyReleaseGraphStructureV1AcceptsSignedFloorAndRejectsSubstitution(t *testing.T) {
	graph := signedFloorReleaseGraph(t)
	evidence := releaseEvidenceForTest(t, graph)
	if err := verifyReleaseGraphStructureAgainstApprovedAuthorityV1(graph, releaseAuthorityPinForTest(t, graph), evidence); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ReleaseGraphV1){
		"role payload": func(graph *ReleaseGraphV1) {
			graph.ArtifactSet.Roles[0].ArtifactPayloadDigest = strings.Repeat("f", 64)
		},
		"report source": func(graph *ReleaseGraphV1) {
			graph.RaceReport.SourceCommit = strings.Repeat("f", 40)
		},
		"signature": func(graph *ReleaseGraphV1) {
			graph.CUReportSet.Signature = strings.Repeat("0", 128)
		},
		"bundle digest": func(graph *ReleaseGraphV1) {
			graph.Bundle.ReleaseArtifactSetDigest = strings.Repeat("f", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := graph
			changed.ArtifactSet.Roles = append([]ReleaseArtifactRoleV1(nil), graph.ArtifactSet.Roles...)
			mutate(&changed)
			if err := verifyReleaseGraphStructureAgainstApprovedAuthorityV1(changed, releaseAuthorityPinForTest(t, graph), evidence); err == nil {
				t.Fatal("accepted substituted release graph")
			}
		})
	}
}

func TestVerifyReleaseGraphStructureV1AcceptsSignedTargetAndRejectsAncestry(t *testing.T) {
	graph := signedTargetReleaseGraph(t)
	evidence := releaseEvidenceForTest(t, graph)
	if err := verifyReleaseGraphStructureAgainstApprovedAuthorityV1(graph, releaseAuthorityPinForTest(t, graph), evidence); err != nil {
		t.Fatal(err)
	}
	for name, fixture := range map[string]struct {
		mutate func(*ReleaseGraphV1)
		resign func(*testing.T, *ReleaseGraphV1)
	}{
		"same source": {mutate: func(graph *ReleaseGraphV1) {
			graph.Ancestry.TargetSourceCommit = graph.Ancestry.FloorSourceCommit
		}, resign: resignTargetAncestryChain},
		"wrong merge base": {mutate: func(graph *ReleaseGraphV1) {
			graph.Ancestry.MergeBaseCommit = graph.Ancestry.TargetSourceCommit
		}, resign: resignTargetAncestryChain},
		"ancestry signature": {mutate: func(graph *ReleaseGraphV1) {
			graph.Ancestry.Signature = strings.Repeat("0", 128)
		}},
		"missing ancestry": {mutate: func(graph *ReleaseGraphV1) {
			graph.Ancestry = nil
		}},
		"role swap": {mutate: func(graph *ReleaseGraphV1) {
			graph.ArtifactSet.Roles[0], graph.ArtifactSet.Roles[1] = graph.ArtifactSet.Roles[1], graph.ArtifactSet.Roles[0]
		}, resign: resignTargetSetChain},
		"CU missing": {mutate: func(graph *ReleaseGraphV1) {
			graph.CUReportSet.Reports = graph.CUReportSet.Reports[:9]
		}, resign: resignTargetCUChain},
		"ABI substitution": {mutate: func(graph *ReleaseGraphV1) {
			replacement := strings.Repeat("f", 64)
			graph.ArtifactManifests[1].LauncherABIDigest = &replacement
		}},
	} {
		t.Run(name, func(t *testing.T) {
			changed := graph
			ancestry := *graph.Ancestry
			changed.Ancestry = &ancestry
			changed.ArtifactSet.Roles = append([]ReleaseArtifactRoleV1(nil), graph.ArtifactSet.Roles...)
			changed.ArtifactManifests = append([]ReleaseArtifactManifestV1(nil), graph.ArtifactManifests...)
			changed.CUReportSet.Reports = append([]CUReportV1(nil), graph.CUReportSet.Reports...)
			fixture.mutate(&changed)
			if fixture.resign != nil {
				fixture.resign(t, &changed)
			}
			if err := verifyReleaseGraphStructureAgainstApprovedAuthorityV1(changed, releaseAuthorityPinForTest(t, graph), evidence); err == nil {
				t.Fatal("accepted invalid target release graph")
			}
		})
	}
}

func TestReleaseAncestryEvidenceMustReturnSignedMergeBase(t *testing.T) {
	graph := signedTargetReleaseGraph(t)
	evidence := releaseEvidenceForTest(t, graph)
	evidence.Ancestry.Stdout = []byte("not-the-signed-merge-base\n")
	if err := verifyReleaseGraphStructureAgainstApprovedAuthorityV1(graph, releaseAuthorityPinForTest(t, graph), evidence); err == nil {
		t.Fatal("accepted ancestry execution whose stdout did not equal signed merge base")
	}
}

func TestReleaseBuildArgvV1RejectsPlaceholderCommands(t *testing.T) {
	tool := "/approved/go/bin/go"
	for _, fixture := range []struct {
		kind string
		argv []string
		ok   bool
	}{
		{kind: "build", argv: []string{tool, "build", "./..."}, ok: true},
		{kind: "vet", argv: []string{tool, "vet", "./..."}, ok: true},
		{kind: "race", argv: []string{tool, "test", "-race", "-count=1", "./..."}, ok: true},
		{kind: "race", argv: []string{tool, "race", "./..."}},
		{kind: "race", argv: []string{tool, "test", "./..."}},
		{kind: "build", argv: []string{"go", "build", "./..."}},
	} {
		err := validateReleaseBuildArgvV1(fixture.kind, fixture.argv)
		if (err == nil) != fixture.ok {
			t.Fatalf("validate %s %v = %v, want ok %t", fixture.kind, fixture.argv, err, fixture.ok)
		}
	}
}

func TestReleaseGraphStructureRejectsResignedFictionalEntrySize(t *testing.T) {
	graph := signedFloorReleaseGraph(t)
	graph.Bundle.Entries[0].Size++
	graph.Bundle.BundleSignature = ""
	graph.Bundle.BundleSignature = signReleaseObjectForTest(t, graph.Bundle, fixedReleasePrivateKey())
	if err := verifyReleaseGraphStructureAgainstApprovedAuthorityV1(graph, releaseAuthorityPinForTest(t, graph), releaseEvidenceForTest(t, graph)); err == nil {
		t.Fatal("accepted re-signed bundle with fictional raw entry size")
	}
}

func TestVerifyReleaseGraphStructureV1RejectsRetainedEvidenceSubstitution(t *testing.T) {
	floor := signedFloorReleaseGraph(t)
	for name, mutate := range map[string]func(*ReleaseVerificationEvidenceV1){
		"repository": func(value *ReleaseVerificationEvidenceV1) {
			value.RepositoryRemote = []byte("https://attacker.invalid/cq.git\n")
		},
		"source commit": func(value *ReleaseVerificationEvidenceV1) { value.SourceCommit = strings.Repeat("9", 40) },
		"source tree": func(value *ReleaseVerificationEvidenceV1) {
			value.SourceTreeListing = append(value.SourceTreeListing, 0)
		},
		"environment": func(value *ReleaseVerificationEvidenceV1) { value.BuildEnvironment[0].Value = "0" },
		"build argv":  func(value *ReleaseVerificationEvidenceV1) { value.BuildReports[0].Argv[1] = "vet" },
		"build result": func(value *ReleaseVerificationEvidenceV1) {
			value.BuildReports[0].Stdout = append(value.BuildReports[0].Stdout, '!')
		},
		"CU manifest": func(value *ReleaseVerificationEvidenceV1) {
			value.ConstructionUnits[0].ManifestBytes = append(value.ConstructionUnits[0].ManifestBytes, '\n')
		},
		"CU invocation": func(value *ReleaseVerificationEvidenceV1) { value.ConstructionUnits[0].Command.Argv[1] = "CU-1" },
		"CU result": func(value *ReleaseVerificationEvidenceV1) {
			value.ConstructionUnits[0].Command.Stderr = []byte("substituted")
		},
		"role payload":      func(value *ReleaseVerificationEvidenceV1) { value.Roles[0].Payload = []byte("substituted") },
		"role signature":    func(value *ReleaseVerificationEvidenceV1) { value.Roles[0].CodeSignature = []byte("substituted") },
		"role ABI":          func(value *ReleaseVerificationEvidenceV1) { value.Roles[0].LauncherABIBytes = []byte("substituted") },
		"build cardinality": func(value *ReleaseVerificationEvidenceV1) { value.BuildReports = value.BuildReports[:2] },
		"CU cardinality":    func(value *ReleaseVerificationEvidenceV1) { value.ConstructionUnits = value.ConstructionUnits[:8] },
		"role cardinality":  func(value *ReleaseVerificationEvidenceV1) { value.Roles = value.Roles[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			evidence := releaseEvidenceForTest(t, floor)
			mutate(&evidence)
			if err := verifyReleaseGraphStructureAgainstApprovedAuthorityV1(floor, releaseAuthorityPinForTest(t, floor), evidence); err == nil {
				t.Fatal("accepted substituted retained evidence")
			}
		})
	}
	target := signedTargetReleaseGraph(t)
	evidence := releaseEvidenceForTest(t, target)
	evidence.Ancestry.Argv[1] = "rev-parse"
	if err := verifyReleaseGraphStructureAgainstApprovedAuthorityV1(target, releaseAuthorityPinForTest(t, target), evidence); err == nil {
		t.Fatal("accepted substituted retained ancestry command")
	}
}

func resignTargetAncestryChain(t *testing.T, graph *ReleaseGraphV1) {
	t.Helper()
	privateKey := fixedReleasePrivateKey()
	graph.Ancestry.Signature = ""
	graph.Ancestry.Signature = signReleaseObjectForTest(t, *graph.Ancestry, privateKey)
	ancestryDigest := releaseObjectDigestForTest(t, "cq/source-ancestry-receipt/v1\x00", *graph.Ancestry)
	graph.ArtifactSet.SourceAncestryReceiptDigest = &ancestryDigest
	graph.Bundle.SourceAncestryReceiptDigest = &ancestryDigest
	resignTargetSetChain(t, graph)
}

func resignTargetCUChain(t *testing.T, graph *ReleaseGraphV1) {
	t.Helper()
	privateKey := fixedReleasePrivateKey()
	graph.CUReportSet.Signature = ""
	graph.CUReportSet.Signature = signReleaseObjectForTest(t, graph.CUReportSet, privateKey)
	cuDigest := releaseObjectDigestForTest(t, "cq/construction-unit-report-set/v1\x00", graph.CUReportSet)
	graph.ArtifactSet.ConstructionUnitReportSetDigest = cuDigest
	graph.Bundle.ConstructionUnitReportSetDigest = cuDigest
	resignTargetSetChain(t, graph)
}

func resignTargetSetChain(t *testing.T, graph *ReleaseGraphV1) {
	t.Helper()
	privateKey := fixedReleasePrivateKey()
	graph.ArtifactSet.SetSignature = ""
	graph.ArtifactSet.SetSignature = signReleaseObjectForTest(t, graph.ArtifactSet, privateKey)
	setDigest := releaseObjectDigestForTest(t, "cq/release-artifact-set/v1\x00", graph.ArtifactSet)
	graph.Bundle.ReleaseArtifactSetDigest = setDigest
	graph.Bundle.BundleSignature = ""
	graph.Bundle.BundleSignature = signReleaseObjectForTest(t, graph.Bundle, privateKey)
}

func TestVerifyReleaseBundleDirectoryV1RecomputesPhysicalFiles(t *testing.T) {
	requireDarwinReleaseFilesystemForTest(t)
	graph := signedFloorReleaseGraph(t)
	t.Run("exact floor", func(t *testing.T) {
		if err := verifyReleaseBundleDirectoryStructureV1(materialiseFloorReleaseBundle(t, graph), graph); err != nil {
			t.Fatal(err)
		}
	})
	for name, mutate := range map[string]func(*testing.T, string){
		"payload substitution": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "payloads", "worker"), []byte("substituted"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"manifest substitution": func(t *testing.T, root string) {
			path := filepath.Join(root, "manifests", "worker.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"missing file": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "reports", "vet.json")); err != nil {
				t.Fatal(err)
			}
		},
		"unknown file": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "unknown"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, root string) {
			if err := os.Symlink("worker", filepath.Join(root, "payloads", "link")); err != nil {
				t.Fatal(err)
			}
		},
		"nested directory": func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "payloads", "nested"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"group-readable file": func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, "payloads", "worker"), 0o640); err != nil {
				t.Fatal(err)
			}
		},
		"group-searchable directory": func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, "payloads"), 0o750); err != nil {
				t.Fatal(err)
			}
		},
		"hard-linked file": func(t *testing.T, root string) {
			if err := os.Link(filepath.Join(root, "payloads", "worker"), filepath.Join(t.TempDir(), "held-worker")); err != nil {
				t.Fatal(err)
			}
		},
		"FIFO": func(t *testing.T, root string) {
			if err := unix.Mkfifo(filepath.Join(root, "payloads", "pipe"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"socket": func(t *testing.T, root string) {
			shortRoot, err := os.MkdirTemp("/tmp", "cq-socket-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(shortRoot) })
			alias := filepath.Join(shortRoot, "r")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(alias, "payloads", "socket"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := materialiseFloorReleaseBundle(t, graph)
			mutate(t, root)
			if err := verifyReleaseBundleDirectoryStructureV1(root, graph); err == nil {
				t.Fatal("accepted substituted physical bundle")
			}
		})
	}
}

func signedFloorReleaseGraph(t *testing.T) ReleaseGraphV1 {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = 7
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := hex.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	digest := func(value byte) string { return strings.Repeat(string(value), 64) }
	commit := strings.Repeat("a", 40)
	tree := digest('b')
	environmentDigest := digest('c')
	reachabilityDigest := digest('d')
	launcherABI := digest('e')
	privateABI := digest('1')
	authority := ReleaseBuildAuthorityV1{
		SchemaVersion: 1, AuthorityID: "authority-1", Ed25519PublicKey: publicKey,
		RepositoryIdentityDigest: digest('2'), BlueprintSHA256: frozenBlueprintSHA256,
		ReviewAttestationAggregateSHA256: frozenReviewAggregateSHA256,
		ReviewAuthorityBaselineCommit:    frozenReviewBaseline, LineageRootCommit: commit,
		LineageRootTreeDigest: tree, ToolchainIdentity: "go1.26.1 darwin/arm64",
		CreatedAt: "2026-08-17T10:00:00Z",
	}
	authorityDigest := releaseObjectDigestForTest(t, "cq/release-build-authority/v1\x00", authority)
	manifests := []ReleaseArtifactManifestV1{
		{SchemaVersion: 1, Role: "supervisor", ReleaseBuildAuthorityDigest: authorityDigest, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildCommandDigest: digest('3'), BuildEnvironmentDigest: environmentDigest, Architecture: "darwin/arm64", BuildID: "supervisor-1", SupportedFeatures: []string{"proxy_v1"}, MinimumFloorFeatures: []string{"proxy_v1"}, LauncherABIDigest: &launcherABI, PrivateABIDigest: &privateABI, CodeSignatureDigest: digest('4'), ArtifactPayloadDigest: sha256HexForTest([]byte("supervisor"))},
		{SchemaVersion: 1, Role: "worker", ReleaseBuildAuthorityDigest: authorityDigest, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildCommandDigest: digest('6'), BuildEnvironmentDigest: environmentDigest, Architecture: "darwin/arm64", BuildID: "worker-1", SupportedFeatures: []string{"proxy_v1"}, MinimumFloorFeatures: []string{"proxy_v1"}, PrivateABIDigest: &privateABI, CodeSignatureDigest: digest('7'), ArtifactPayloadDigest: sha256HexForTest([]byte("worker"))},
	}
	manifestDigests := []string{
		releaseObjectDigestForTest(t, "cq/release-artifact-manifest/v1\x00", manifests[0]),
		releaseObjectDigestForTest(t, "cq/release-artifact-manifest/v1\x00", manifests[1]),
	}
	roles := []ReleaseArtifactRoleV1{
		{Role: "supervisor", ArtifactPayloadDigest: manifests[0].ArtifactPayloadDigest, ArtifactManifestDigest: manifestDigests[0]},
		{Role: "worker", ArtifactPayloadDigest: manifests[1].ArtifactPayloadDigest, ArtifactManifestDigest: manifestDigests[1]},
	}
	build := ReleaseBuildReportV1{SchemaVersion: 1, Kind: "build", Purpose: "floor", ReleaseBuildAuthorityDigest: authorityDigest, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildEnvironmentDigest: environmentDigest, CommandDigest: digest('9'), Outcome: "passed", ExitCode: 0, RaceEnabled: false, ExecutionResultDigest: digest('a'), StartedAt: "2026-08-17T10:00:01Z", EndedAt: "2026-08-17T10:00:02Z", SignerPublicKey: publicKey}
	for index, role := range roles {
		build.RoleExecutions = append(build.RoleExecutions, ReleaseRoleExecutionV1{Role: role.Role, BuildCommandDigest: manifests[index].BuildCommandDigest, ArtifactPayloadDigest: role.ArtifactPayloadDigest, ArtifactManifestDigest: role.ArtifactManifestDigest})
	}
	build.Signature = signReleaseObjectForTest(t, build, privateKey)
	vet := build
	vet.Kind, vet.CommandDigest, vet.ExecutionResultDigest, vet.RoleExecutions, vet.StartedAt, vet.EndedAt, vet.Signature = "vet", digest('b'), digest('c'), []ReleaseRoleExecutionV1{}, "2026-08-17T10:00:03Z", "2026-08-17T10:00:04Z", ""
	vet.Signature = signReleaseObjectForTest(t, vet, privateKey)
	race := build
	race.Kind, race.CommandDigest, race.ExecutionResultDigest, race.RoleExecutions, race.RaceEnabled, race.StartedAt, race.EndedAt, race.Signature = "race", digest('d'), digest('e'), []ReleaseRoleExecutionV1{}, true, "2026-08-17T10:00:05Z", "2026-08-17T10:00:06Z", ""
	race.Signature = signReleaseObjectForTest(t, race, privateKey)
	reports := makeCUReports(9)
	for index := range reports {
		reports[index].VerificationManifestDigest = digest('1')
		reports[index].InvocationDigest = digest('2')
		reports[index].ExecutionResultDigest = digest('3')
		reports[index].StartedAt = "2026-08-17T10:00:07Z"
		reports[index].EndedAt = "2026-08-17T10:00:08Z"
	}
	cuSet := ConstructionUnitReportSetV1{SchemaVersion: 1, Kind: "construction_unit_report_set_v1", Purpose: "floor", ReleaseBuildAuthorityDigest: authorityDigest, BlueprintSHA256: frozenBlueprintSHA256, ReviewAttestationAggregateSHA256: frozenReviewAggregateSHA256, ReviewAuthorityBaselineCommit: frozenReviewBaseline, LegacyAtomicWriterReachabilityCatalogueDigest: reachabilityDigest, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildEnvironmentDigest: environmentDigest, Reports: reports, SignerPublicKey: publicKey}
	cuSet.Signature = signReleaseObjectForTest(t, cuSet, privateKey)
	buildDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", build)
	vetDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", vet)
	raceDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", race)
	cuSetDigest := releaseObjectDigestForTest(t, "cq/construction-unit-report-set/v1\x00", cuSet)
	artifactSet := ReleaseArtifactSetV1{SchemaVersion: 1, Purpose: "floor", ReleaseBuildAuthorityDigest: authorityDigest, SignerPublicKey: publicKey, SourceCommit: commit, SourceTreeDigest: tree, ToolchainIdentity: authority.ToolchainIdentity, BuildEnvironmentDigest: environmentDigest, BuildReportDigest: buildDigest, VetReportDigest: vetDigest, RaceTestReportDigest: raceDigest, ConstructionUnitReportSetDigest: cuSetDigest, LegacyAtomicWriterReachabilityCatalogueDigest: reachabilityDigest, RequiredLauncherABIDigest: &launcherABI, Roles: roles, SupportedFeatures: []string{"proxy_v1"}, MinimumFloorFeatures: []string{"proxy_v1"}}
	artifactSet.SetSignature = signReleaseObjectForTest(t, artifactSet, privateKey)
	artifactSetDigest := releaseObjectDigestForTest(t, "cq/release-artifact-set/v1\x00", artifactSet)
	entries := []ReleaseBundleEntryV1{
		{RelativePath: "manifests/supervisor.json", Kind: "file", Digest: manifestDigests[0], Size: canonicalSizeForTest(t, manifests[0])},
		{RelativePath: "manifests/worker.json", Kind: "file", Digest: manifestDigests[1], Size: canonicalSizeForTest(t, manifests[1])},
		{RelativePath: "payloads/supervisor", Kind: "file", Digest: manifests[0].ArtifactPayloadDigest, Size: uint64(len("supervisor"))},
		{RelativePath: "payloads/worker", Kind: "file", Digest: manifests[1].ArtifactPayloadDigest, Size: uint64(len("worker"))},
		{RelativePath: "release-artifact-set.json", Kind: "file", Digest: artifactSetDigest, Size: canonicalSizeForTest(t, artifactSet)},
		{RelativePath: "release-build-authority.json", Kind: "file", Digest: authorityDigest, Size: canonicalSizeForTest(t, authority)},
		{RelativePath: "reports/build.json", Kind: "file", Digest: buildDigest, Size: canonicalSizeForTest(t, build)},
		{RelativePath: "reports/construction-units.json", Kind: "file", Digest: cuSetDigest, Size: canonicalSizeForTest(t, cuSet)},
		{RelativePath: "reports/race.json", Kind: "file", Digest: raceDigest, Size: canonicalSizeForTest(t, race)},
		{RelativePath: "reports/vet.json", Kind: "file", Digest: vetDigest, Size: canonicalSizeForTest(t, vet)},
	}
	bundle := ReleaseBundleV1{SchemaVersion: 1, Purpose: "floor", ReleaseBuildAuthorityDigest: authorityDigest, ReleaseArtifactSetDigest: artifactSetDigest, ConstructionUnitReportSetDigest: cuSetDigest, Entries: entries, SignerPublicKey: publicKey}
	bundle.BundleSignature = signReleaseObjectForTest(t, bundle, privateKey)
	graph := ReleaseGraphV1{Authority: authority, BuildReport: build, VetReport: vet, RaceReport: race, CUReportSet: cuSet, ArtifactManifests: manifests, ArtifactSet: artifactSet, Bundle: bundle}
	closeReleaseFixtureGraph(t, &graph)
	return graph
}

func signedTargetReleaseGraph(t *testing.T) ReleaseGraphV1 {
	t.Helper()
	floor := signedFloorReleaseGraph(t)
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = 7
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	digest := func(value byte) string { return strings.Repeat(string(value), 64) }
	targetCommit := strings.Repeat("f", 40)
	targetTree := digest('9')
	launcherABI := digest('e')
	privateABI := digest('1')
	authorityDigest := releaseObjectDigestForTest(t, "cq/release-build-authority/v1\x00", floor.Authority)
	launcher := ReleaseArtifactManifestV1{
		SchemaVersion: 1, Role: "launcher", ReleaseBuildAuthorityDigest: authorityDigest,
		SourceCommit: targetCommit, SourceTreeDigest: targetTree, ToolchainIdentity: floor.Authority.ToolchainIdentity,
		BuildCommandDigest: digest('8'), BuildEnvironmentDigest: floor.ArtifactSet.BuildEnvironmentDigest,
		Architecture: "darwin/arm64", BuildID: "launcher-1", SupportedFeatures: []string{"proxy_v1"},
		MinimumFloorFeatures: []string{"proxy_v1"}, LauncherABIDigest: &launcherABI,
		CodeSignatureDigest: digest('5'), ArtifactPayloadDigest: sha256HexForTest([]byte("launcher")),
	}
	manifests := []ReleaseArtifactManifestV1{launcher, floor.ArtifactManifests[0], floor.ArtifactManifests[1]}
	for index := 1; index < len(manifests); index++ {
		manifests[index].SourceCommit = targetCommit
		manifests[index].SourceTreeDigest = targetTree
		manifests[index].BuildID = "target-" + manifests[index].Role
		if manifests[index].Role == "supervisor" {
			manifests[index].LauncherABIDigest = &launcherABI
			manifests[index].PrivateABIDigest = &privateABI
		}
	}
	manifestDigests := make([]string, len(manifests))
	roles := make([]ReleaseArtifactRoleV1, len(manifests))
	for index := range manifests {
		manifestDigests[index] = releaseObjectDigestForTest(t, "cq/release-artifact-manifest/v1\x00", manifests[index])
		roles[index] = ReleaseArtifactRoleV1{Role: manifests[index].Role, ArtifactPayloadDigest: manifests[index].ArtifactPayloadDigest, ArtifactManifestDigest: manifestDigests[index]}
	}

	build := floor.BuildReport
	build.Purpose, build.SourceCommit, build.SourceTreeDigest = "target", targetCommit, targetTree
	build.CommandDigest, build.ExecutionResultDigest, build.Signature = digest('4'), digest('5'), ""
	build.RoleExecutions = make([]ReleaseRoleExecutionV1, len(roles))
	for index := range roles {
		build.RoleExecutions[index] = ReleaseRoleExecutionV1{Role: roles[index].Role, BuildCommandDigest: manifests[index].BuildCommandDigest, ArtifactPayloadDigest: roles[index].ArtifactPayloadDigest, ArtifactManifestDigest: roles[index].ArtifactManifestDigest}
	}
	build.Signature = signReleaseObjectForTest(t, build, privateKey)
	vet := floor.VetReport
	vet.Purpose, vet.SourceCommit, vet.SourceTreeDigest, vet.Signature = "target", targetCommit, targetTree, ""
	vet.CommandDigest, vet.ExecutionResultDigest = digest('6'), digest('7')
	vet.Signature = signReleaseObjectForTest(t, vet, privateKey)
	race := floor.RaceReport
	race.Purpose, race.SourceCommit, race.SourceTreeDigest, race.Signature = "target", targetCommit, targetTree, ""
	race.CommandDigest, race.ExecutionResultDigest = digest('8'), digest('9')
	race.Signature = signReleaseObjectForTest(t, race, privateKey)

	cuSet := floor.CUReportSet
	cuSet.Purpose, cuSet.SourceCommit, cuSet.SourceTreeDigest, cuSet.Signature = "target", targetCommit, targetTree, ""
	cuSet.Reports = makeCUReports(10)
	for index := range cuSet.Reports {
		cuSet.Reports[index].VerificationManifestDigest = digest('1')
		cuSet.Reports[index].InvocationDigest = digest('2')
		cuSet.Reports[index].ExecutionResultDigest = digest('3')
		cuSet.Reports[index].StartedAt = "2026-08-17T10:00:07Z"
		cuSet.Reports[index].EndedAt = "2026-08-17T10:00:08Z"
	}
	cuSet.Signature = signReleaseObjectForTest(t, cuSet, privateKey)

	ancestry := SourceAncestryReceiptV1{
		SchemaVersion: 1, Kind: "source_ancestry_v1", ReleaseBuildAuthorityDigest: authorityDigest,
		RepositoryIdentityDigest: floor.Authority.RepositoryIdentityDigest,
		FloorSourceCommit:        floor.Authority.LineageRootCommit, FloorSourceTreeDigest: floor.Authority.LineageRootTreeDigest,
		TargetSourceCommit: targetCommit, TargetSourceTreeDigest: targetTree, MergeBaseCommit: floor.Authority.LineageRootCommit,
		VerificationCommandDigest: digest('a'), VerifiedAt: "2026-08-17T10:00:09Z",
		SignerPublicKey: floor.Authority.Ed25519PublicKey,
	}
	ancestry.Signature = signReleaseObjectForTest(t, ancestry, privateKey)
	ancestryDigest := releaseObjectDigestForTest(t, "cq/source-ancestry-receipt/v1\x00", ancestry)
	buildDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", build)
	vetDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", vet)
	raceDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", race)
	cuSetDigest := releaseObjectDigestForTest(t, "cq/construction-unit-report-set/v1\x00", cuSet)
	artifactSet := ReleaseArtifactSetV1{
		SchemaVersion: 1, Purpose: "target", ReleaseBuildAuthorityDigest: authorityDigest,
		SignerPublicKey: floor.Authority.Ed25519PublicKey, SourceCommit: targetCommit, SourceTreeDigest: targetTree,
		ToolchainIdentity: floor.Authority.ToolchainIdentity, BuildEnvironmentDigest: floor.ArtifactSet.BuildEnvironmentDigest,
		BuildReportDigest: buildDigest, VetReportDigest: vetDigest, RaceTestReportDigest: raceDigest,
		ConstructionUnitReportSetDigest:               cuSetDigest,
		LegacyAtomicWriterReachabilityCatalogueDigest: floor.ArtifactSet.LegacyAtomicWriterReachabilityCatalogueDigest,
		SourceAncestryReceiptDigest:                   &ancestryDigest, LauncherABIDigest: &launcherABI, Roles: roles,
		SupportedFeatures: []string{"proxy_v1"}, MinimumFloorFeatures: []string{"proxy_v1"},
	}
	artifactSet.SetSignature = signReleaseObjectForTest(t, artifactSet, privateKey)
	artifactSetDigest := releaseObjectDigestForTest(t, "cq/release-artifact-set/v1\x00", artifactSet)
	entries := []ReleaseBundleEntryV1{
		{RelativePath: "manifests/launcher.json", Kind: "file", Digest: manifestDigests[0], Size: canonicalSizeForTest(t, manifests[0])},
		{RelativePath: "manifests/supervisor.json", Kind: "file", Digest: manifestDigests[1], Size: canonicalSizeForTest(t, manifests[1])},
		{RelativePath: "manifests/worker.json", Kind: "file", Digest: manifestDigests[2], Size: canonicalSizeForTest(t, manifests[2])},
		{RelativePath: "payloads/launcher", Kind: "file", Digest: manifests[0].ArtifactPayloadDigest, Size: uint64(len("launcher"))},
		{RelativePath: "payloads/supervisor", Kind: "file", Digest: manifests[1].ArtifactPayloadDigest, Size: uint64(len("supervisor"))},
		{RelativePath: "payloads/worker", Kind: "file", Digest: manifests[2].ArtifactPayloadDigest, Size: uint64(len("worker"))},
		{RelativePath: "release-artifact-set.json", Kind: "file", Digest: artifactSetDigest, Size: canonicalSizeForTest(t, artifactSet)},
		{RelativePath: "release-build-authority.json", Kind: "file", Digest: authorityDigest, Size: canonicalSizeForTest(t, floor.Authority)},
		{RelativePath: "reports/build.json", Kind: "file", Digest: buildDigest, Size: canonicalSizeForTest(t, build)},
		{RelativePath: "reports/construction-units.json", Kind: "file", Digest: cuSetDigest, Size: canonicalSizeForTest(t, cuSet)},
		{RelativePath: "reports/race.json", Kind: "file", Digest: raceDigest, Size: canonicalSizeForTest(t, race)},
		{RelativePath: "reports/vet.json", Kind: "file", Digest: vetDigest, Size: canonicalSizeForTest(t, vet)},
		{RelativePath: "source-ancestry.json", Kind: "file", Digest: ancestryDigest, Size: canonicalSizeForTest(t, ancestry)},
	}
	bundle := ReleaseBundleV1{SchemaVersion: 1, Purpose: "target", ReleaseBuildAuthorityDigest: authorityDigest, ReleaseArtifactSetDigest: artifactSetDigest, ConstructionUnitReportSetDigest: cuSetDigest, SourceAncestryReceiptDigest: &ancestryDigest, Entries: entries, SignerPublicKey: floor.Authority.Ed25519PublicKey}
	bundle.BundleSignature = signReleaseObjectForTest(t, bundle, privateKey)
	graph := ReleaseGraphV1{Authority: floor.Authority, Ancestry: &ancestry, BuildReport: build, VetReport: vet, RaceReport: race, CUReportSet: cuSet, ArtifactManifests: manifests, ArtifactSet: artifactSet, Bundle: bundle}
	closeReleaseFixtureGraph(t, &graph)
	return graph
}

func closeReleaseFixtureGraph(t *testing.T, graph *ReleaseGraphV1) {
	t.Helper()
	evidence := releaseEvidenceForTest(t, *graph)
	privateKey := fixedReleasePrivateKey()
	publicKey := hex.EncodeToString(privateKey.Public().(ed25519.PublicKey))
	purpose := graph.ArtifactSet.Purpose
	repositoryDigest := framedDigestForTest("cq/repository-identity/v1\x00", bytes.TrimSpace(evidence.RepositoryRemote))
	treeDigest := sha256HexForTest(evidence.SourceTreeListing)
	environmentDigest, err := BuildEnvironmentDigestV1(evidence.BuildEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	reachabilityDigest := graph.ArtifactSet.LegacyAtomicWriterReachabilityCatalogueDigest

	graph.Authority.RepositoryIdentityDigest = repositoryDigest
	graph.Authority.Ed25519PublicKey = publicKey
	graph.Authority.LineageRootCommit = strings.Repeat("a", 40)
	if purpose == "floor" {
		graph.Authority.LineageRootTreeDigest = treeDigest
	}
	authorityDigest := releaseObjectDigestForTest(t, "cq/release-build-authority/v1\x00", graph.Authority)

	manifestDigests := make([]string, len(graph.ArtifactManifests))
	roles := make([]ReleaseArtifactRoleV1, len(graph.ArtifactManifests))
	for index := range graph.ArtifactManifests {
		manifest := &graph.ArtifactManifests[index]
		retained := evidence.Roles[index]
		commandDigest, err := CommandDigestV1(retained.Command.Purpose, retained.Command.WorkingDirectory, retained.Command.Argv)
		if err != nil {
			t.Fatal(err)
		}
		manifest.ReleaseBuildAuthorityDigest = authorityDigest
		manifest.SourceCommit = evidence.SourceCommit
		manifest.SourceTreeDigest = treeDigest
		manifest.BuildEnvironmentDigest = environmentDigest
		manifest.BuildCommandDigest = commandDigest
		manifest.ArtifactPayloadDigest = sha256HexForTest(retained.Payload)
		manifest.CodeSignatureDigest = sha256HexForTest(retained.CodeSignature)
		if manifest.LauncherABIDigest != nil {
			digest := sha256HexForTest(retained.LauncherABIBytes)
			manifest.LauncherABIDigest = &digest
		}
		if manifest.PrivateABIDigest != nil {
			digest := sha256HexForTest(retained.PrivateABIBytes)
			manifest.PrivateABIDigest = &digest
		}
		manifestDigests[index] = releaseObjectDigestForTest(t, "cq/release-artifact-manifest/v1\x00", *manifest)
		roles[index] = ReleaseArtifactRoleV1{Role: manifest.Role, ArtifactPayloadDigest: manifest.ArtifactPayloadDigest, ArtifactManifestDigest: manifestDigests[index]}
	}

	reports := []*ReleaseBuildReportV1{&graph.BuildReport, &graph.VetReport, &graph.RaceReport}
	for index, report := range reports {
		retained := evidence.BuildReports[index]
		commandDigest, resultDigest, err := retainedCommandDigestsForTest(retained)
		if err != nil {
			t.Fatal(err)
		}
		report.Purpose = purpose
		report.ReleaseBuildAuthorityDigest = authorityDigest
		report.SourceCommit = evidence.SourceCommit
		report.SourceTreeDigest = treeDigest
		report.BuildEnvironmentDigest = environmentDigest
		report.CommandDigest = commandDigest
		report.ExecutionResultDigest = resultDigest
		report.ExitCode = retained.ExitCode
		report.Outcome = "passed"
		report.SignerPublicKey = publicKey
		report.Signature = ""
		if index == 0 {
			report.RoleExecutions = make([]ReleaseRoleExecutionV1, len(roles))
			for roleIndex := range roles {
				report.RoleExecutions[roleIndex] = ReleaseRoleExecutionV1{Role: roles[roleIndex].Role, BuildCommandDigest: graph.ArtifactManifests[roleIndex].BuildCommandDigest, ArtifactPayloadDigest: roles[roleIndex].ArtifactPayloadDigest, ArtifactManifestDigest: roles[roleIndex].ArtifactManifestDigest}
			}
		} else {
			report.RoleExecutions = []ReleaseRoleExecutionV1{}
		}
		report.Signature = signReleaseObjectForTest(t, *report, privateKey)
	}

	graph.CUReportSet.Purpose = purpose
	graph.CUReportSet.ReleaseBuildAuthorityDigest = authorityDigest
	graph.CUReportSet.LegacyAtomicWriterReachabilityCatalogueDigest = reachabilityDigest
	graph.CUReportSet.SourceCommit = evidence.SourceCommit
	graph.CUReportSet.SourceTreeDigest = treeDigest
	graph.CUReportSet.BuildEnvironmentDigest = environmentDigest
	graph.CUReportSet.SignerPublicKey = publicKey
	for index := range graph.CUReportSet.Reports {
		retained := evidence.ConstructionUnits[index]
		commandDigest, resultDigest, err := retainedCommandDigestsForTest(retained.Command)
		if err != nil {
			t.Fatal(err)
		}
		report := &graph.CUReportSet.Reports[index]
		report.VerificationManifestDigest = framedDigestForTest("cq/construction-unit-verification-manifest/v1\x00", retained.ManifestBytes)
		report.InvocationDigest = commandDigest
		report.ExecutionResultDigest = resultDigest
		report.ExitCode = retained.Command.ExitCode
		report.Outcome = "passed"
		report.RaceEnabled = true
	}
	graph.CUReportSet.Signature = ""
	graph.CUReportSet.Signature = signReleaseObjectForTest(t, graph.CUReportSet, privateKey)

	var ancestryDigest *string
	if purpose == "target" {
		commandDigest, _, err := retainedCommandDigestsForTest(*evidence.Ancestry)
		if err != nil {
			t.Fatal(err)
		}
		graph.Ancestry.ReleaseBuildAuthorityDigest = authorityDigest
		graph.Ancestry.RepositoryIdentityDigest = repositoryDigest
		graph.Ancestry.FloorSourceCommit = graph.Authority.LineageRootCommit
		graph.Ancestry.FloorSourceTreeDigest = graph.Authority.LineageRootTreeDigest
		graph.Ancestry.TargetSourceCommit = evidence.SourceCommit
		graph.Ancestry.TargetSourceTreeDigest = treeDigest
		graph.Ancestry.MergeBaseCommit = graph.Authority.LineageRootCommit
		graph.Ancestry.VerificationCommandDigest = commandDigest
		graph.Ancestry.SignerPublicKey = publicKey
		graph.Ancestry.Signature = ""
		graph.Ancestry.Signature = signReleaseObjectForTest(t, *graph.Ancestry, privateKey)
		digest := releaseObjectDigestForTest(t, "cq/source-ancestry-receipt/v1\x00", *graph.Ancestry)
		ancestryDigest = &digest
	}

	buildDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", graph.BuildReport)
	vetDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", graph.VetReport)
	raceDigest := releaseObjectDigestForTest(t, "cq/release-build-report/v1\x00", graph.RaceReport)
	cuSetDigest := releaseObjectDigestForTest(t, "cq/construction-unit-report-set/v1\x00", graph.CUReportSet)
	set := &graph.ArtifactSet
	set.ReleaseBuildAuthorityDigest = authorityDigest
	set.SignerPublicKey = publicKey
	set.SourceCommit = evidence.SourceCommit
	set.SourceTreeDigest = treeDigest
	set.BuildEnvironmentDigest = environmentDigest
	set.BuildReportDigest = buildDigest
	set.VetReportDigest = vetDigest
	set.RaceTestReportDigest = raceDigest
	set.ConstructionUnitReportSetDigest = cuSetDigest
	set.LegacyAtomicWriterReachabilityCatalogueDigest = reachabilityDigest
	set.SourceAncestryReceiptDigest = ancestryDigest
	set.Roles = roles
	if purpose == "floor" {
		set.LauncherABIDigest = nil
		value := *graph.ArtifactManifests[0].LauncherABIDigest
		set.RequiredLauncherABIDigest = &value
	} else {
		value := *graph.ArtifactManifests[0].LauncherABIDigest
		set.LauncherABIDigest = &value
		set.RequiredLauncherABIDigest = nil
	}
	set.SetSignature = ""
	set.SetSignature = signReleaseObjectForTest(t, *set, privateKey)
	setDigest := releaseObjectDigestForTest(t, "cq/release-artifact-set/v1\x00", *set)

	entries := make([]ReleaseBundleEntryV1, 0, 13)
	for index, manifest := range graph.ArtifactManifests {
		entries = append(entries,
			ReleaseBundleEntryV1{RelativePath: "manifests/" + manifest.Role + ".json", Kind: "file", Digest: manifestDigests[index], Size: canonicalSizeForTest(t, manifest)},
			ReleaseBundleEntryV1{RelativePath: "payloads/" + manifest.Role, Kind: "file", Digest: manifest.ArtifactPayloadDigest, Size: uint64(len(evidence.Roles[index].Payload))},
		)
	}
	entries = append(entries,
		ReleaseBundleEntryV1{RelativePath: "release-artifact-set.json", Kind: "file", Digest: setDigest, Size: canonicalSizeForTest(t, *set)},
		ReleaseBundleEntryV1{RelativePath: "release-build-authority.json", Kind: "file", Digest: authorityDigest, Size: canonicalSizeForTest(t, graph.Authority)},
		ReleaseBundleEntryV1{RelativePath: "reports/build.json", Kind: "file", Digest: buildDigest, Size: canonicalSizeForTest(t, graph.BuildReport)},
		ReleaseBundleEntryV1{RelativePath: "reports/construction-units.json", Kind: "file", Digest: cuSetDigest, Size: canonicalSizeForTest(t, graph.CUReportSet)},
		ReleaseBundleEntryV1{RelativePath: "reports/race.json", Kind: "file", Digest: raceDigest, Size: canonicalSizeForTest(t, graph.RaceReport)},
		ReleaseBundleEntryV1{RelativePath: "reports/vet.json", Kind: "file", Digest: vetDigest, Size: canonicalSizeForTest(t, graph.VetReport)},
	)
	if purpose == "target" {
		entries = append(entries, ReleaseBundleEntryV1{RelativePath: "source-ancestry.json", Kind: "file", Digest: *ancestryDigest, Size: canonicalSizeForTest(t, *graph.Ancestry)})
	}
	slices.SortFunc(entries, func(left, right ReleaseBundleEntryV1) int {
		return strings.Compare(left.RelativePath, right.RelativePath)
	})
	graph.Bundle = ReleaseBundleV1{SchemaVersion: 1, Purpose: purpose, ReleaseBuildAuthorityDigest: authorityDigest, ReleaseArtifactSetDigest: setDigest, ConstructionUnitReportSetDigest: cuSetDigest, SourceAncestryReceiptDigest: ancestryDigest, Entries: entries, SignerPublicKey: publicKey}
	graph.Bundle.BundleSignature = signReleaseObjectForTest(t, graph.Bundle, privateKey)
}

func releaseEvidenceForTest(t *testing.T, graph ReleaseGraphV1) ReleaseVerificationEvidenceV1 {
	t.Helper()
	purpose := graph.ArtifactSet.Purpose
	evidence := ReleaseVerificationEvidenceV1{
		RepositoryRemote:  []byte("https://example.invalid/cq.git\n"),
		SourceCommit:      strings.Repeat("a", 40),
		SourceTreeListing: []byte("100644 blob 1111111111111111111111111111111111111111\tfloor\x00"),
		BuildEnvironment:  releaseEnvironmentForTest(),
	}
	if purpose == "target" {
		evidence.SourceCommit = strings.Repeat("f", 40)
		evidence.SourceTreeListing = []byte("100644 blob 2222222222222222222222222222222222222222\ttarget\x00")
		ancestry := releaseCommandEvidenceForTest("ancestry", []string{"/usr/bin/git", "merge-base", strings.Repeat("a", 40), strings.Repeat("f", 40)})
		ancestry.Stdout = []byte(strings.Repeat("a", 40) + "\n")
		evidence.Ancestry = &ancestry
	}
	evidence.BuildReports = append(evidence.BuildReports,
		releaseCommandEvidenceForTest("release-"+purpose+"-build", []string{"/usr/local/go/bin/go", "build", "./..."}),
		releaseCommandEvidenceForTest("release-"+purpose+"-vet", []string{"/usr/local/go/bin/go", "vet", "./..."}),
		releaseCommandEvidenceForTest("release-"+purpose+"-race", []string{"/usr/local/go/bin/go", "test", "-race", "-count=1", "./..."}),
	)
	for _, report := range graph.CUReportSet.Reports {
		manifestBytes := releaseCUManifestForTest(t, report.CUID)
		evidence.ConstructionUnits = append(evidence.ConstructionUnits, ReleaseCUEvidenceV1{CUID: report.CUID, ManifestBytes: manifestBytes, Command: releaseCommandEvidenceForTest("verify-"+report.CUID, []string{"./scripts/verify-proxy-cu", report.CUID})})
	}
	for _, manifest := range graph.ArtifactManifests {
		role := ReleaseRoleEvidenceV1{
			Role:          manifest.Role,
			Command:       releaseCommandEvidenceForTest("role-"+manifest.Role+"-build", []string{"/usr/local/go/bin/go", "build", "-o", manifest.Role, "./cmd/cq"}),
			Payload:       []byte(manifest.Role),
			CodeSignature: []byte(manifest.Role + "-code-signature"),
		}
		if manifest.LauncherABIDigest != nil {
			role.LauncherABIBytes = []byte("launcher-abi-v1")
		}
		if manifest.PrivateABIDigest != nil {
			role.PrivateABIBytes = []byte("private-abi-v1")
		}
		evidence.Roles = append(evidence.Roles, role)
	}
	return evidence
}

func releaseCommandEvidenceForTest(purpose string, argv []string) ReleaseCommandEvidenceV1 {
	return ReleaseCommandEvidenceV1{Purpose: purpose, WorkingDirectory: ".", Argv: argv, ExitCode: 0, TerminationReason: "exited", Stdout: []byte(purpose + "\n"), Stderr: []byte{}}
}

func retainedCommandDigestsForTest(evidence ReleaseCommandEvidenceV1) (string, string, error) {
	commandDigest, err := CommandDigestV1(evidence.Purpose, evidence.WorkingDirectory, evidence.Argv)
	if err != nil {
		return "", "", err
	}
	resultDigest, err := ExecutionResultDigestV1(evidence.ExitCode, evidence.TerminationReason, evidence.Stdout, evidence.Stderr)
	return commandDigest, resultDigest, err
}

func releaseEnvironmentForTest() []BuildEnvironmentEntryV1 {
	return []BuildEnvironmentEntryV1{
		{Key: "CGO_ENABLED", Value: "1"}, {Key: "GOAMD64", Value: ""}, {Key: "GOARCH", Value: "arm64"},
		{Key: "GOARM", Value: ""}, {Key: "GOARM64", Value: ""}, {Key: "GOEXPERIMENT", Value: ""},
		{Key: "GOFLAGS", Value: "-trimpath"}, {Key: "GOOS", Value: "darwin"}, {Key: "GOTOOLCHAIN", Value: "go1.26.1"},
		{Key: "LC_ALL", Value: "C"}, {Key: "SOURCE_DATE_EPOCH", Value: "1786960800"}, {Key: "TZ", Value: "UTC"},
	}
}

func releaseCUManifestForTest(t *testing.T, cuID string) []byte {
	t.Helper()
	if data, err := CanonicalCUManifestV1(cuID); err == nil {
		return data
	}
	data, err := CanonicalJSONV1(CUManifestV1{SchemaVersion: 1, Kind: "construction_unit_verification_manifest_v1", BlueprintSHA256: frozenBlueprintSHA256, ReviewAttestationAggregateSHA256: frozenReviewAggregateSHA256, ReviewAuthorityBaselineCommit: frozenReviewBaseline, Unit: cuID, RaceCount: 1, Packages: []CUTestPackageV1{{Package: "./internal/proxy", TopLevelTests: []string{"TestFixture"}, FullTestIDs: []string{"TestFixture"}, MinimumPassCount: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func framedDigestForTest(domain string, data []byte) string {
	hash := sha256.New()
	hash.Write([]byte(domain))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	hash.Write(length[:])
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func materialiseFloorReleaseBundle(t *testing.T, graph ReleaseGraphV1) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"manifests", "payloads", "reports"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	objects := map[string]any{
		"bundle.json":                     graph.Bundle,
		"release-build-authority.json":    graph.Authority,
		"release-artifact-set.json":       graph.ArtifactSet,
		"reports/build.json":              graph.BuildReport,
		"reports/vet.json":                graph.VetReport,
		"reports/race.json":               graph.RaceReport,
		"reports/construction-units.json": graph.CUReportSet,
	}
	for _, manifest := range graph.ArtifactManifests {
		objects["manifests/"+manifest.Role+".json"] = manifest
	}
	for path, object := range objects {
		data, err := CanonicalJSONV1(object)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for role, data := range map[string][]byte{"supervisor": []byte("supervisor"), "worker": []byte("worker")} {
		if err := os.WriteFile(filepath.Join(root, "payloads", role), data, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func canonicalSizeForTest(t *testing.T, value any) uint64 {
	t.Helper()
	data, err := CanonicalJSONV1(value)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(len(data))
}

func sha256HexForTest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func signReleaseObjectForTest(t *testing.T, value any, privateKey ed25519.PrivateKey) string {
	t.Helper()
	switch object := value.(type) {
	case SourceAncestryReceiptV1:
		value = sourceAncestrySignable(object)
	case ReleaseBuildReportV1:
		value = releaseBuildReportSignable(object)
	case ConstructionUnitReportSetV1:
		value = cuReportSetSignable(object)
	case ReleaseArtifactSetV1:
		value = releaseArtifactSetSignable(object)
	case ReleaseBundleV1:
		value = releaseBundleSignable(object)
	}
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(ed25519.Sign(privateKey, canonical))
}

func fixedReleasePrivateKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = 7
	}
	return ed25519.NewKeyFromSeed(seed)
}

func resignReleaseObjectForTest(t *testing.T, value any) {
	t.Helper()
	privateKey := fixedReleasePrivateKey()
	switch object := value.(type) {
	case *SourceAncestryReceiptV1:
		object.Signature = ""
		object.Signature = signReleaseObjectForTest(t, sourceAncestrySignable(*object), privateKey)
	case *ReleaseBuildReportV1:
		object.Signature = ""
		object.Signature = signReleaseObjectForTest(t, releaseBuildReportSignable(*object), privateKey)
	case *ConstructionUnitReportSetV1:
		object.Signature = ""
		object.Signature = signReleaseObjectForTest(t, cuReportSetSignable(*object), privateKey)
	case *ReleaseArtifactSetV1:
		object.SetSignature = ""
		object.SetSignature = signReleaseObjectForTest(t, releaseArtifactSetSignable(*object), privateKey)
	case *ReleaseBundleV1:
		object.BundleSignature = ""
		object.BundleSignature = signReleaseObjectForTest(t, releaseBundleSignable(*object), privateKey)
	default:
		t.Fatalf("cannot resign %T", value)
	}
}

func releaseObjectDigestForTest(t *testing.T, domain string, value any) string {
	t.Helper()
	canonical, err := CanonicalJSONV1(value)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	hash.Write([]byte(domain))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(canonical)))
	hash.Write(length[:])
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil))
}

func releaseAuthorityPinForTest(t *testing.T, graph ReleaseGraphV1) ReleaseAuthorityPinV1 {
	t.Helper()
	return ReleaseAuthorityPinV1{
		Digest:           releaseObjectDigestForTest(t, "cq/release-build-authority/v1\x00", graph.Authority),
		Ed25519PublicKey: graph.Authority.Ed25519PublicKey,
	}
}

func makeCUReports(count int) []CUReportV1 {
	reports := make([]CUReportV1, count)
	for index := range reports {
		reports[index] = CUReportV1{SchemaVersion: 1, CUID: "CU-" + string(rune('0'+index)), Kind: "construction_unit_report_v1", Outcome: "passed", RaceEnabled: true}
	}
	return reports
}

func makeReleaseBundleEntries(purpose string) []ReleaseBundleEntryV1 {
	paths := []string{
		"manifests/supervisor.json", "manifests/worker.json", "payloads/supervisor", "payloads/worker",
		"release-artifact-set.json", "release-build-authority.json", "reports/build.json", "reports/construction-units.json", "reports/race.json", "reports/vet.json",
	}
	if purpose == "target" {
		paths = append(paths, "manifests/launcher.json", "payloads/launcher", "source-ancestry.json")
	}
	// Validation requires byte ordering, so keep the fixture independent of
	// implementation-owned expected path tables.
	slices.Sort(paths)
	entries := make([]ReleaseBundleEntryV1, len(paths))
	for index, path := range paths {
		entries[index] = ReleaseBundleEntryV1{RelativePath: path, Kind: "file", Digest: strings.Repeat("1", 64), Size: 1}
	}
	return entries
}

func TestParseCUManifestRejectsUnknownMemberBeforeDispatch(t *testing.T) {
	_, err := ParseCUManifestV1(strings.NewReader(`{"schema_version":1,"unit":"CU-0","extra":true}`))
	if err == nil {
		t.Fatal("accepted unknown member")
	}
}

func TestExecutionResultDigestV1MatchesLiteralFraming(t *testing.T) {
	got, err := ExecutionResultDigestV1(0, "exited", []byte("ok\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	const want = "5b5105133bcdac62f609fb3e6d2ca7aaa3d9a5cbcad526b7aef67baf66d05bf1"
	if got != want {
		t.Fatalf("ExecutionResultDigestV1() = %s, want %s", got, want)
	}
}

func TestNewCUReportV1TreatsForgedReportOutputAsCapturedBytes(t *testing.T) {
	started := time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC)
	ended := started.Add(time.Second)
	capture := CUReportCaptureV1{
		CUID:                       "CU-0",
		VerificationManifestDigest: strings.Repeat("1", 64),
		InvocationDigest:           strings.Repeat("2", 64),
		ExitCode:                   0,
		TerminationReason:          "exited",
		RaceEnabled:                true,
		Stdout:                     []byte(`{"kind":"construction_unit_report_v1"}`),
		StartedAt:                  started,
		EndedAt:                    ended,
	}
	report, err := NewCUReportV1(capture)
	if err != nil {
		t.Fatal(err)
	}
	changed := capture
	changed.Stdout = append(append([]byte(nil), capture.Stdout...), '\n')
	changedReport, err := NewCUReportV1(changed)
	if err != nil {
		t.Fatal(err)
	}
	if report.ExecutionResultDigest == changedReport.ExecutionResultDigest {
		t.Fatal("captured output substitution did not change the result digest")
	}
	if report.Kind != "construction_unit_report_v1" || report.Outcome != "passed" {
		t.Fatalf("report = %#v", report)
	}
}

func TestNewCUReportV1RejectsNonPassedAndOversizeCapture(t *testing.T) {
	base := CUReportCaptureV1{
		CUID:                       "CU-0",
		VerificationManifestDigest: strings.Repeat("1", 64),
		InvocationDigest:           strings.Repeat("2", 64),
		RaceEnabled:                true,
		TerminationReason:          "exited",
		StartedAt:                  time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC),
		EndedAt:                    time.Date(2026, 8, 14, 16, 0, 1, 0, time.UTC),
	}
	for name, mutate := range map[string]func(*CUReportCaptureV1){
		"nonzero":       func(value *CUReportCaptureV1) { value.ExitCode = 1 },
		"signal":        func(value *CUReportCaptureV1) { value.TerminationReason = "signalled" },
		"race disabled": func(value *CUReportCaptureV1) { value.RaceEnabled = false },
		"oversize":      func(value *CUReportCaptureV1) { value.Stdout = make([]byte, maxCUCaptureStreamBytes+1) },
	} {
		t.Run(name, func(t *testing.T) {
			capture := base
			mutate(&capture)
			if _, err := NewCUReportV1(capture); err == nil {
				t.Fatal("accepted invalid CU capture")
			}
		})
	}
}

func TestParseCUManifestAcceptsClosedCU0Selection(t *testing.T) {
	input := `{"blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","kind":"construction_unit_verification_manifest_v1","packages":[{"full_test_ids":["TestCanonicalJSONV1UsesJCSKeyOrderAndStringEscaping"],"minimum_pass_count":1,"package":"./internal/proxy","top_level_tests":["TestCanonicalJSONV1UsesJCSKeyOrderAndStringEscaping"]}],"race_count":1,"review_attestation_aggregate_sha256":"3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be","review_authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","schema_version":1,"unit":"CU-0"}`
	manifest, err := ParseCUManifestV1(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Packages) != 1 || manifest.Packages[0].MinimumPassCount != 1 {
		t.Fatalf("manifest packages = %#v", manifest.Packages)
	}
}

func TestCanonicalCUManifestV1ProvidesPinnedCU0Selection(t *testing.T) {
	data, err := CanonicalCUManifestV1("CU-0")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseCUManifestV1(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Unit != "CU-0" || len(manifest.Packages) != 4 {
		t.Fatalf("CU-0 manifest = %#v", manifest)
	}
	wantCounts := map[string]int{
		"./cmd/cq":                      81,
		"./internal/proxy":              266,
		"./internal/tools/proxycu":      66,
		"./internal/tools/proxyrelease": 7,
	}
	for _, selection := range manifest.Packages {
		if got, want := len(selection.FullTestIDs), wantCounts[selection.Package]; got != want {
			t.Fatalf("CU-0 manifest package %s has %d tests, want %d", selection.Package, got, want)
		}
	}
	cu1Data, err := CanonicalCUManifestV1("CU-1")
	if err != nil {
		t.Fatal(err)
	}
	cu1, err := ParseCUManifestV1(strings.NewReader(string(cu1Data)))
	if err != nil {
		t.Fatal(err)
	}
	wantCU1Counts := map[string]int{"./cmd/cq": 20, "./internal/proxy": 20}
	if cu1.Unit != "CU-1" || len(cu1.Packages) != len(wantCU1Counts) {
		t.Fatalf("CU-1 manifest = %#v", cu1)
	}
	for _, selection := range cu1.Packages {
		if got, want := len(selection.FullTestIDs), wantCU1Counts[selection.Package]; got != want {
			t.Fatalf("CU-1 manifest package %s has %d tests, want %d", selection.Package, got, want)
		}
	}
	cu2Data, err := CanonicalCUManifestV1("CU-2")
	if err != nil {
		t.Fatal(err)
	}
	cu2, err := ParseCUManifestV1(strings.NewReader(string(cu2Data)))
	if err != nil {
		t.Fatal(err)
	}
	wantCU2Counts := map[string]int{"./internal/auth": 3, "./internal/provider/codex": 48, "./internal/proxy": 7}
	if cu2.Unit != "CU-2" || len(cu2.Packages) != len(wantCU2Counts) {
		t.Fatalf("CU-2 manifest = %#v", cu2)
	}
	for _, selection := range cu2.Packages {
		if got, want := len(selection.FullTestIDs), wantCU2Counts[selection.Package]; got != want {
			t.Fatalf("CU-2 manifest package %s has %d tests, want %d", selection.Package, got, want)
		}
	}
}

func TestParseCUManifestRejectsEmptySelectionDuplicateTestAndWrongRaceCount(t *testing.T) {
	for name, input := range map[string]string{
		"empty selection": `{"blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","kind":"construction_unit_verification_manifest_v1","packages":[],"race_count":1,"review_attestation_aggregate_sha256":"3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be","review_authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","schema_version":1,"unit":"CU-0"}`,
		"duplicate test":  `{"blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","kind":"construction_unit_verification_manifest_v1","packages":[{"full_test_ids":["TestOne","TestOne"],"minimum_pass_count":2,"package":"./internal/proxy","top_level_tests":["TestOne"]}],"race_count":1,"review_attestation_aggregate_sha256":"3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be","review_authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","schema_version":1,"unit":"CU-0"}`,
		"race count":      `{"blueprint_sha256":"bd8bdff9a8ce4582d0a66e847c74f5e69744651de457ba8e6847e0fcda678f38","kind":"construction_unit_verification_manifest_v1","packages":[{"full_test_ids":["TestOne"],"minimum_pass_count":1,"package":"./internal/proxy","top_level_tests":["TestOne"]}],"race_count":2,"review_attestation_aggregate_sha256":"3b227af5077cbaab1ad1f29444549062bad5c343baa1d15e254a1994fe2850be","review_authority_baseline_commit":"9fe30df8d4101f69084d6487740ed324a5d0b59d","schema_version":1,"unit":"CU-0"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCUManifestV1(strings.NewReader(input)); err == nil {
				t.Fatal("accepted invalid CU manifest")
			}
		})
	}
}
