//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type candidateRemovalFaultFS struct {
	fsutil.FileSystem
	fsutil.SecurePathInspector
	fsutil.NoFollowFileOpener
	fail        string
	hit         bool
	cancel      context.CancelFunc
	renames     int
	before      func(string)
	after       func(string)
	failRestore bool
	finalMode   string
	finalCalled bool
	failRead    string
}

func (f *candidateRemovalFaultFS) OpenSecureDirectory(path string) (fsutil.SecureDirectory, error) {
	d, e := (fsutil.OSFileSystem{}).OpenSecureDirectory(path)
	if e != nil {
		return nil, e
	}
	return &candidateRemovalFaultDirectory{SecureDirectory: d, fs: f}, nil
}
func (f *candidateRemovalFaultFS) OpenDurableDirectory(path string) (fsutil.DurableDirectory, error) {
	d, e := (fsutil.OSFileSystem{}).OpenDurableDirectory(path)
	if e != nil {
		return nil, e
	}
	return &candidateRemovalFaultDirectory{SecureDirectory: d.(fsutil.SecureDirectory), durable: d, fs: f}, nil
}

type candidateRemovalFaultDirectory struct {
	fsutil.SecureDirectory
	durable fsutil.DurableDirectory
	fs      *candidateRemovalFaultFS
}

func (d *candidateRemovalFaultDirectory) OpenDirectory(n string) (fsutil.DurableDirectory, error) {
	return d.durable.OpenDirectory(n)
}
func (d *candidateRemovalFaultDirectory) Mkdir(n string, p os.FileMode) error {
	return d.durable.Mkdir(n, p)
}
func (d *candidateRemovalFaultDirectory) ReadDir() ([]os.DirEntry, error) {
	if info, err := d.Stat(); err == nil && info.Name() == d.fs.failRead {
		d.fs.hit = true
		return nil, io.ErrUnexpectedEOF
	}
	return d.SecureDirectory.(fsutil.SecureDirectoryReader).ReadDir()
}
func (d *candidateRemovalFaultDirectory) RenameNoReplaceChecked(a, b string, id fsutil.SecureFileIdentity) error {
	d.fs.renames++
	if d.fs.failRestore && strings.Contains(a, ".removed-") {
		return io.ErrUnexpectedEOF
	}
	return d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameNoReplaceChecked(a, b, id)
}
func (d *candidateRemovalFaultDirectory) RemoveChecked(n string, id fsutil.SecureFileIdentity) error {
	if d.fs.before != nil {
		d.fs.before(n)
	}
	if !d.fs.hit && (n == d.fs.fail || d.fs.fail == "root" && strings.Contains(n, ".removed-")) {
		d.fs.hit = true
		if d.fs.cancel != nil {
			d.fs.cancel()
		}
		return io.ErrUnexpectedEOF
	}
	err := d.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(n, id)
	if err == nil && d.fs.after != nil {
		d.fs.after(n)
	}
	return err
}

func TestCLIV2CandidateRemovalFailureRestoresInspection(t *testing.T) {
	for _, fail := range []string{"payload", "candidate.key", "root"} {
		t.Run(fail, func(t *testing.T) {
			a, d := candidateV2Inputs(t)
			prepareCandidateV2Test(t, a, d)
			if err := os.WriteFile(filepath.Join(a.InstanceStateRoot, "payload"), []byte("ordinary contents"), 0o600); err != nil {
				t.Fatal(err)
			}
			fs := &candidateRemovalFaultFS{FileSystem: fsutil.OSFileSystem{}, SecurePathInspector: fsutil.OSFileSystem{}, NoFollowFileOpener: fsutil.OSFileSystem{}, fail: fail}
			fs.before = func(name string) {
				if name == "payload" {
					matches, _ := filepath.Glob(filepath.Join(filepath.Dir(a.InstanceStateRoot), ".*.removed-*"))
					if len(matches) != 1 {
						t.Fatalf("retired roots: %v", matches)
					}
					if _, err := proxy.InspectCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, matches[0]); err != nil {
						t.Fatalf("inspection removed before ordinary contents: %v", err)
					}
				}
			}
			d.FS = fs
			exit, out := runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}, &d)
			if exit != 1 || !fs.hit {
				t.Fatalf("failure=%d hit=%t %+v", exit, fs.hit, out)
			}
			state, found, _, err := inspectCandidateRemovalCheckpoint(context.Background(), fsutil.OSFileSystem{}, a.InstanceStateRoot)
			if err != nil || !found || state.Phase != proxy.CandidatePhaseRemoved || state.PendingAction != "" {
				t.Fatalf("inspection lost: %+v %v", state, err)
			}
			if fail != "payload" {
				if _, err = os.Stat(filepath.Join(a.InstanceStateRoot, "payload")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("ordinary contents not removed: %v", err)
				}
			}
		})
	}
}

func (d *candidateRemovalFaultDirectory) RenameChecked(a, b string, id fsutil.SecureFileIdentity) error {
	return d.SecureDirectory.(fsutil.IdentityBoundRenamer).RenameChecked(a, b, id)
}

func TestCLIV2CandidateRemovalCancellationDoesNotRestoreAfterDeadline(t *testing.T) {
	a, d := candidateV2Inputs(t)
	prepareCandidateV2Test(t, a, d)
	if err := os.WriteFile(filepath.Join(a.InstanceStateRoot, "payload"), []byte("ordinary contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &candidateRemovalFaultFS{FileSystem: fsutil.OSFileSystem{}, SecurePathInspector: fsutil.OSFileSystem{}, NoFollowFileOpener: fsutil.OSFileSystem{}, fail: "payload", cancel: cancel}
	d.FS = fs
	exit, out := runCandidateV2(t, ctx, []string{"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}, &d)
	if exit != 130 || !fs.hit || fs.renames != 2 {
		t.Fatalf("cancel=%d hit=%t renames=%d %+v", exit, fs.hit, fs.renames, out)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(a.InstanceStateRoot), ".*.removed-*"))
	if len(matches) != 1 {
		t.Fatalf("retired roots: %v", matches)
	}
	state, err := proxy.InspectCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, matches[0])
	if err != nil || state.Phase != proxy.CandidatePhaseRemoved {
		t.Fatalf("retired inspection: %+v %v", state, err)
	}
}

func TestCLIV2CandidateRemovalCheckpointSurvivesFinalFailures(t *testing.T) {
	for _, tc := range []struct {
		name, fail, after string
		restore           bool
	}{
		{"cancel_after_state", "", "candidate.json", false}, {"cancel_after_key", "", "candidate.key", false}, {"cancel_final_root", "root", "", false}, {"restore_failure", "candidate.key", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, d := candidateV2Inputs(t)
			prepareCandidateV2Test(t, a, d)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fs := &candidateRemovalFaultFS{FileSystem: fsutil.OSFileSystem{}, SecurePathInspector: fsutil.OSFileSystem{}, NoFollowFileOpener: fsutil.OSFileSystem{}, fail: tc.fail, failRestore: tc.restore}
			if tc.after != "" {
				fs.after = func(name string) {
					if name == tc.after {
						fs.hit = true
						cancel()
					}
				}
			} else if !tc.restore {
				fs.cancel = cancel
			}
			d.FS = fs
			exit, out := runCandidateV2(t, ctx, []string{"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}, &d)
			if exit == 0 || !fs.hit {
				t.Fatalf("failure=%d hit=%t %+v", exit, fs.hit, out)
			}
			fresh := d
			fresh.FS = fsutil.OSFileSystem{}
			exit, out = runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "status", "--state-dir", a.InstanceStateRoot, "--json"}, &fresh)
			if exit != 0 || decodeCandidateV2(t, out).Phase != proxy.CandidatePhaseRemoved {
				t.Fatalf("lost canonical inspection: exit=%d %+v", exit, out)
			}
		})
	}
}

func (d *candidateRemovalFaultDirectory) RemoveFinalFile(ctx context.Context, name, quarantine string, id fsutil.SecureFileIdentity) (bool, error) {
	d.fs.finalCalled = true
	if d.fs.finalMode == "quarantine_failure" || d.fs.finalMode == "quarantine_cancel" {
		// Inject the same deterministic namespace outcome as the native Unix
		// unlink-failure gate, independently exercised in fsutil.
		if err := d.SecureDirectory.RenameNoReplace(name, quarantine); err != nil {
			return false, err
		}
		if d.fs.finalMode == "quarantine_cancel" {
			d.fs.cancel()
		}
		return false, io.ErrUnexpectedEOF
	}
	committed, err := d.SecureDirectory.(fsutil.FinalFileRemover).RemoveFinalFile(ctx, name, quarantine, id)
	if committed && d.fs.finalMode == "postcommit_cancel" {
		d.fs.cancel()
	}
	return committed, err
}

func TestCLIV2CandidateRemovalFinalCommitAndDiscovery(t *testing.T) {
	for _, mode := range []string{"quarantine_failure", "quarantine_cancel", "postcommit_cancel"} {
		t.Run(mode, func(t *testing.T) {
			a, d := candidateV2Inputs(t)
			prepareCandidateV2Test(t, a, d)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fs := &candidateRemovalFaultFS{FileSystem: fsutil.OSFileSystem{}, SecurePathInspector: fsutil.OSFileSystem{}, NoFollowFileOpener: fsutil.OSFileSystem{}, finalMode: mode, cancel: cancel}
			d.FS = fs
			exit, out := runCandidateV2(t, ctx, []string{"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}, &d)
			if !fs.finalCalled {
				t.Fatalf("final commit not reached: %d %+v", exit, out)
			}
			fresh := d
			fresh.FS = fsutil.OSFileSystem{}
			statusExit, status := runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "status", "--state-dir", a.InstanceStateRoot, "--json"}, &fresh)
			if mode == "postcommit_cancel" {
				if exit != 130 || decodeCandidateV2(t, out).Phase != proxy.CandidatePhaseRemoved || statusExit != 3 {
					t.Fatalf("committed cancellation: %d %+v status=%d", exit, out, statusExit)
				}
				for _, e := range out.Errors {
					if e.Code == "candidate_timeout" {
						t.Fatal("invented pending timeout")
					}
				}
				return
			}
			if statusExit != 0 || decodeCandidateV2(t, status).Phase != proxy.CandidatePhaseRemoved {
				t.Fatalf("checkpoint status=%d %+v", statusExit, status)
			}
			if mode == "quarantine_cancel" && exit != 130 || mode == "quarantine_failure" && exit != 1 {
				t.Fatalf("exit=%d %+v", exit, out)
			}
			before := candidateTreeBytes(t, filepath.Dir(a.InstanceStateRoot))
			for _, args := range [][]string{candidatePrepareArgs(a), {"proxy", "candidate", "stop", "--state-dir", a.InstanceStateRoot, "--confirm-client-stopped", "--json"}, {"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}} {
				code, result := runCandidateV2(t, context.Background(), args, &fresh)
				if code != 6 {
					t.Fatalf("replay=%d %+v", code, result)
				}
			}
			if !reflect.DeepEqual(before, candidateTreeBytes(t, filepath.Dir(a.InstanceStateRoot))) {
				t.Fatal("inspection or replay changed checkpoint")
			}
		})
	}
}

func TestCLIV2CandidateRemovalCheckpointRejectsTamperAndReplacement(t *testing.T) {
	for _, change := range []string{"tamper", "duplicate", "replacement", "wrong_path", "symlink"} {
		t.Run(change, func(t *testing.T) {
			a, d := candidateV2Inputs(t)
			prepareCandidateV2Test(t, a, d)
			fs := &candidateRemovalFaultFS{FileSystem: fsutil.OSFileSystem{}, SecurePathInspector: fsutil.OSFileSystem{}, NoFollowFileOpener: fsutil.OSFileSystem{}, finalMode: "quarantine_failure"}
			d.FS = fs
			exit, _ := runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}, &d)
			if exit != 1 || !fs.finalCalled {
				t.Fatal("fixture did not retain checkpoint")
			}
			normal, final := candidateCheckpointNames(a.InstanceStateRoot)
			parent := filepath.Dir(a.InstanceStateRoot)
			path := filepath.Join(parent, final)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "tamper":
				body[len(body)/2] ^= 1
				if err = os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				if err = os.WriteFile(filepath.Join(parent, normal), body, 0o600); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				if err = os.Mkdir(a.InstanceStateRoot, 0o700); err != nil {
					t.Fatal(err)
				}
			case "wrong_path":
				other := filepath.Join(parent, "other")
				n, _ := candidateCheckpointNames(other)
				if err = os.Rename(path, filepath.Join(parent, n)); err != nil {
					t.Fatal(err)
				}
				a.InstanceStateRoot = other
			case "symlink":
				if err = os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			}
			fresh := d
			fresh.FS = fsutil.OSFileSystem{}
			exit, out := runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "status", "--state-dir", a.InstanceStateRoot, "--json"}, &fresh)
			if exit != 6 {
				t.Fatalf("accepted %s: %d %+v", change, exit, out)
			}
		})
	}
}

func TestCLIV2CandidateCheckpointSurvivingReceipt(t *testing.T) {
	a, d := candidateV2Inputs(t)
	prepareCandidateV2Test(t, a, d)
	attempt := strings.Repeat("a", 32)
	key := []byte(strings.Repeat("k", 32))
	receipt := proxy.CandidateReceiptInspectionV1{Found: true, AttemptID: attempt, Outcome: "published", ReceiptDigest: strings.Repeat("b", 64), PromotionDigest: strings.Repeat("c", 64)}
	body, err := proxy.CandidateReceiptStoredBytesV1(receipt, key)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(a.InstanceStateRoot, "receipt-export")
	if err = os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{"key": key, attempt + ".json": body} {
		if err = os.WriteFile(filepath.Join(root, name), value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fs := &candidateRemovalFaultFS{FileSystem: fsutil.OSFileSystem{}, SecurePathInspector: fsutil.OSFileSystem{}, NoFollowFileOpener: fsutil.OSFileSystem{}}
	fs.failRead = "receipt-export"
	d.FS = fs
	code, out := runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "remove", "--state-dir", a.InstanceStateRoot, "--confirm-candidate-state-loss", "--json"}, &d)
	if code != 1 || !fs.hit {
		t.Fatalf("removal=%d %+v", code, out)
	}

	fresh := d
	fresh.FS = fsutil.OSFileSystem{}
	code, out = runCandidateV2(t, context.Background(), []string{"proxy", "candidate", "receipt", "show", "--state-dir", a.InstanceStateRoot, "--attempt-id", attempt, "--json"}, &fresh)
	if code != 0 {
		t.Fatalf("surviving receipt=%d %+v", code, out)
	}
}
