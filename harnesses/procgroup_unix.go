//go:build unix

package harnesses

import (
	"os/exec"
	"syscall"
)

// NewProcessGroup makes cmd the leader of its own process group, so
// KillProcessGroup also reaches the tools it spawns.
func NewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillProcessGroup sends sig to the process group led by pid.
func KillProcessGroup(pid int, sig syscall.Signal) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, sig)
}
