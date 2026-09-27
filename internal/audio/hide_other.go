//go:build !windows

package audio

import "os/exec"

// hideConsole is a no-op outside Windows.
func hideConsole(cmd *exec.Cmd) {}
