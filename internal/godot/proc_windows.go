//go:build windows

package godot

import (
	"os/exec"
	"strconv"
)

func setProcessGroup(cmd *exec.Cmd) {}

// killGroup на Windows убивает дерево процессов через taskkill.
// Мягкой остановки здесь нет: Godot не обрабатывает WM_CLOSE из консоли.
func killGroup(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
