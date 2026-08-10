//go:build !darwin

package main

import "errors"

func resolveInstalledHTTPValidationService(string) (installedHTTPValidationServiceBinding, error) {
	return installedHTTPValidationServiceBinding{}, errors.New("installed HTTP validation is only supported on macOS")
}
