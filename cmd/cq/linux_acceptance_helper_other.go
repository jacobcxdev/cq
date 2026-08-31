//go:build !linux

package main

import "context"

func isLinuxAcceptanceHelperCommand([]string) bool { return false }

func runLinuxAcceptanceHelper(context.Context) error { return nil }
