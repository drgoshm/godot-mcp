//go:build !windows

package godot

import (
	"os/exec"
	"syscall"
)

// setProcessGroup запускает Godot в собственной группе процессов, чтобы
// остановка убивала и всё, что он породил (OS.execute, OS.create_process).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup шлёт SIGTERM (или SIGKILL при force) всей группе процессов.
func killGroup(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	// Отрицательный pid = вся группа.
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		return cmd.Process.Signal(sig)
	}
	return nil
}
