package runtime

import (
	"os/exec"
	"strconv"
	"syscall"
)

// CREATE_NEW_PROCESS_GROUP: the app and what it starts form one group,
// apart from xu526's own console.
func groupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
}

// stopGroup asks the process tree to close. Windows has no SIGTERM;
// taskkill without /F sends a close request, which console programs often
// ignore, so Stop follows up with killGroup after its grace period.
func stopGroup(pid int) {
	cmd := exec.Command("taskkill", "/T", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

// killGroup ends the whole process tree.
func killGroup(pid int) {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

// hideWindow keeps a console program (podman) from flashing a window when
// the windowless service runs it.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
