package proxy

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"sync"
)

type CandidateCapabilitySlotRole string

const (
	CandidateControllerSlot CandidateCapabilitySlotRole = "controller"
	CandidateRuntimeSlot    CandidateCapabilitySlotRole = "runtime"
)

type CandidateCapabilitySlotV1 struct {
	Index int                         `json:"index"`
	Role  CandidateCapabilitySlotRole `json:"role"`
}

type CandidateCapabilityPlanV1 struct {
	SchemaVersion int                         `json:"schema_version"`
	Slots         []CandidateCapabilitySlotV1 `json:"slots"`
}

type CandidateRuntimeGrantV1 struct {
	SchemaVersion       int                  `json:"schema_version"`
	RunID               string               `json:"run_id"`
	ControllerKeyID     string               `json:"controller_key_id"`
	ControllerPublicKey string               `json:"controller_public_key"`
	SourceIdentity      StableObjectIdentity `json:"source_identity"`
}

// CandidateController owns a process-private signing key. Only its public
// identity is copied into candidate runtime grants.
type CandidateController struct {
	mu         sync.Mutex
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	keyID      string
	closed     bool
}

func NewCandidateController(random io.Reader) (*CandidateController, error) {
	if random == nil {
		return nil, errors.New("candidate controller randomness unavailable")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(random)
	if err != nil {
		return nil, err
	}
	keyID, err := FramedSHA256Hex("cq/candidate-validation-controller-key-id/v1\x00", publicKey)
	if err != nil {
		for index := range privateKey {
			privateKey[index] = 0
		}
		return nil, err
	}
	return &CandidateController{privateKey: privateKey, publicKey: publicKey, keyID: keyID}, nil
}

func (c *CandidateController) RuntimeGrant(runID string, source StableObjectIdentity) CandidateRuntimeGrantV1 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || runID == "" {
		return CandidateRuntimeGrantV1{}
	}
	return CandidateRuntimeGrantV1{
		SchemaVersion:       1,
		RunID:               runID,
		ControllerKeyID:     c.keyID,
		ControllerPublicKey: base64.RawURLEncoding.EncodeToString(c.publicKey),
		SourceIdentity:      source,
	}
}

func (c *CandidateController) Sign(body []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("candidate controller closed")
	}
	return ed25519.Sign(c.privateKey, body), nil
}

func (c *CandidateController) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	for index := range c.privateKey {
		c.privateKey[index] = 0
	}
	c.closed = true
	return nil
}

func DefaultCandidateCapabilityPlan() CandidateCapabilityPlanV1 {
	plan := CandidateCapabilityPlanV1{SchemaVersion: 1, Slots: make([]CandidateCapabilitySlotV1, 17)}
	for index := range plan.Slots {
		role := CandidateRuntimeSlot
		if index == 0 {
			role = CandidateControllerSlot
		}
		plan.Slots[index] = CandidateCapabilitySlotV1{Index: index, Role: role}
	}
	return plan
}

func ValidateCandidateCapabilityPlan(plan CandidateCapabilityPlanV1) error {
	if plan.SchemaVersion != 1 || len(plan.Slots) != 17 {
		return errors.New("candidate capability plan must contain seventeen slots")
	}
	for index, slot := range plan.Slots {
		want := CandidateRuntimeSlot
		if index == 0 {
			want = CandidateControllerSlot
		}
		if slot.Index != index || slot.Role != want {
			return errors.New("invalid candidate capability slot")
		}
	}
	return nil
}
