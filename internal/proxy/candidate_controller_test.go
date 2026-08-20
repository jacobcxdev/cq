package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestCandidateControllerExposesOnlyPublicRuntimeAuthority(t *testing.T) {
	controller, err := NewCandidateController(bytes.NewReader(bytes.Repeat([]byte{0x47}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	grant := controller.RuntimeGrant("run-a", StableObjectIdentity{Digest: "source"})
	body, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{controller.privateKey, []byte("bearer"), []byte("provider_socket"), []byte("authority_key")} {
		if len(forbidden) > 0 && bytes.Contains(body, forbidden) {
			t.Fatalf("runtime grant exposed forbidden authority: %s", body)
		}
	}
	if grant.ControllerKeyID == "" || grant.ControllerPublicKey == "" {
		t.Fatalf("grant = %#v", grant)
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(grant.ControllerPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("cq/candidate-validation-controller-key-id/v1\x00"))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(publicKey)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(publicKey)
	if want := hex.EncodeToString(hash.Sum(nil)); grant.ControllerKeyID != want {
		t.Fatalf("controller key ID = %q, want %q", grant.ControllerKeyID, want)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	for _, value := range controller.privateKey {
		if value != 0 {
			t.Fatal("controller private key was not zeroised")
		}
	}
}

func TestCandidateControllerCapabilityPlanHasSeventeenFixedSlots(t *testing.T) {
	plan := DefaultCandidateCapabilityPlan()
	if err := ValidateCandidateCapabilityPlan(plan); err != nil {
		t.Fatalf("default plan: %v", err)
	}
	if len(plan.Slots) != 17 || plan.Slots[0].Role != CandidateControllerSlot {
		t.Fatalf("plan = %#v", plan)
	}
	plan.Slots = plan.Slots[:16]
	if err := ValidateCandidateCapabilityPlan(plan); err == nil {
		t.Fatal("accepted incomplete plan")
	}
}
