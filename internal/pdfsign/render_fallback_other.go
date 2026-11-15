//go:build !windows

package pdfsign

import "os/exec"

func setCmdHideWindow(cmd *exec.Cmd) {}
