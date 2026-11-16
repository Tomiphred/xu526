//go:build !windows

package runtime

import (
	"os/exec"
	"syscall"
)

func groupAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// stopGroup asks the process group to exit (SIGTERM).
func stopGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGTERM) }

// killGroup ends the process group (SIGKILL).
func killGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

func hideWindow(*exec.Cmd) {}
