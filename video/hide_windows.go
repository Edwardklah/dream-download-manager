package video

import (
	"os/exec"
	"syscall"
)

// hideWindow stops a console window flashing up: yt-dlp is a console
// program, and Windows gives each one a window unless told not to.
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
