package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

type InstalledMarkerRole string

const (
	InstalledMarkerPrimary   InstalledMarkerRole = "primary"
	InstalledMarkerCandidate InstalledMarkerRole = "candidate"
)

type InstalledMarkerV1 struct {
	SchemaVersion    int                 `json:"schema_version"`
	InstanceID       string              `json:"instance_id"`
	Role             InstalledMarkerRole `json:"role"`
	PolicyDigest     string              `json:"policy_digest"`
	PolicyGeneration uint64              `json:"policy_generation"`
	ControllerKeyID  string              `json:"controller_key_id,omitempty"`
	MAC              string              `json:"mac,omitempty"`
}

type InstalledMarkerStore struct {
	mu        sync.Mutex
	ctx       context.Context
	inspector fsutil.SecurePathInspector
	directory fsutil.SecureDirectory
	publisher DurableObjectPublisher
	key       [32]byte
	markers   map[string]InstalledMarkerV1
}

func OpenInstalledMarkerStore(ctx context.Context, inspector fsutil.SecurePathInspector, directory fsutil.SecureDirectory, publisher DurableObjectPublisher, key []byte) (*InstalledMarkerStore, error) {
	if ctx == nil || inspector == nil || directory == nil || publisher == nil || len(key) != 32 {
		return nil, fsutil.ErrSecureCapabilityUnavailable
	}
	store := &InstalledMarkerStore{ctx: ctx, inspector: inspector, directory: directory, publisher: publisher, markers: make(map[string]InstalledMarkerV1)}
	copy(store.key[:], key)
	if err := store.reopen(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *InstalledMarkerStore) Publish(marker InstalledMarkerV1) (StableObjectIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	marker.MAC = ""
	if err := validateInstalledMarker(marker); err != nil {
		return StableObjectIdentity{}, err
	}
	if _, exists := s.markers[marker.InstanceID]; exists {
		return StableObjectIdentity{}, errors.New("installed marker already exists")
	}
	marker.MAC = installedMarkerMAC(marker, s.key[:])
	body, err := json.Marshal(marker)
	if err != nil {
		return StableObjectIdentity{}, err
	}
	digest := sha256.Sum256(body)
	name := "installed-marker-" + hex.EncodeToString(digest[:]) + ".json"
	identity, err := s.publisher.PublishImmutable(s.ctx, s.directory, name, body, fs.FileMode(0o600))
	if err != nil {
		return StableObjectIdentity{}, err
	}
	s.markers[marker.InstanceID] = marker
	return identity, nil
}

func (s *InstalledMarkerStore) Markers() []InstalledMarkerV1 {
	s.mu.Lock()
	defer s.mu.Unlock()
	markers := make([]InstalledMarkerV1, 0, len(s.markers))
	for _, marker := range s.markers {
		markers = append(markers, marker)
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].InstanceID < markers[j].InstanceID })
	return markers
}

func (s *InstalledMarkerStore) reopen() error {
	reader, ok := s.directory.(fsutil.SecureDirectoryReader)
	if !ok {
		return fsutil.ErrSecureCapabilityUnavailable
	}
	entries, err := reader.ReadDir()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "installed-marker-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, _, err := fsutil.ReadSecureFileInDirectoryWithIdentity(s.inspector, s.directory, entry.Name(), routingPolicyMaxBytes)
		if err != nil {
			return err
		}
		var marker InstalledMarkerV1
		if err := json.Unmarshal(body, &marker); err != nil {
			return err
		}
		if !hmac.Equal([]byte(marker.MAC), []byte(installedMarkerMAC(marker, s.key[:]))) {
			return errors.New("installed marker authentication failed")
		}
		if err := validateInstalledMarker(marker); err != nil {
			return err
		}
		if _, exists := s.markers[marker.InstanceID]; exists {
			return errors.New("duplicate installed marker")
		}
		s.markers[marker.InstanceID] = marker
	}
	return nil
}

func validateInstalledMarker(marker InstalledMarkerV1) error {
	if marker.SchemaVersion != 1 || marker.InstanceID == "" || validateAuthorityEntryName("marker-"+marker.InstanceID) != nil || !lowerHexDigest(marker.PolicyDigest) || marker.PolicyGeneration == 0 {
		return errors.New("invalid installed marker")
	}
	switch marker.Role {
	case InstalledMarkerPrimary:
		if marker.ControllerKeyID != "" {
			return errors.New("primary marker cannot claim candidate controller")
		}
	case InstalledMarkerCandidate:
		if marker.ControllerKeyID == "" {
			return errors.New("candidate marker controller unavailable")
		}
	default:
		return errors.New("invalid installed marker role")
	}
	return nil
}

func installedMarkerMAC(marker InstalledMarkerV1, key []byte) string {
	marker.MAC = ""
	body, _ := json.Marshal(marker)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("cq/installed-marker/v1\x00"))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
