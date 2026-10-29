package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// canInstallUpdates: was this copy installed by the setup? (A build run
// from the project folder isn't: it only gets the "What's new" link.)
func canInstallUpdates() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(filepath.Dir(exe), "uninstall.exe"))
	return err == nil
}

// runSetup starts the new version's setup, silently, as the same kind of
// install as this one.
//
// The setup asks Windows for administrator rights when the user has them
// (so it can offer "for everyone"), and a program can't simply start a
// program that asks for that: Windows refuses with "The requested operation
// requires elevation". So:
//   - installed just for this user: the setup is started without asking
//     for them (__COMPAT_LAYER=RunAsInvoker). It doesn't need them, and
//     there's no Windows prompt;
//   - installed for everyone (Program Files): it's started the way
//     Explorer starts programs (ShellExecute "runas"), and Windows asks for
//     permission once.
func runSetup(path string) error {
	if allUsersInstall() {
		return shellRunAs(path, "/S /AllUsers /RELAUNCH")
	}
	cmd := exec.Command(path, "/S", "/CurrentUser", "/RELAUNCH")
	cmd.Env = append(os.Environ(), "__COMPAT_LAYER=RunAsInvoker")
	return cmd.Start()
}

// allUsersInstall: is this copy in Program Files?
func allUsersInstall() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if pf != "" && strings.HasPrefix(strings.ToLower(exe), strings.ToLower(pf)+`\`) {
			return true
		}
	}
	return false
}

var procShellExecuteW = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")

// shellRunAs starts path with administrator rights (Windows asks first).
func shellRunAs(path, params string) error {
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(path)
	args, _ := syscall.UTF16PtrFromString(params)
	const swShowNormal = 1
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(args)), 0, swShowNormal)
	if r <= 32 { // ShellExecute's errors are the small numbers
		if r == 5 || r == 1223 {
			return fmt.Errorf("the update needs your permission to install: try again and choose Yes")
		}
		return fmt.Errorf("couldn't start the update (error %d)", r)
	}
	return nil
}
