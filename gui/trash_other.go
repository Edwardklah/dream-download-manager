//go:build !windows

package main

import (
	"os/exec"
)

// moveToTrash moves a file to the desktop's Trash (gio, which GNOME and
// most desktops have).
func moveToTrash(path string) error {
	gio, err := exec.LookPath("gio")
	if err != nil {
		return err
	}
	if out, err := exec.Command(gio, "trash", "--", path).CombinedOutput(); err != nil {
		return &trashError{string(out)}
	}
	return nil
}

type trashError struct{ msg string }

func (e *trashError) Error() string { return "the Trash refused it: " + e.msg }
