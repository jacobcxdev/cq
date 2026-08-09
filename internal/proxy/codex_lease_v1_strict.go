package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
)

func decodeCodexLeaseV1StrictJSON(data []byte, destination *codexLeaseJournalEnvelope) error {
	if destination == nil {
		return errors.New("nil Codex lease v1 destination")
	}
	if err := decodeCodexLeaseStrictJSON(data, destination); err != nil {
		return err
	}
	original, err := decodeCodexLeaseJSONShape(data)
	if err != nil {
		return err
	}
	if codexLeaseJSONContainsNull(original) {
		return errors.New("Codex lease v1 JSON contains null")
	}
	canonical, err := json.Marshal(destination)
	if err != nil {
		return err
	}
	want, err := decodeCodexLeaseJSONShape(canonical)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(original, want) {
		return errors.New("Codex lease v1 JSON has non-canonical member presence")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return err
	}
	if !bytes.Equal(compact.Bytes(), canonical) {
		return errors.New("Codex lease v1 JSON has non-canonical encoding or member order")
	}
	return nil
}
