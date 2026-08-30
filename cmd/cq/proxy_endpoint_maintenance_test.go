package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/userdirs"
)

func TestReadOnlyLegacyEndpointInspectCommandBypassesCompatibilityEpoch(t *testing.T) {
	t.Parallel()
	if !isReadOnlyLegacyEndpointInspectCommand([]string{"proxy", "endpoint", "inspect-legacy"}) {
		t.Fatal("inspect command did not bypass compatibility initialisation")
	}
	for _, args := range [][]string{
		{"proxy", "endpoint", "transition-legacy", "prepare"},
		{"proxy", "start"},
		{"codex", "accounts"},
	} {
		if isReadOnlyLegacyEndpointInspectCommand(args) {
			t.Fatalf("mutating or ordinary command bypassed compatibility: %v", args)
		}
	}
}

func TestProxyEndpointInspectLegacyDoesNotChangeDirectoryInventory(t *testing.T) {
	t.Parallel()
	stateDir, path := createCLIRefusedLegacyEndpoint(t)
	before := readDirectoryInventory(t, filepath.Dir(path))
	var output bytes.Buffer
	err := runProxyEndpointWithDependencies(context.Background(), []string{"inspect-legacy"}, proxyEndpointMaintenanceDependencies{
		resolveRoots: func() (userdirs.Roots, error) { return userdirs.Roots{State: stateDir}, nil }, stdin: bytes.NewReader(nil), stdout: &output,
		stderr:     bytes.NewBuffer(nil),
		stdinIsTTY: func() bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	after := readDirectoryInventory(t, filepath.Dir(path))
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("inspect inventory changed: before=%v after=%v", before, after)
	}
	var snapshot codexprov.LegacyCredentialEndpointSnapshot
	if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Path != path || snapshot.State != codexprov.LegacyCredentialEndpointRefused {
		t.Fatalf("inspect output = %#v", snapshot)
	}
}

func TestProxyEndpointTransitionRequiresConfirmationAndKeepsRollbackUntilFinalise(t *testing.T) {
	t.Parallel()
	stateDir, path := createCLIRefusedLegacyEndpoint(t)
	snapshot, err := codexprov.InspectLegacyCredentialEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	snapshotData, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshotFile := filepath.Join(filepath.Dir(path), "snapshot.json")
	if err := os.WriteFile(snapshotFile, snapshotData, 0o600); err != nil {
		t.Fatal(err)
	}
	deps := proxyEndpointMaintenanceDependencies{
		resolveRoots: func() (userdirs.Roots, error) { return userdirs.Roots{State: stateDir}, nil }, stdin: bytes.NewReader(nil), stdout: &bytes.Buffer{},
		stderr:     bytes.NewBuffer(nil),
		stdinIsTTY: func() bool { return false },
	}
	if err := runProxyEndpointWithDependencies(context.Background(), []string{
		"transition-legacy", "prepare", "--snapshot-file", snapshotFile, "--non-interactive",
	}, deps); err == nil {
		t.Fatal("prepare without stopped-and-drained confirmation succeeded")
	}
	for _, absent := range []string{codexprov.DefaultCredentialControlPath(stateDir) + ".lock", codexprov.DefaultCredentialControlPath(stateDir) + ".maintenance.json"} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unconfirmed prepare created %s: %v", absent, err)
		}
	}

	var prepareOutput bytes.Buffer
	deps.stdout = &prepareOutput
	if err := runProxyEndpointWithDependencies(context.Background(), []string{
		"transition-legacy", "prepare", "--snapshot-file", snapshotFile,
		"--confirm-stopped-and-drained", "--non-interactive",
	}, deps); err != nil {
		t.Fatal(err)
	}
	var prepared codexprov.LegacyCredentialEndpointTransitionStatus
	if err := json.Unmarshal(prepareOutput.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.State != codexprov.CredentialEndpointMaintenanceQuarantined {
		t.Fatalf("prepare state = %q", prepared.State)
	}
	ticketData, err := json.Marshal(prepared.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	ticketFile := filepath.Join(filepath.Dir(path), "ticket.json")
	if err := os.WriteFile(ticketFile, ticketData, 0o600); err != nil {
		t.Fatal(err)
	}
	var activateOutput bytes.Buffer
	deps.stdout = &activateOutput
	if err := runProxyEndpointWithDependencies(context.Background(), []string{
		"transition-legacy", "activate", "--ticket-file", ticketFile,
		"--confirm-stopped-and-drained", "--non-interactive",
	}, deps); err != nil {
		t.Fatal(err)
	}
	var activated codexprov.LegacyCredentialEndpointTransitionStatus
	if err := json.Unmarshal(activateOutput.Bytes(), &activated); err != nil {
		t.Fatal(err)
	}
	if activated.State != codexprov.CredentialEndpointMaintenanceActivated {
		t.Fatalf("activate state = %q", activated.State)
	}
	if _, err := os.Lstat(path + ".lock"); err != nil {
		t.Fatalf("permanent lock missing: %v", err)
	}
	for _, absent := range []string{path, path + ".maintenance.json"} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("activated path %s error = %v, want absent", absent, err)
		}
	}
	for _, present := range []string{path + ".maintenance.rollback.json", filepath.Join(filepath.Dir(path), prepared.Ticket.QuarantineName)} {
		if _, err := os.Lstat(present); err != nil {
			t.Fatalf("activated rollback artifact %s missing: %v", present, err)
		}
	}

	beforeCommit := readDirectoryInventory(t, filepath.Dir(path))
	if err := runProxyEndpointWithDependencies(context.Background(), []string{
		"transition-legacy", "commit", "--ticket-file", ticketFile,
		"--confirm-stopped-and-drained", "--non-interactive",
	}, deps); err == nil {
		t.Fatal("deprecated commit action succeeded")
	}
	afterCommit := readDirectoryInventory(t, filepath.Dir(path))
	if !reflect.DeepEqual(beforeCommit, afterCommit) {
		t.Fatalf("deprecated commit changed namespace: before=%v after=%v", beforeCommit, afterCommit)
	}

	var rollbackOutput bytes.Buffer
	deps.stdout = &rollbackOutput
	if err := runProxyEndpointWithDependencies(context.Background(), []string{
		"transition-legacy", "rollback", "--ticket-file", ticketFile,
		"--confirm-stopped-and-drained", "--non-interactive",
	}, deps); err != nil {
		t.Fatal(err)
	}
	var rolledBack codexprov.LegacyCredentialEndpointTransitionStatus
	if err := json.Unmarshal(rollbackOutput.Bytes(), &rolledBack); err != nil {
		t.Fatal(err)
	}
	if rolledBack.State != codexprov.CredentialEndpointMaintenanceRolledBack {
		t.Fatalf("rollback state = %q", rolledBack.State)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("legacy socket was not restored: %v", err)
	}
	if _, err := os.Lstat(path + ".maintenance.rollback.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback receipt remains: %v", err)
	}
}

func TestProxyEndpointFinaliseRequiresHealthAuthorityNotDrainAuthority(t *testing.T) {
	t.Parallel()
	ticketFile := filepath.Join(t.TempDir(), "ticket.json")
	if err := os.WriteFile(ticketFile, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"finalise", "--ticket-file", ticketFile, "--non-interactive"},
		{"finalise", "--ticket-file", ticketFile, "--confirm-stopped-and-drained", "--non-interactive"},
		{"activate", "--ticket-file", ticketFile, "--confirm-candidate-healthy", "--non-interactive"},
		{"commit", "--ticket-file", ticketFile, "--confirm-stopped-and-drained", "--non-interactive"},
	} {
		if _, err := parseLegacyEndpointTransitionOptions(args); err == nil {
			t.Fatalf("unsafe options accepted: %v", args)
		}
	}
	opts, err := parseLegacyEndpointTransitionOptions([]string{
		"finalise", "--ticket-file", ticketFile, "--confirm-candidate-healthy", "--non-interactive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.confirmCandidateHealthy || opts.confirmStoppedAndDrained {
		t.Fatalf("finalise options = %#v", opts)
	}
}

func createCLIRefusedLegacyEndpoint(t *testing.T) (string, string) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "cq-endpoint-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	state := filepath.Join(home, ".config", "cq", "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	path := codexprov.DefaultCredentialControlPath(state)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return state, path
}

func readDirectoryInventory(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	inventory := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		inventory = append(inventory, entry.Name()+":"+info.Mode().String())
	}
	return inventory
}
