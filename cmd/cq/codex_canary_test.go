package main

import "testing"

func TestRunCodexCanaryRejectsUnknownCommand(t *testing.T) {
	if err := runCodexCanary([]string{"unknown"}); err == nil {
		t.Fatal("expected unknown command error")
	}
}
