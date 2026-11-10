package main

import (
	"errors"
	"net"
	"testing"
)

func TestProxyStartReportsFinalStderrFlushFailure(t *testing.T) {
	originalStart := startProxyServiceStderrRetention
	originalAdopt := adoptProxyListenerFn
	t.Cleanup(func() {
		startProxyServiceStderrRetention = originalStart
		adoptProxyListenerFn = originalAdopt
	})
	flushFailure := errors.New("final stderr flush failed")
	startupFailure := errors.New("listener unavailable")
	stopped := false
	startProxyServiceStderrRetention = func(string) (func() error, error) {
		return func() error {
			stopped = true
			return flushFailure
		}, nil
	}
	adoptProxyListenerFn = func() (net.Listener, error) { return nil, startupFailure }
	err := runProxyStart(proxyCommandOptions{})
	if !stopped || !errors.Is(err, startupFailure) || !errors.Is(err, flushFailure) {
		t.Fatalf("proxy shutdown lost final flush failure: stopped=%t error=%v", stopped, err)
	}
}
