//go:build darwin

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if err := ownership.Save(record); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(root, "proxy.plist")
	writeInstalledHTTPValidationPlist(t, plist, proxyAgentLabel, retained[0].Path, "/tmp/proxy.log")
	current := link
	ops := installedHTTPValidationServiceOperations{executable: func() (string, error) { return current, nil }, plistPath: func(string) (string, error) { return plist, nil }, launchctlPrint: func(string) error { return nil }, retainedRuntime: func(cli, configured string) (installer.RuntimeArtifact, error) {
		return resolveDarwinRetainedServiceRuntime(context.Background(), roots, cli, configured)
	}}
	resolve := func(string) (installedHTTPValidationServiceBinding, error) {
		return resolveInstalledHTTPValidationServiceWithOperations(proxyAgentLabel, ops)
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
