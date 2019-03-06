// Copyright 2016 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package libkb

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modcryptsp   = windows.NewLazySystemDLL("cryptsp.dll")
	modcryptbase = windows.NewLazySystemDLL("cryptbase.dll")
)

const loadLibrarySearchSystem32 = 0x800

// SaferDLLLoading sets DLL load path to be safer.
func SaferDLLLoading() error {
	kernel32, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		return err
	}

	procSetDllDirectoryW, err := kernel32.FindProc("SetDllDirectoryW")
	if err != nil {
		return err
	}
	var zero uint16
	r1, _, e1 := syscall.Syscall(procSetDllDirectoryW.Addr(), 1,
		uintptr(unsafe.Pointer(&zero)), 0, 0)

	procSetDefaultDllDirectories, err := kernel32.FindProc("SetDefaultDllDirectories")
	if err == nil && procSetDefaultDllDirectories.Addr() != 0 {
		r1, _, e1 = syscall.Syscall(procSetDefaultDllDirectories.Addr(), 1,
			loadLibrarySearchSystem32, 0, 0)
		if r1 == 0 {
			err = e1
		}
	} else {
		err = errors.New("SetDefaultDllDirectories not found - please install KB2533623 for safer DLL loading")
	}

	// Attemt to load these from the system directory to thwart sideloading
	modcryptbase.Load()
	modcryptsp.Load()

	return err
}
