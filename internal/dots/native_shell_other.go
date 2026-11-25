//go:build !windows

package dots

func nativeAppPresentation(*Settings, int, []string) func() { return func() {} }
