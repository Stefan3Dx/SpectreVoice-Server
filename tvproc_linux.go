package main

import (
	"os/exec"
	"syscall"
)

// ownGroup puts ffmpeg in a process group of its own (so it and anything it starts can be stopped together), and
// has the kernel kill it if the server dies: no ffmpeg is left playing for nobody.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// signalGroup asks the group to end (SIGTERM), or kills it.
func signalGroup(cmd *exec.Cmd, kill bool) error {
	if cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
