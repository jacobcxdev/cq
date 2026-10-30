//go:build !darwin

package proxy

import "testing"

func TestInterfaceWireGuardBindUnsupportedPlatform(t *testing.T) {
	if bind, err := newInterfaceWireGuardBind("en0"); bind != nil || err == nil {
		t.Fatalf("unsupported platform accepted interface binding: bind=%v err=%v", bind, err)
	}
}
