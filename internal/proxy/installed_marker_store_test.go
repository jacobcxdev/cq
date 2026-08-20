package proxy

import (
	"bytes"
	"context"
	"testing"

	"github.com/jacobcxdev/cq/internal/fsutil"
)

func TestInstalledMarkerStorePersistsPolicyAcknowledgement(t *testing.T) {
	fsys := fsutil.NewMemFS()
	if err := fsutil.EnsureSecureDirectory(fsys, "/markers"); err != nil {
		t.Fatal(err)
	}
	directory, err := fsys.OpenSecureDirectory("/markers")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	store, err := OpenInstalledMarkerStore(context.Background(), fsys, directory, NewAuthorityObjectPublisher(fsys, bytes.NewReader(bytes.Repeat([]byte{0x43}, 1024))), bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	marker := InstalledMarkerV1{SchemaVersion: 1, InstanceID: "candidate-a", Role: InstalledMarkerCandidate, PolicyDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", PolicyGeneration: 7, ControllerKeyID: "controller-key"}
	identity, err := store.Publish(marker)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if identity.Digest == "" {
		t.Fatal("empty marker identity")
	}
	reopened, err := OpenInstalledMarkerStore(context.Background(), fsys, directory, store.publisher, store.key[:])
	if err != nil {
		t.Fatal(err)
	}
	markers := reopened.Markers()
	if len(markers) != 1 || markers[0].PolicyGeneration != 7 {
		t.Fatalf("Markers = %#v", markers)
	}
}
