//go:build !linux

package main

import "os/exec"

// Elsewhere (a developer's machine) ffmpeg is simply killed with the server's own process handling.
func ownGroup(*exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, _ bool) error {
	if cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Kill()
	return nil
}
