package proxy

import (
	"context"
	"testing"
	"time"
)

func TestRunCodexHTTPInstalledAcceptance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := RunCodexHTTPInstalledAcceptance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Turns != 20 || result.Requests != 40 || result.SelectorCalls != 20 || result.ContinuityErrors != 0 || result.UnknownEvents != 0 {
		t.Fatalf("result = %#v", result)
	}
}
