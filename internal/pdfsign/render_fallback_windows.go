//go:build windows

package pdfsign

import (
	"os/exec"
	"strconv"
	"syscall"
)

func setCmdHideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		killCmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
		killCmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: 0x08000000,
		}
		return killCmd.Run()
	}
}
