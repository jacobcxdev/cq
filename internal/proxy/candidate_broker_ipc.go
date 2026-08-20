package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const candidateBrokerIPCMaxBytes = 24 << 20

type CandidateBrokerIPCKind string

const (
	CandidateBrokerIPCAcquire  CandidateBrokerIPCKind = "acquire"
	CandidateBrokerIPCExchange CandidateBrokerIPCKind = "exchange"
)

type CandidateBrokerIPCRequestV1 struct {
	SchemaVersion int                                   `json:"schema_version"`
	Kind          CandidateBrokerIPCKind                `json:"kind"`
	Acquire       *CandidateProviderCapabilityAcquireV1 `json:"acquire,omitempty"`
	Capability    *CandidateProviderRequestCapabilityV1 `json:"capability,omitempty"`
	Exchange      *CandidateProviderExchange            `json:"exchange,omitempty"`
}

func EncodeCandidateBrokerIPCRequest(request CandidateBrokerIPCRequestV1) ([]byte, error) {
	if err := validateCandidateBrokerIPCRequest(request); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(body) > candidateBrokerIPCMaxBytes {
		return nil, errors.New("candidate broker IPC request exceeds limit")
	}
	return body, nil
}

func DecodeCandidateBrokerIPCRequest(body []byte) (CandidateBrokerIPCRequestV1, error) {
	if len(body) == 0 || len(body) > candidateBrokerIPCMaxBytes {
		return CandidateBrokerIPCRequestV1{}, errors.New("candidate broker IPC request size invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request CandidateBrokerIPCRequestV1
	if err := decoder.Decode(&request); err != nil {
		return CandidateBrokerIPCRequestV1{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return CandidateBrokerIPCRequestV1{}, errors.New("candidate broker IPC request has trailing value")
		}
		return CandidateBrokerIPCRequestV1{}, err
	}
	if err := validateCandidateBrokerIPCRequest(request); err != nil {
		return CandidateBrokerIPCRequestV1{}, err
	}
	return request, nil
}

func validateCandidateBrokerIPCRequest(request CandidateBrokerIPCRequestV1) error {
	if request.SchemaVersion != 1 {
		return errors.New("candidate broker IPC schema invalid")
	}
	switch request.Kind {
	case CandidateBrokerIPCAcquire:
		if request.Acquire == nil || request.Capability != nil || request.Exchange != nil {
			return errors.New("candidate broker IPC acquire union invalid")
		}
	case CandidateBrokerIPCExchange:
		if request.Acquire != nil || request.Capability == nil || request.Exchange == nil {
			return errors.New("candidate broker IPC exchange union invalid")
		}
	default:
		return errors.New("candidate broker IPC kind invalid")
	}
	return nil
}
