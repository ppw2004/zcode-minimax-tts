package audio

import (
	"os/exec"
	"syscall"
)

// hideConsole prevents a console window from popping up when this package
// spawns a player subprocess (powershell on Windows). Without it, a
// GUI-subsystem parent (ttsd is built with -H windowsgui) causes every
// console child to allocate its own visible console window.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
