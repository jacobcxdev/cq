//go:build linux

package proxy

import (
	"bytes"
	"testing"
)

func TestLinuxRelayMessageRejectsProtocolViolations(t *testing.T) {
	message := encodeLinuxRelayMessage(linuxRelayMessage{Kind: linuxRelayMessageReady, Sequence: 1})
	decoded, err := decodeLinuxRelayMessage(message)
	if err != nil || decoded.Kind != linuxRelayMessageReady || decoded.Sequence != 1 {
		t.Fatalf("ready decode = %#v, %v", decoded, err)
	}
	tests := [][]byte{
		nil,
		message[:len(message)-1],
		append(append([]byte(nil), message...), 0),
		bytes.Repeat([]byte{0xff}, len(message)),
	}
	for _, input := range tests {
		if _, err := decodeLinuxRelayMessage(input); err == nil {
			t.Fatalf("accepted invalid relay message %x", input)
		}
	}
}
