package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// install as this one: in Program Files it's for everyone (Windows asks
// for permission once), otherwise just for this user.
func runSetup(path string) error {
	mode := "/CurrentUser"
	if exe, err := os.Executable(); err == nil {
		for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
			if pf != "" && strings.HasPrefix(strings.ToLower(exe), strings.ToLower(pf)+`\`) {
				mode = "/AllUsers"
			}
		}
	}
	return exec.Command(path, "/S", mode, "/RELAUNCH").Start()
}
