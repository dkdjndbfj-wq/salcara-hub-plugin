//go:build !windows && !linux

package edgepool

import "os/exec"

func configureKernelProcess(cmd *exec.Cmd) {}

func startKernelProcess(cmd *exec.Cmd) (func(), error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {}, nil
}
