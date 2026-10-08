//go:build !unix

package harnesses

import (
	"os"
	"os/exec"
	"syscall"
)

// Seats run on Linux. These fallbacks only let the harness packages compile
// elsewhere (the Terraform provider imports the harness registry).

// NewProcessGroup does nothing: process groups are a Unix feature.
func NewProcessGroup(*exec.Cmd) {}

// KillProcessGroup kills pid itself; the signal cannot be delivered.
func KillProcessGroup(pid int, _ syscall.Signal) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
