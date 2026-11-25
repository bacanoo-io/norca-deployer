//go:build unix

package connector

import (
	"os/exec"
	"syscall"
)

// killGroup makes an abort kill the whole process group of a command: sh
// does not exec its last command as bash does, so killing only the shell
// would leave its children running with the output pipes open.
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
