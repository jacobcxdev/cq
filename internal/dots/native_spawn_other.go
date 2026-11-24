//go:build !darwin && !windows

package dots

import (
	"os"
	"path/filepath"
	"slices"
)

func nativeAppExecutable(s *Settings) string {
	return filepath.Join(s.AppPath, "Contents", "MacOS", "ChatGPT")
}
func nativeAppCheck(s *Settings) error { return AppCheck(s) }
func nativeAppArchive(s *Settings) string {
	return filepath.Join(s.AppPath, "Contents", "Resources", "app.asar")
}
func nativePipeLaunchArgs(args []string) []string {
	return append(append([]string(nil), args...), "--remote-debugging-pipe")
}
func nativeDebugFlagsMatch(flags []string) bool {
	return slices.Equal(flags, []string{"--remote-debugging-pipe"})
}

func startNativeApp(executable string, args, env []string, input, output *os.File) (*os.Process, error) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	return os.StartProcess(executable, append([]string{executable}, args...), &os.ProcAttr{
		Env: env, Files: []*os.File{null, os.Stdout, os.Stderr, input, output},
	})
}
