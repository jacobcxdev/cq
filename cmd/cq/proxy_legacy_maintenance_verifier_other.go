//go:build !unix

package main

import (
	"context"

	codexprov "github.com/jacobcxdev/cq/internal/provider/codex"
	"github.com/jacobcxdev/cq/internal/proxy"
)

type proxyLegacyMaintenanceFinaliseVerifier struct{}

func newProxyLegacyMaintenanceFinaliseVerifier(string, string, int, *proxy.ServingAttestor) *proxyLegacyMaintenanceFinaliseVerifier {
	return &proxyLegacyMaintenanceFinaliseVerifier{}
}

func (*proxyLegacyMaintenanceFinaliseVerifier) bind(*proxy.CodexRoutingRuntime, *proxy.HeadroomBridge, proxy.HeadroomMode) error {
	return nil
}

func (*proxyLegacyMaintenanceFinaliseVerifier) AcquireLegacyMaintenanceFinalise(context.Context, codexprov.LegacyMaintenanceFinaliseVerification) (codexprov.LegacyMaintenanceFinaliseLease, error) {
	return nil, codexprov.ErrCredentialControlDisabled
}
