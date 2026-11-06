//go:build !darwin

package dots

import (
	"context"
	"errors"
	"net"
)

func nativeCloudGatewayPeer(context.Context, *Settings, string) (int, bool) {
	return 0, false
}

func cloudBridgeGateway(*Settings) (net.Listener, error) {
	return nil, errors.New("native gateway handoff requires macOS")
}
