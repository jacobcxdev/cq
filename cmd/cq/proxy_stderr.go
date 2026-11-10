package main

// Platforms with service-owned file stderr may bound that file. Journald and
// foreground terminals keep their existing logging owner.
var startProxyServiceStderrRetention = func(string) (func() error, error) {
	return func() error { return nil }, nil
}
