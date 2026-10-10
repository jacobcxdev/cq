//go:build darwin

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/installer"
	"github.com/jacobcxdev/cq/internal/installstate"
	"github.com/jacobcxdev/cq/internal/proxy"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestDarwinRuntimeStageUsesStablePackageLink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "package-cq")
	command := exec.Command("go", "build", "-o", source, "-ldflags", "-X main.version=0.34.0", ".")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v %s", err, out)
	}
	link := filepath.Join(root, "cq")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	store := darwinServiceRuntimeArtifacts{installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: userdirs.Roots{State: filepath.Join(root, "state")}}}
	artifact, err := store.Stage(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Version != "0.34.0" || artifact.Path == source || artifact.Path == link {
		t.Fatalf("package not retained: %+v", artifact)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(context.Background(), artifact); err != nil {
		t.Fatalf("package purge affected runtime: %v", err)
	}
	if _, err := store.Stage(context.Background(), link); err == nil {
		t.Fatal("dangling package link accepted")
	}
}

func TestDarwinRuntimePruningWaitsForOwnershipAndPreservesJobReferences(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	roots := userdirs.Roots{State: filepath.Join(root, "state")}
	artifacts := installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	var copies []installer.RuntimeArtifact
	for _, digit := range []string{"a", "b", "c", "d", "e"} {
		digest := strings.Repeat(digit, 64)
		path := filepath.Join(roots.State, "runtime-artifacts", digest, "cq")
		if err := fsutil.EnsureSecureDirectory(artifacts.FS, filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
		copies = append(copies, installer.RuntimeArtifact{Path: path, SHA256: digest, Version: "0.34.0", ProtocolVersion: 1})
	}
	platform := &darwinServicePlatform{home: root, roots: roots}
	if err := os.MkdirAll(filepath.Dir(platform.plistPath(proxyAgentLabel)), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, definition := range []darwinLaunchAgentDefinition{
		{Label: proxyAgentLabel, ProgramArguments: []string{copies[0].Path, "proxy", "start"}, StandardErrorPath: filepath.Join(root, "proxy.log")},
		{Label: agentLabel, ProgramArguments: []string{copies[1].Path, "refresh"}, StandardErrorPath: filepath.Join(root, "refresh.log")},
	} {
		data, err := renderDarwinLaunchAgent(definition)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(platform.plistPath(definition.Label), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ownership := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots}
	record := installstate.Record{SchemaVersion: 1, Owner: installstate.OwnerHomebrew, Version: "0.34.0", Executable: filepath.Join(root, "package-cq"), BinaryDigest: copies[2].SHA256, Services: []string{proxyAgentLabel, agentLabel}}
	if err := ownership.Save(record); err != nil {
		t.Fatal(err)
	}
	receipts := proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "prune", Generation: 1, Phase: "prepared", Previous: copies[2], Candidate: copies[3], ListenerIdentity: "tcp|127.0.0.1:29280", SupervisorPID: 42}
	for _, phase := range []string{"prepared", "waiting", "handoff", "verifying", "committed"} {
		receipt.Phase = phase
		if err := receipts.Save(receipt); err != nil {
			t.Fatal(err)
		}
		if err := pruneDarwinServiceRuntime(ctx, platform, ownership, artifacts, receipts); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Dir(copies[4].Path)); err != nil {
			t.Fatalf("pruned before runtime and ownership agreed: %s %v", phase, err)
		}
	}
	record.BinaryDigest = copies[3].SHA256
	if err := ownership.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := pruneDarwinServiceRuntime(ctx, platform, ownership, artifacts, receipts); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range copies[:4] {
		if _, err := os.Stat(filepath.Dir(artifact.Path)); err != nil {
			t.Fatalf("job or transaction reference lost: %s %v", artifact.Path, err)
		}
	}
	if _, err := os.Stat(filepath.Dir(copies[4].Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete directory retained: %v", err)
	}
	// A snapshot made after A -> B must still restore refresh B after the
	// current job and latest transaction have advanced to C -> D.
	var components []serviceComponentSnapshot
	for _, label := range []string{proxyAgentLabel, agentLabel} {
		data, err := os.ReadFile(platform.plistPath(label))
		if err != nil {
			t.Fatal(err)
		}
		components = append(components, serviceComponentSnapshot{ID: label, Exists: true, Definition: data})
	}
	snapshot := persistedServiceSnapshot{SchemaVersion: serviceSnapshotSchemaVersion, Owner: installstate.OwnerHomebrew, Executable: record.Executable, Platform: servicePlatformSnapshot{Manager: "launchd", Components: components}}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(root, "snapshots", "old.json")
	lock, err := artifacts.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDarwinRuntimeSnapshot(ctx, artifacts, snapshotPath, data); !errors.Is(err, fsutil.ErrExclusiveLockHeld) {
		t.Fatalf("snapshot publication bypassed artifact lock: %v", err)
	}
	lock.Close()
	if err := writeDarwinRuntimeSnapshot(ctx, artifacts, snapshotPath, data); err != nil {
		t.Fatal(err)
	}
	refresh, err := renderDarwinLaunchAgent(darwinLaunchAgentDefinition{Label: agentLabel, ProgramArguments: []string{copies[3].Path, "refresh"}, StandardErrorPath: filepath.Join(root, "refresh.log")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform.plistPath(agentLabel), refresh, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pruneDarwinServiceRuntime(ctx, platform, ownership, artifacts, receipts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(copies[1].Path)); err != nil {
		t.Fatalf("saved snapshot lost its refresh executable: %v", err)
	}
	if err := os.WriteFile(snapshotPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pruneDarwinServiceRuntime(ctx, platform, ownership, artifacts, receipts); err == nil {
		t.Fatal("changed snapshot allowed pruning")
	}
	if _, err := os.Stat(filepath.Dir(copies[1].Path)); err != nil {
		t.Fatalf("changed snapshot lost its retained executable: %v", err)
	}
	if err := os.Remove(snapshotPath); err != nil {
		t.Fatal(err)
	}
	if err := pruneDarwinServiceRuntime(ctx, platform, ownership, artifacts, receipts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(copies[1].Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted snapshot still pinned old runtime: %v", err)
	}
	pins, err := os.ReadDir(filepath.Join(roots.State, "runtime-snapshots"))
	if err != nil || len(pins) != 0 {
		t.Fatalf("deleted snapshot left retention pin: %v %v", pins, err)
	}
}

type upgradeBrokenResponseBody struct{}

func (upgradeBrokenResponseBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestDarwinRuntimeUpgradeSettlesAmbiguousSubmission(t *testing.T) {
	for _, outcome := range []string{"transport", "body", "invalid-receipt"} {
		t.Run(outcome, func(t *testing.T) {
			store := proxy.RuntimeUpgradeStore{FS: fsutil.NewMemFS(), Roots: userdirs.Roots{State: "/fixture/state"}}
			previous := installer.RuntimeArtifact{Path: "/fixture/previous/cq", SHA256: strings.Repeat("a", 64), Version: "0.34.0", ProtocolVersion: 1}
			candidate := installer.RuntimeArtifact{Path: "/fixture/candidate/cq", SHA256: strings.Repeat("b", 64), Version: "0.34.1", ProtocolVersion: 1}
			receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "lost-ack", Generation: 1, Phase: "prepared", Previous: previous, Candidate: candidate, ListenerIdentity: "tcp|127.0.0.1:29280", SupervisorPID: 42}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:29280", nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			client := testDoer(func(*http.Request) (*http.Response, error) {
				if err := store.Save(receipt); err != nil {
					return nil, err
				}
				cancel()
				go func() {
					time.Sleep(25 * time.Millisecond)
					for _, phase := range []string{"waiting", "handoff", "verifying", "committed"} {
						receipt.Phase = phase
						if err := store.Save(receipt); err != nil {
							done <- err
							return
						}
					}
					done <- nil
				}()
				if outcome == "transport" {
					return nil, errors.New("acknowledgement lost")
				}
				body := io.NopCloser(strings.NewReader("invalid JSON"))
				if outcome == "body" {
					body = io.NopCloser(upgradeBrokenResponseBody{})
				}
				return &http.Response{StatusCode: http.StatusAccepted, Body: body}, nil
			})
			settled, err := submitDarwinServiceRuntimeUpgrade(ctx, store, candidate, "lost-ack", proxy.RuntimeUpgradeReceiptV1{}, request, client)
			if updateErr := <-done; updateErr != nil {
				t.Fatal(updateErr)
			}
			if err != nil || settled.Phase != "committed" || settled.TransactionID != "lost-ack" {
				t.Fatalf("returned before accepted transaction settled: %+v %v", settled, err)
			}
		})
	}
}

func TestDarwinRuntimeUpgradeWaitsForKnownTransactionPastDiscoveryDeadline(t *testing.T) {
	store := proxy.RuntimeUpgradeStore{FS: fsutil.NewMemFS(), Roots: userdirs.Roots{State: "/fixture/state"}}
	previous := installer.RuntimeArtifact{Path: "/fixture/previous/cq", SHA256: strings.Repeat("a", 64), Version: "0.34.0", ProtocolVersion: 1}
	candidate := installer.RuntimeArtifact{Path: "/fixture/candidate/cq", SHA256: strings.Repeat("b", 64), Version: "0.34.1", ProtocolVersion: 1}
	receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "long-drain", Generation: 1, Phase: "prepared", Previous: previous, Candidate: candidate, ListenerIdentity: "tcp|127.0.0.1:29280", SupervisorPID: 42}
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Phase = "waiting"
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	discovery, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errors.New("receipt publication panic")
			}
		}()
		time.Sleep(25 * time.Millisecond)
		for _, phase := range []string{"waiting", "handoff", "verifying", "committed"} {
			receipt.Phase = phase
			if err := store.Save(receipt); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var probes atomic.Int32
	settled, err := awaitDarwinServiceRuntimeUpgrade(discovery, store, candidate, "long-drain", proxy.RuntimeUpgradeReceiptV1{}, proxy.RuntimeUpgradeReceiptV1{}, nil, func() error {
		probes.Add(1)
		return nil // The paused listener's timed-out status probe is inconclusive.
	})
	if updateErr := <-done; updateErr != nil {
		t.Fatal(updateErr)
	}
	if err != nil || settled.Phase != "committed" || probes.Load() != 1 {
		t.Fatalf("accepted drain lost to discovery deadline: %+v %v", settled, err)
	}
}

func TestDarwinRuntimeUpgradePreparedOnlyKeepsDiscoveryBound(t *testing.T) {
	store := proxy.RuntimeUpgradeStore{FS: fsutil.NewMemFS(), Roots: userdirs.Roots{State: "/fixture/state"}}
	previous := installer.RuntimeArtifact{Path: "/fixture/previous/cq", SHA256: strings.Repeat("a", 64), Version: "0.34.0", ProtocolVersion: 1}
	candidate := installer.RuntimeArtifact{Path: "/fixture/candidate/cq", SHA256: strings.Repeat("b", 64), Version: "0.34.1", ProtocolVersion: 1}
	receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "prepared-only", Generation: 1, Phase: "prepared", Previous: previous, Candidate: candidate, ListenerIdentity: "tcp|127.0.0.1:29280", SupervisorPID: 42}
	if err := store.Save(receipt); err != nil {
		t.Fatal(err)
	}
	discovery, cancel := context.WithCancel(context.Background())
	cancel()
	// A terminal fallback also releases the pre-fix poller, so the regression
	// fails without leaving a background upgrade wait behind.
	timer := time.AfterFunc(100*time.Millisecond, func() {
		receipt.Phase = "failed"
		_ = store.Save(receipt)
	})
	defer timer.Stop()
	settled, err := awaitDarwinServiceRuntimeUpgrade(discovery, store, candidate, receipt.TransactionID, proxy.RuntimeUpgradeReceiptV1{}, proxy.RuntimeUpgradeReceiptV1{}, nil, nil)
	if !errors.Is(err, context.Canceled) || settled.Phase != "prepared" {
		t.Fatalf("prepared receipt hid failed waiting publication: %+v %v", settled, err)
	}
}

func TestDarwinRuntimeUpgradeReportsStoppedControllerWithReadableWaitingReceipt(t *testing.T) {
	store := proxy.RuntimeUpgradeStore{FS: fsutil.NewMemFS(), Roots: userdirs.Roots{State: "/fixture/state"}}
	previous := installer.RuntimeArtifact{Path: "/fixture/previous/cq", SHA256: strings.Repeat("a", 64), Version: "0.34.0", ProtocolVersion: 1}
	candidate := installer.RuntimeArtifact{Path: "/fixture/candidate/cq", SHA256: strings.Repeat("b", 64), Version: "0.34.1", ProtocolVersion: 1}
	receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "stopped-controller", Generation: 1, Previous: previous, Candidate: candidate, ListenerIdentity: "tcp|127.0.0.1:29280", SupervisorPID: 42}
	for _, phase := range []string{"prepared", "waiting"} {
		receipt.Phase = phase
		if err := store.Save(receipt); err != nil {
			t.Fatal(err)
		}
	}
	discovery, cancel := context.WithCancel(context.Background())
	cancel()
	// Release the pre-fix poller after demonstrating that readable waiting
	// alone cannot detect the controller's stopped transaction.
	timer := time.AfterFunc(100*time.Millisecond, func() {
		receipt.Phase = "failed"
		_ = store.Save(receipt)
	})
	defer timer.Stop()
	statusErr := errors.New("runtime upgrade status rejected: HTTP 409")
	settled, err := awaitDarwinServiceRuntimeUpgrade(discovery, store, candidate, receipt.TransactionID, proxy.RuntimeUpgradeReceiptV1{}, proxy.RuntimeUpgradeReceiptV1{}, nil, func() error { return statusErr })
	if !errors.Is(err, statusErr) || !strings.Contains(err.Error(), "runtime selection unverified") || settled.Phase != "waiting" {
		t.Fatalf("stopped controller hid behind readable waiting: %+v %v", settled, err)
	}
}

type upgradeReceiptReadFailureFS struct {
	*fsutil.MemFS
	reads    atomic.Int32
	recovery atomic.Bool
	err      error
}

func (fsys *upgradeReceiptReadFailureFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	if fsys.reads.Add(1) > 1 && !fsys.recovery.Load() {
		return nil, fsys.err
	}
	return fsys.MemFS.OpenSecureDirectory(path)
}

func TestDarwinRuntimeUpgradeReportsReceiptReadFailureAfterWaiting(t *testing.T) {
	store := proxy.RuntimeUpgradeStore{FS: fsutil.NewMemFS(), Roots: userdirs.Roots{State: "/fixture/state"}}
	previous := installer.RuntimeArtifact{Path: "/fixture/previous/cq", SHA256: strings.Repeat("a", 64), Version: "0.34.0", ProtocolVersion: 1}
	candidate := installer.RuntimeArtifact{Path: "/fixture/candidate/cq", SHA256: strings.Repeat("b", 64), Version: "0.34.1", ProtocolVersion: 1}
	receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "unreadable-drain", Generation: 1, Phase: "prepared", Previous: previous, Candidate: candidate, ListenerIdentity: "tcp|127.0.0.1:29280", SupervisorPID: 42}
	for _, phase := range []string{"prepared", "waiting"} {
		receipt.Phase = phase
		if err := store.Save(receipt); err != nil {
			t.Fatal(err)
		}
	}
	readErr := errors.New("receipt storage unavailable")
	fsys := &upgradeReceiptReadFailureFS{MemFS: store.FS.(*fsutil.MemFS), err: readErr}
	unreadable := store
	unreadable.FS = fsys
	timer := time.AfterFunc(100*time.Millisecond, func() {
		for _, phase := range []string{"handoff", "verifying", "committed"} {
			receipt.Phase = phase
			_ = store.Save(receipt)
		}
		fsys.recovery.Store(true)
	})
	defer timer.Stop()
	settled, err := awaitDarwinServiceRuntimeUpgrade(context.Background(), unreadable, candidate, receipt.TransactionID, proxy.RuntimeUpgradeReceiptV1{}, proxy.RuntimeUpgradeReceiptV1{}, nil, nil)
	if !errors.Is(err, readErr) || !strings.Contains(err.Error(), "runtime selection unverified") || settled.Phase != "waiting" {
		t.Fatalf("waiting receipt hid storage failure: %+v %v", settled, err)
	}
}

func TestDarwinRuntimeUpgradeUnobservedTransactionKeepsDiscoveryBound(t *testing.T) {
	store := proxy.RuntimeUpgradeStore{FS: fsutil.NewMemFS(), Roots: userdirs.Roots{State: "/fixture/state"}}
	discovery, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := awaitDarwinServiceRuntimeUpgrade(discovery, store, installer.RuntimeArtifact{}, "missing", proxy.RuntimeUpgradeReceiptV1{}, proxy.RuntimeUpgradeReceiptV1{}, errors.New("submission lost"), nil)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "submission lost") {
		t.Fatalf("unobserved transaction lost bounded reconciliation: %v", err)
	}
}

func TestDarwinRetainedInspectionAndValidationAfterUpgrade(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	roots := userdirs.Roots{State: filepath.Join(root, "state")}
	artifacts := installer.RuntimeArtifactStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	retained := make([]installer.RuntimeArtifact, 0, 2)
	sources := make([]string, 0, 2)
	for _, version := range []string{"0.34.0", "0.34.1"} {
		source := filepath.Join(root, "cq-"+version)
		if out, err := exec.Command("go", "build", "-o", source, "-ldflags", "-X main.version="+version, ".").CombinedOutput(); err != nil {
			t.Fatalf("build fixture: %v %s", err, out)
		}
		artifact, err := artifacts.Stage(context.Background(), source)
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, artifact)
		sources = append(sources, source)
	}
	link := filepath.Join(root, "package-cq")
	if err := os.Symlink(sources[0], link); err != nil {
		t.Fatal(err)
	}
	ownership := installstate.Store{FS: fsutil.OSFileSystem{}, Roots: roots}
	record := installstate.Record{SchemaVersion: 1, Owner: installstate.OwnerHomebrew, Version: retained[0].Version, Executable: link, BinaryDigest: retained[0].SHA256, Services: []string{proxyAgentLabel, agentLabel}}
	plist := filepath.Join(root, "proxy.plist")
	writeInstalledHTTPValidationPlist(t, plist, proxyAgentLabel, retained[0].Path, "/tmp/proxy.log")
	current := link
	ops := installedHTTPValidationServiceOperations{executable: func() (string, error) { return current, nil }, plistPath: func(string) (string, error) { return plist, nil }, launchctlPrint: func(string) error { return nil }, retainedRuntime: func(cli, configured string) (installer.RuntimeArtifact, error) {
		return resolveDarwinRetainedServiceRuntime(context.Background(), roots, cli, configured)
	}}
	resolve := func(string) (installedHTTPValidationServiceBinding, error) {
		return resolveInstalledHTTPValidationServiceWithOperations(proxyAgentLabel, ops)
	}
	current = retained[0].Path
	bootstrap, err := resolve("")
	if err != nil || bootstrap.executableSHA256 != retained[0].SHA256 {
		t.Fatalf("bootstrap inspection before ownership publication: %+v %v", bootstrap, err)
	}
	current = link
	if _, err := resolve(""); err == nil {
		t.Fatal("unowned package CLI accepted during setup")
	}
	if err := ownership.Save(record); err != nil {
		t.Fatal(err)
	}
	initial, err := resolve("")
	if err != nil || initial.executableSHA256 != retained[0].SHA256 {
		t.Fatalf("first retained install unavailable: %+v %v", initial, err)
	}
	receipts := proxy.RuntimeUpgradeStore{FS: fsutil.OSFileSystem{}, Roots: roots}
	receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "compatible", Generation: 1, Previous: retained[0], Candidate: retained[1], ListenerIdentity: "tcp|127.0.0.1:29280", SupervisorPID: 42}
	for _, phase := range []string{"prepared", "waiting", "handoff", "verifying", "committed"} {
		receipt.Phase = phase
		if err := receipts.Save(receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sources[1], link); err != nil {
		t.Fatal(err)
	}
	record.Version = retained[1].Version
	record.BinaryDigest = retained[1].SHA256
	if err := ownership.Save(record); err != nil {
		t.Fatal(err)
	}
	upgraded, err := resolve("")
	if err != nil || upgraded.executableSHA256 != retained[1].SHA256 || upgraded.runtimeExecutable != retained[1].Path || upgraded.serviceSHA256 == initial.serviceSHA256 {
		t.Fatalf("compatible runtime unavailable from package CLI: %+v %v", upgraded, err)
	}
	requestStore := installedHTTPValidationRequestStore{fs: fsutil.OSFileSystem{}, path: filepath.Join(roots.State, "validation", "request.json"), now: time.Now, random: rand.Reader, resolveService: resolve}
	if err := createInstalledHTTPValidationRequest(requestStore, retained[1].Version); err != nil {
		t.Fatal(err)
	}
	current = retained[1].Path
	consumed, err := consumeInstalledHTTPValidationRequestWithIntent(requestStore, retained[1].Version)
	if err != nil || consumed == nil {
		t.Fatalf("selected runtime could not consume package-prepared validation: %v", err)
	}
	current = sources[0]
	if _, err := resolve(""); err == nil {
		t.Fatal("unowned stale package executable accepted")
	}
}

func TestDarwinRuntimeUpgradeStatusProbe(t *testing.T) {
	previous := installer.RuntimeArtifact{Path: "/fixture/previous/cq", SHA256: strings.Repeat("a", 64), Version: "0.34.0", ProtocolVersion: 1}
	candidate := installer.RuntimeArtifact{Path: "/fixture/candidate/cq", SHA256: strings.Repeat("b", 64), Version: "0.34.1", ProtocolVersion: 1}
	receipt := proxy.RuntimeUpgradeReceiptV1{SchemaVersion: 1, TransactionID: "status-probe", Generation: 1, Phase: "waiting", Previous: previous, Candidate: candidate, ListenerIdentity: "tcp|127.0.0.1:29280", SupervisorPID: 42}
	body, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	conflict := receipt
	conflict.TransactionID = "different-transaction"
	conflictingBody, err := json.Marshal(conflict)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		status         int
		body           []byte
		transportError error
		wantError      bool
	}{
		{name: "waiting", status: http.StatusOK, body: body},
		{name: "stopped", status: http.StatusConflict, wantError: true},
		{name: "transient", status: http.StatusServiceUnavailable},
		{name: "paused", transportError: context.DeadlineExceeded},
		{name: "unreadable", status: http.StatusOK, body: []byte("invalid")},
		{name: "conflicting", status: http.StatusOK, body: conflictingBody, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			submission, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:29280"+proxy.RuntimeUpgradePath, strings.NewReader("submission"))
			if err != nil {
				t.Fatal(err)
			}
			submission.Header.Set("Authorization", "Bearer fixture-local-token")
			submission.Header.Set("Content-Type", "application/json")
			client := testDoer(func(request *http.Request) (*http.Response, error) {
				deadline, bounded := request.Context().Deadline()
				if request.Method != http.MethodGet || request.URL.Path != proxy.RuntimeUpgradeStatusPath || request.URL.Query().Get("transaction_id") != receipt.TransactionID || request.Header.Get("Authorization") != submission.Header.Get("Authorization") || request.Body != nil || request.ContentLength != 0 || !request.Close || request.Context().Err() != nil || !bounded || time.Until(deadline) > time.Second {
					t.Fatal("status probe lost authentication, transaction or timeout boundary")
				}
				if tc.transportError != nil {
					return nil, tc.transportError
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(string(tc.body)))}, nil
			})
			err = probeDarwinServiceRuntimeUpgrade(submission, client, candidate, receipt.TransactionID)
			if (err != nil) != tc.wantError {
				t.Fatalf("probe error = %v, want error %t", err, tc.wantError)
			}
			if submission.Method != http.MethodPost || submission.Body == nil || submission.Header.Get("Content-Type") != "application/json" {
				t.Fatal("status probe changed submission")
			}
		})
	}
}
