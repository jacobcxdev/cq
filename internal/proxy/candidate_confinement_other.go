//go:build !darwin && !linux

package proxy

func candidatePlatformConfinementAvailable() bool { return false }
