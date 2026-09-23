package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/jacobcxdev/cq/internal/fsutil"
	"github.com/jacobcxdev/cq/internal/proxy"
)

const candidateCheckpointLimit = 256 << 10

type candidateRemovalCheckpoint struct {
	Version  int      `json:"version"`
	Kind     string   `json:"kind"`
	Root     string   `json:"root"`
	Device   uint64   `json:"device"`
	Inode    uint64   `json:"inode"`
	FileID   [16]byte `json:"file_id"`
	State    []byte   `json:"state"`
	Key      []byte   `json:"key"`
	Registry []byte   `json:"registry"`
	MAC      string   `json:"mac"`
}

func candidateCheckpointNames(root string) (string, string) {
	digest := sha256.Sum256([]byte(root))
	name := ".cq-candidate-remove-" + hex.EncodeToString(digest[:]) + ".checkpoint"
	return name, name + ".final"
}
func candidateCheckpointMAC(c candidateRemovalCheckpoint) (string, error) {
	c.MAC = ""
	body, err := proxy.CanonicalJSONV1(c)
	if err != nil {
		return "", err
	}
	defer zeroCandidateBytes(body)
	mac := hmac.New(sha256.New, c.Key)
	_, _ = mac.Write([]byte("cq/candidate-removal-checkpoint/v1\x00"))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
func publishCandidateRemovalCheckpoint(ctx context.Context, fsys fsutil.FileSystem, root string, state proxy.CandidateLifecycleStateV1, identity fsutil.SecureFileIdentity, parent fsutil.SecureDirectory) (fsutil.SecureFileIdentity, error) {
	var zero fsutil.SecureFileIdentity
	c := candidateRemovalCheckpoint{Version: 1, Kind: "candidate_removal_checkpoint_v1", Root: root, Device: identity.Device, Inode: identity.Inode, FileID: identity.FileID}
	var err error
	c.Key, err = readCandidateInputFile(fsys, filepath.Join(root, "candidate.key"), 32)
	if err != nil {
		return zero, err
	}
	defer zeroCandidateBytes(c.Key)
	c.State, err = readCandidateInputFile(fsys, filepath.Join(root, "candidate.json"), 64<<10)
	if err != nil {
		return zero, err
	}
	c.Registry, err = readCandidateInputFile(fsys, filepath.Join(root, "client-sender-registry.json"), 64<<10)
	if err != nil {
		return zero, err
	}
	authenticated, err := proxy.InspectCandidateLifecycleBytes(c.State, c.Key, c.Registry)
	if err != nil {
		return zero, err
	}
	if authenticated != state {
		return zero, proxy.ErrCandidateLifecycleInvalid
	}
	c.MAC, err = candidateCheckpointMAC(c)
	if err != nil {
		return zero, err
	}
	body, err := proxy.CanonicalJSONV1(c)
	if err != nil {
		return zero, err
	}
	defer zeroCandidateBytes(body)
	if len(body) > candidateCheckpointLimit {
		return zero, proxy.ErrCandidateLifecycleInvalid
	}
	inspector := fsys.(fsutil.SecurePathInspector)
	name, final := candidateCheckpointNames(root)
	if f, e := parent.OpenNoFollow(final); e == nil {
		_ = f.Close()
		return zero, proxy.ErrCandidateLifecycleInvalid
	} else if !errors.Is(e, os.ErrNotExist) {
		return zero, e
	}
	if err = ctx.Err(); err != nil {
		return zero, err
	}
	if err = fsutil.SecureAtomicCreateInOwnerControlledDirectory(inspector, parent, filepath.Dir(root), name, body, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := inspector.Lstat(root)
		if err != nil {
			return err
		}
		now, ok := inspector.FileIdentity(info)
		if !ok || !candidateSameDirectoryIdentity(identity, now) {
			return fsutil.ErrUnsafeSecurePath
		}
		return fsutil.ValidateSecureDirectory(fsys, root)
	}); err != nil {
		return zero, err
	}
	retained, id, err := fsutil.ReadOwnerControlledFileInDirectoryWithIdentity(inspector, parent, filepath.Dir(root), name, candidateCheckpointLimit)
	defer zeroCandidateBytes(retained)
	if err == nil && !hmac.Equal(retained, body) {
		return zero, proxy.ErrCandidateLifecycleInvalid
	}
	return id, err
}

// Discovery checks only two deterministic leaves. It never repairs or scans
// siblings, and a conflicting replacement root cannot inherit old authority.
func inspectCandidateRemovalCheckpoint(ctx context.Context, fsys fsutil.FileSystem, root string) (proxy.CandidateLifecycleStateV1, bool, string, error) {
	var zero proxy.CandidateLifecycleStateV1
	if err := ctx.Err(); err != nil {
		return zero, false, "", err
	}
	inspector, ok := fsys.(fsutil.SecurePathInspector)
	if !ok {
		return zero, false, "", fsutil.ErrSecureCapabilityUnavailable
	}
	parent, err := fsutil.OpenOwnerControlledDirectory(fsys, filepath.Dir(root))
	if err != nil {
		return zero, false, "", err
	}
	defer parent.Close()
	name, final := candidateCheckpointNames(root)
	var body []byte
	for _, leaf := range []string{name, final} {
		if err := fsutil.ValidateSecureRegularFile(fsys, filepath.Join(filepath.Dir(root), leaf)); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return zero, false, "", err
		}
		data, _, err := fsutil.ReadOwnerControlledFileInDirectoryWithIdentity(inspector, parent, filepath.Dir(root), leaf, candidateCheckpointLimit)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			zeroCandidateBytes(data)
			return zero, false, "", err
		}
		if body != nil {
			zeroCandidateBytes(body)
			zeroCandidateBytes(data)
			return zero, false, "", proxy.ErrCandidateLifecycleInvalid
		}
		body = data
	}
	if body == nil {
		return zero, false, root, nil
	}
	defer zeroCandidateBytes(body)
	var c candidateRemovalCheckpoint
	if err = decodeCandidateCanonicalJSON(body, &c); err != nil {
		return zero, false, "", proxy.ErrCandidateLifecycleInvalid
	}
	defer zeroCandidateBytes(c.Key)
	if c.Version != 1 || c.Kind != "candidate_removal_checkpoint_v1" || c.Root != root || len(c.Key) != 32 {
		return zero, false, "", proxy.ErrCandidateLifecycleInvalid
	}
	want, err := candidateCheckpointMAC(c)
	if err != nil || !hmac.Equal([]byte(want), []byte(c.MAC)) {
		return zero, false, "", proxy.ErrCandidateLifecycleInvalid
	}
	state, err := proxy.InspectCandidateLifecycleBytes(c.State, c.Key, c.Registry)
	if err != nil || state.Phase != proxy.CandidatePhaseRemoved || state.PendingAction != "" {
		return zero, false, "", proxy.ErrCandidateLifecycleInvalid
	}
	expected := fsutil.SecureFileIdentity{Device: c.Device, Inode: c.Inode, FileID: c.FileID}
	tombstone := filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".removed-"+state.OperationID)
	receiptRoot := ""
	for _, path := range []string{root, tombstone} {
		info, err := inspector.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return zero, false, "", err
		}
		if err = fsutil.ValidateSecureDirectory(fsys, path); err != nil {
			return zero, false, "", err
		}
		identity, ok := inspector.FileIdentity(info)
		if !ok || !candidateSameDirectoryIdentity(expected, identity) || receiptRoot != "" {
			return zero, false, "", proxy.ErrCandidateLifecycleInvalid
		}
		receiptRoot = path
	}
	if err = ctx.Err(); err != nil {
		return zero, false, "", err
	}
	return state, true, receiptRoot, nil
}
