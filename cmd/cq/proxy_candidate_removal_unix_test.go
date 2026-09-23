//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type candidateRemovalFaultFS struct {
	fsutil.FileSystem
	fsutil.SecurePathInspector
	fsutil.NoFollowFileOpener
	fail    string
	hit     bool
	cancel  context.CancelFunc
	renames int
	before  func(string)
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
	return d.SecureDirectory.(fsutil.SecureDirectoryReader).ReadDir()
}
func (d *candidateRemovalFaultDirectory) RenameNoReplaceChecked(a, b string, id fsutil.SecureFileIdentity) error {
	d.fs.renames++
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
	return d.SecureDirectory.(fsutil.IdentityBoundRemover).RemoveChecked(n, id)
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
			state, err := proxy.InspectCandidateLifecycle(context.Background(), fsutil.OSFileSystem{}, a.InstanceStateRoot)
			if err != nil || state.Phase != proxy.CandidatePhaseRemoved || state.PendingAction != "" {
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
	if exit != 130 || !fs.hit || fs.renames != 1 {
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
