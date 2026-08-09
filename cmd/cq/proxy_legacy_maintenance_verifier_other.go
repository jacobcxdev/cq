//go:build !unix

package main

import (
	"context"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type proxyLegacyMaintenanceFinaliseVerifier struct{}

func newProxyLegacyMaintenanceFinaliseVerifier(string, string, int) *proxyLegacyMaintenanceFinaliseVerifier {
	return &proxyLegacyMaintenanceFinaliseVerifier{}
}

func (*proxyLegacyMaintenanceFinaliseVerifier) bind(*proxy.CodexRoutingRuntime, bool, proxy.HeadroomMode) error {
	return nil
}

func (*proxyLegacyMaintenanceFinaliseVerifier) VerifyLegacyMaintenanceFinalise(context.Context, codexprov.LegacyMaintenanceFinaliseVerification) error {
	return codexprov.ErrCredentialControlDisabled
}
