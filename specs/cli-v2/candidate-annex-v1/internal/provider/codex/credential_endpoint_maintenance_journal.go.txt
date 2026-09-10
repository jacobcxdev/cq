package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const legacyCredentialEndpointTransitionVersion = 1

// Version 2 adds the required owner field used by reversible activation
// receipts. Version 1 records are deliberately rejected instead of being
// ambiguously reinterpreted under the stricter schema.
const credentialEndpointMaintenanceJournalVersion = 2

type credentialEndpointMaintenanceJournal struct {
	Version    int                                      `json:"version"`
	Generation uint64                                   `json:"generation"`
	State      CredentialEndpointMaintenanceState       `json:"state"`
	TicketHash string                                   `json:"ticket_hash"`
	Ticket     LegacyCredentialEndpointTransitionTicket `json:"ticket"`
	Owner      *credentialEndpointMaintenanceOwnerProof `json:"owner"`
}

type credentialEndpointMaintenanceOwnerProof struct {
	Generation    string                           `json:"generation"`
	Socket        LegacyCredentialEndpointIdentity `json:"socket"`
	SidecarFile   LegacyCredentialEndpointIdentity `json:"sidecar_file"`
	SidecarSHA256 string                           `json:"sidecar_sha256"`
}

func ParseLegacyCredentialEndpointTransitionTicket(data []byte) (LegacyCredentialEndpointTransitionTicket, error) {
	if len(data) == 0 || len(data) > legacyCredentialEndpointProofMaxBytes {
		return LegacyCredentialEndpointTransitionTicket{}, errors.New("invalid credential endpoint maintenance ticket size")
	}
	if err := validateCredentialEndpointMaintenanceTicketJSON(data); err != nil {
		return LegacyCredentialEndpointTransitionTicket{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var ticket LegacyCredentialEndpointTransitionTicket
	if err := decoder.Decode(&ticket); err != nil {
		return LegacyCredentialEndpointTransitionTicket{}, fmt.Errorf("parse credential endpoint maintenance ticket: %w", err)
	}
	if err := requireCredentialMaintenanceEOF(decoder); err != nil {
		return LegacyCredentialEndpointTransitionTicket{}, err
	}
	if err := ticket.validate(); err != nil {
		return LegacyCredentialEndpointTransitionTicket{}, err
	}
	return ticket, nil
}

func validateCredentialEndpointMaintenanceTicketJSON(data []byte) error {
	fields, err := decodeCredentialMaintenanceObject(data)
	if err != nil {
		return fmt.Errorf("parse credential endpoint maintenance ticket: %w", err)
	}
	if err := requireCredentialMaintenanceFields(fields, "version", "id", "path", "directory", "socket", "lock", "quarantine_name"); err != nil {
		return err
	}
	for _, name := range []string{"directory", "socket", "lock"} {
		identityFields, err := decodeCredentialMaintenanceObject(fields[name])
		if err != nil {
			return fmt.Errorf("parse credential endpoint maintenance ticket %s: %w", name, err)
		}
		if err := requireCredentialMaintenanceFields(identityFields, "device", "inode", "uid", "links", "type", "mode"); err != nil {
			return fmt.Errorf("parse credential endpoint maintenance ticket %s: %w", name, err)
		}
	}
	return nil
}

func (ticket LegacyCredentialEndpointTransitionTicket) validate() error {
	if ticket.Version != legacyCredentialEndpointTransitionVersion || len(ticket.ID) != 32 {
		return ErrCredentialEndpointMaintenanceTicketMismatch
	}
	decodedID, err := hex.DecodeString(ticket.ID)
	if err != nil || len(decodedID) != 16 || strings.ToLower(ticket.ID) != ticket.ID {
		return ErrCredentialEndpointMaintenanceTicketMismatch
	}
	if ticket.Path == "" || !filepath.IsAbs(ticket.Path) || filepath.Clean(ticket.Path) != ticket.Path {
		return ErrCredentialEndpointMaintenanceTicketMismatch
	}
	if err := ticket.Directory.validate("directory", 0o700, false); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	if err := ticket.Socket.validate("socket", 0o600, true); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	if err := ticket.Lock.validate("regular", 0o600, true); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	wantQuarantine := "." + filepath.Base(ticket.Path) + ".legacy-" + ticket.ID + ".quarantine"
	if ticket.QuarantineName != wantQuarantine || filepath.Base(ticket.QuarantineName) != ticket.QuarantineName {
		return ErrCredentialEndpointMaintenanceTicketMismatch
	}
	return nil
}

func credentialEndpointMaintenanceTicketHash(ticket LegacyCredentialEndpointTransitionTicket) (string, error) {
	if err := ticket.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(ticket)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func encodeCredentialEndpointMaintenanceJournal(journal credentialEndpointMaintenanceJournal) ([]byte, error) {
	if err := journal.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(journal)
}

func decodeCredentialEndpointMaintenanceJournal(data []byte) (credentialEndpointMaintenanceJournal, error) {
	if len(data) == 0 || len(data) > legacyCredentialEndpointProofMaxBytes {
		return credentialEndpointMaintenanceJournal{}, errors.New("invalid credential endpoint maintenance journal size")
	}
	fields, err := decodeCredentialMaintenanceObject(data)
	if err != nil {
		return credentialEndpointMaintenanceJournal{}, fmt.Errorf("parse credential endpoint maintenance journal: %w", err)
	}
	if err := requireCredentialMaintenanceFields(fields, "version", "generation", "state", "ticket_hash", "ticket", "owner"); err != nil {
		return credentialEndpointMaintenanceJournal{}, err
	}
	if err := validateCredentialEndpointMaintenanceTicketJSON(fields["ticket"]); err != nil {
		return credentialEndpointMaintenanceJournal{}, err
	}
	if !bytes.Equal(bytes.TrimSpace(fields["owner"]), []byte("null")) {
		ownerFields, err := decodeCredentialMaintenanceObject(fields["owner"])
		if err != nil {
			return credentialEndpointMaintenanceJournal{}, fmt.Errorf("parse credential endpoint maintenance owner proof: %w", err)
		}
		if err := requireCredentialMaintenanceFields(ownerFields, "generation", "socket", "sidecar_file", "sidecar_sha256"); err != nil {
			return credentialEndpointMaintenanceJournal{}, fmt.Errorf("parse credential endpoint maintenance owner proof: %w", err)
		}
		for _, name := range []string{"socket", "sidecar_file"} {
			identityFields, err := decodeCredentialMaintenanceObject(ownerFields[name])
			if err != nil {
				return credentialEndpointMaintenanceJournal{}, fmt.Errorf("parse credential endpoint maintenance owner %s proof: %w", name, err)
			}
			if err := requireCredentialMaintenanceFields(identityFields, "device", "inode", "uid", "links", "type", "mode"); err != nil {
				return credentialEndpointMaintenanceJournal{}, fmt.Errorf("parse credential endpoint maintenance owner %s proof: %w", name, err)
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var journal credentialEndpointMaintenanceJournal
	if err := decoder.Decode(&journal); err != nil {
		return credentialEndpointMaintenanceJournal{}, err
	}
	if err := requireCredentialMaintenanceEOF(decoder); err != nil {
		return credentialEndpointMaintenanceJournal{}, err
	}
	if err := journal.validate(); err != nil {
		return credentialEndpointMaintenanceJournal{}, err
	}
	return journal, nil
}

func (journal credentialEndpointMaintenanceJournal) validate() error {
	if journal.Version != credentialEndpointMaintenanceJournalVersion || journal.Generation == 0 {
		return ErrCredentialEndpointMaintenanceTicketMismatch
	}
	switch journal.State {
	case CredentialEndpointMaintenancePrepared,
		CredentialEndpointMaintenanceQuarantined,
		CredentialEndpointMaintenanceActivating,
		CredentialEndpointMaintenanceActivated,
		CredentialEndpointMaintenanceFinalising,
		CredentialEndpointMaintenanceCommitting,
		CredentialEndpointMaintenanceRollingBack,
		CredentialEndpointMaintenanceRolledBack:
	default:
		return errors.New("invalid credential endpoint maintenance state")
	}
	if journal.Owner != nil {
		if err := journal.Owner.validate(); err != nil {
			return err
		}
		switch journal.State {
		case CredentialEndpointMaintenanceActivated,
			CredentialEndpointMaintenanceFinalising,
			CredentialEndpointMaintenanceRollingBack:
		default:
			return errors.New("credential endpoint maintenance owner proof is invalid for state")
		}
	} else if journal.State == CredentialEndpointMaintenanceFinalising {
		return errors.New("credential endpoint finalising state requires owner proof")
	}
	wantHash, err := credentialEndpointMaintenanceTicketHash(journal.Ticket)
	if err != nil || journal.TicketHash != wantHash {
		return errors.Join(ErrCredentialEndpointMaintenanceTicketMismatch, err)
	}
	return nil
}

func (proof credentialEndpointMaintenanceOwnerProof) validate() error {
	if !validCredentialEndpointMaintenanceHex(proof.Generation, 16) || !validCredentialEndpointMaintenanceHex(proof.SidecarSHA256, sha256.Size) {
		return errors.New("invalid credential endpoint maintenance owner digest")
	}
	if err := proof.Socket.validate("socket", 0o600, true); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	if err := proof.SidecarFile.validate("regular", 0o600, true); err != nil {
		return errors.Join(ErrCredentialEndpointMaintenanceConflict, err)
	}
	return nil
}

func credentialEndpointMaintenanceJournalsEqual(left, right credentialEndpointMaintenanceJournal) bool {
	if left.Version != right.Version || left.Generation != right.Generation || left.State != right.State ||
		left.TicketHash != right.TicketHash || left.Ticket != right.Ticket {
		return false
	}
	if left.Owner == nil || right.Owner == nil {
		return left.Owner == nil && right.Owner == nil
	}
	return *left.Owner == *right.Owner
}

func validCredentialEndpointMaintenanceHex(value string, bytes int) bool {
	if len(value) != bytes*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == bytes
}
