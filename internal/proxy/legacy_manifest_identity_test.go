//go:build !windows

package proxy

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestLegacyCUManifestsRetainSignedRecordIdentity(t *testing.T) {
	for unit, expected := range map[string]string{
		"CU-0": "b913830014ab778e9584352a2ef0a039e01935f25e703976d180ff4fae5cc64c",
		"CU-1": "94efe9c4883a8094ba3fcbe140859d247517689c3d69b405eaae3f04e86e50c6",
		"CU-2": "7f3b2b35ffd98d7c89b5a42edd6501b7a0e083163299b04e6a68e535224dc23c",
	} {
		data, err := legacyCUManifestV1(unit)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != expected {
			t.Fatalf("%s legacy manifest changed: got %s, want %s", unit, got, expected)
		}
	}
}
