//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// Keep child processes (gh) from flashing a console window.
func hide(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
