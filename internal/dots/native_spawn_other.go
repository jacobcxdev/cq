//go:build !darwin

package dots

import "os"

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
