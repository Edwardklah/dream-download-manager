package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var procSHFileOperationW = syscall.NewLazyDLL("shell32.dll").NewProc("SHFileOperationW")

// shFileOpStruct is Windows' SHFILEOPSTRUCTW.
type shFileOpStruct struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

// moveToTrash moves a file to the Recycle Bin (as Explorer's Delete does).
func moveToTrash(path string) error {
	const (
		foDelete          = 3
		fofSilent         = 0x0004
		fofNoConfirmation = 0x0010
		fofAllowUndo      = 0x0040 // to the Recycle Bin, not gone for good
		fofNoErrorUI      = 0x0400
	)
	// The list of paths ends with two NULs.
	from, err := syscall.UTF16FromString(path)
	if err != nil {
		return err
	}
	from = append(from, 0)
	op := shFileOpStruct{
		wFunc:  foDelete,
		pFrom:  &from[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI,
	}
	r, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		return fmt.Errorf("Windows error %#x (is the file open in another program?)", r)
	}
	if op.fAnyOperationsAborted != 0 {
		return fmt.Errorf("it was stopped")
	}
	return nil
}
