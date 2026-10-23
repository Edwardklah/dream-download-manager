package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Windows-specific parts of the extras (extras.go).

// hidden: a console program started from this window program would flash a
// black console window; this stops that.
func hidden(cmd *exec.Cmd) *exec.Cmd {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}

var (
	user32                         = syscall.NewLazyDLL("user32.dll")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procSetSuspendState            = syscall.NewLazyDLL("powrprof.dll").NewProc("SetSuspendState")
	procCreateIconFromResourceEx   = user32.NewProc("CreateIconFromResourceEx")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
	procSendMessage                = user32.NewProc("SendMessageW")
)

// setWindowIcon changes the window's icon (its title bar and taskbar
// button) to a PNG. Windows builds an icon straight from PNG data
// (CreateIconFromResourceEx), and WM_SETICON gives it to the window.
// (Shortcuts and Explorer keep showing the program file's own icon.)
func setWindowIcon(w *application.WebviewWindow, png []byte) {
	hwnd := uintptr(w.NativeWindow())
	if hwnd == 0 || len(png) == 0 {
		return
	}
	const iconVersion = 0x00030000
	icon, _, _ := procCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&png[0])), uintptr(len(png)), 1, iconVersion, 0, 0, 0)
	if icon == 0 {
		return
	}
	const wmSetIcon, iconSmall, iconBig = 0x0080, 0, 1
	procSendMessage.Call(hwnd, wmSetIcon, iconSmall, icon)
	procSendMessage.Call(hwnd, wmSetIcon, iconBig, icon)
}

// clipboardSequence is Windows' clipboard change counter: it goes up every
// time something is copied. Reading it doesn't open the clipboard (opening
// it every second could get in the way of other programs).
func clipboardSequence() uint32 {
	n, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(n)
}

// suspendComputer puts the computer to sleep. (SetSuspendState(FALSE, ...):
// sleep, not hibernate.)
func suspendComputer() error {
	if r, _, err := procSetSuspendState.Call(0, 0, 0); r == 0 {
		return err
	}
	return nil
}

// shutDownComputer asks Windows to shut down, as the Start menu does.
func shutDownComputer() error {
	return hidden(exec.Command("shutdown", "/s", "/t", "0")).Run()
}

// Programs listed under this key start when you sign in to Windows.
const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "dlmanager"

func startsWithSystem() bool {
	return hidden(exec.Command("reg", "query", runKey, "/v", runValue)).Run() == nil
}

// setStartWithSystem adds or removes the app (started in the tray, with no
// window) from the programs Windows starts at sign-in.
func setStartWithSystem(on bool) error {
	if !on {
		hidden(exec.Command("reg", "delete", runKey, "/v", runValue, "/f")).Run() // fine if it wasn't there
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmdline := `"` + exe + `" --tray`
	return hidden(exec.Command("reg", "add", runKey, "/v", runValue, "/t", "REG_SZ", "/d", cmdline, "/f")).Run()
}

// clipboardText reads the clipboard's text directly from Windows, on
// whatever thread this goroutine is on (pinned to it while the clipboard
// is open, as Windows requires). Another program may hold the clipboard for
// a moment: then it tries again a few times.
func clipboardText(*application.App) (string, bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const cfUnicodeText = 13
	if ok, _, _ := procIsClipboardFormatAvailable.Call(cfUnicodeText); ok == 0 {
		return "", true // no text on the clipboard (a picture, files...)
	}
	opened := false
	for i := 0; i < 5 && !opened; i++ {
		r, _, _ := procOpenClipboard.Call(0)
		opened = r != 0
		if !opened {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !opened {
		return "", false
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", false
	}
	size, _, _ := procGlobalSize.Call(h)
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return "", false
	}
	defer procGlobalUnlock.Call(h)
	n := int(size / 2)
	if n > 1<<16 { // a download link is short; don't read a huge copied text
		n = 1 << 16
	}
	text := syscall.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(p)), n))
	return text, true
}

// browserPath finds a browser's program from the "App Paths" Windows keeps
// for installed programs (the same lookup the Run box uses).
func browserPath(browser string) string {
	exe := map[string]string{"chrome": "chrome.exe", "edge": "msedge.exe"}[browser]
	if exe == "" {
		return ""
	}
	for _, root := range []string{"HKCU", "HKLM"} {
		key := root + `\Software\Microsoft\Windows\CurrentVersion\App Paths\` + exe
		out, err := hidden(exec.Command("reg", "query", key, "/ve")).Output()
		if err != nil {
			continue
		}
		// The answer's last line: "    (Default)    REG_SZ    C:\...\chrome.exe"
		if i := strings.Index(string(out), "REG_SZ"); i >= 0 {
			path := strings.Trim(strings.TrimSpace(string(out[i+len("REG_SZ"):])), `"`)
			if path != "" && exists(path) {
				return path
			}
		}
	}
	return ""
}
