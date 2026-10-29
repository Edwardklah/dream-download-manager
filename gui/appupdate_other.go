//go:build !windows

package main

import "errors"

// On Linux updates come as a new .deb or .rpm package, from the release
// page: the notice links to it.
func canInstallUpdates() bool { return false }

func runSetup(string) error { return errors.New("updates are installed with the new package") }
