//go:build !windows

package dots

import "net/http"

func windowsNativeCloudGateway(*Probe, http.ResponseWriter, *http.Request) bool { return false }
