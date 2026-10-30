//go:build !darwin

package proxy

import (
	"errors"

	wgconn "golang.zx2c4.com/wireguard/conn"
)

func newInterfaceWireGuardBind(string) (wgconn.Bind, error) {
	return nil, errors.New("codex_egress_interface is supported only on macOS")
}
