//go:build windows

package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
	"github.com/jacobcxdev/cq/internal/fsutil"
	"golang.org/x/sys/windows"
)

const runtimeEgressPipePrefix = `\\.\pipe\cq-egress-`

func listenRuntimeEgress(string) (string, net.Listener, func(), error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return "", nil, nil, errors.New("resolve private egress owner failed")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, nil, errors.New("create private egress listener name failed")
	}
	endpoint := runtimeEgressPipePrefix + hex.EncodeToString(nonce[:])
	sid := user.User.Sid.String()
	listener, err := winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: "O:" + sid + "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)",
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		return "", nil, nil, errors.New("create private egress listener failed")
	}
	return endpoint, listener, func() {}, nil
}

func validRuntimeEgressEndpoint(_ fsutil.FileSystem, _, endpoint string) bool {
	suffix, ok := strings.CutPrefix(endpoint, runtimeEgressPipePrefix)
	if !ok || len(suffix) != 32 {
		return false
	}
	id, err := hex.DecodeString(suffix)
	return err == nil && hex.EncodeToString(id) == suffix
}

func dialRuntimeEgress(ctx context.Context, endpoint string) (net.Conn, error) {
	if !validRuntimeEgressEndpoint(nil, "", endpoint) {
		return nil, errors.New("invalid private egress endpoint")
	}
	return winio.DialPipeContext(ctx, endpoint)
}
