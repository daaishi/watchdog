package main

// Windows logon autostart, registered under the current user's Run key.
//
// The Run key is used rather than a shortcut in shell:startup because it can be
// read, written and removed with a few registry calls — a .lnk would need COM —
// and because it is trivial to inspect on site with regedit.

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	advapi32             = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW  = advapi32.NewProc("RegDeleteValueW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

const (
	hkeyCurrentUser = 0x80000001

	keyQueryValue = 0x0001
	keySetValue   = 0x0002

	regSZ = 1

	errorSuccess       = 0
	errorFileNotFound  = 2
	runKeyPath         = `Software\Microsoft\Windows\CurrentVersion\Run`
	autoStartValueName = "Watchdog"
)

// openRunKey opens HKCU's Run key with the requested access.
func openRunKey(access uint32) (syscall.Handle, error) {
	pathPtr, err := syscall.UTF16PtrFromString(runKeyPath)
	if err != nil {
		return 0, err
	}
	var h syscall.Handle
	ret, _, _ := procRegOpenKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(pathPtr)),
		0,
		uintptr(access),
		uintptr(unsafe.Pointer(&h)),
	)
	if ret != errorSuccess {
		return 0, fmt.Errorf("open Run key: error %d", ret)
	}
	return h, nil
}

// autoStartCommand is the command line registered for logon start. The exe path
// is quoted so a path containing spaces still starts.
func autoStartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `"`, nil
}

// autoStartState reports whether this executable is registered to start at
// logon. It also returns the command line currently registered, which may point
// at a *different* copy of watchdog.exe — worth showing in the UI, because that
// is exactly the situation where two instances end up fighting.
func autoStartState() (enabled bool, registered string) {
	h, err := openRunKey(keyQueryValue)
	if err != nil {
		return false, ""
	}
	defer procRegCloseKey.Call(uintptr(h))

	namePtr, err := syscall.UTF16PtrFromString(autoStartValueName)
	if err != nil {
		return false, ""
	}
	buf := make([]uint16, 2048)
	size := uint32(len(buf) * 2) // bytes
	var valType uint32
	ret, _, _ := procRegQueryValueExW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(namePtr)),
		0,
		uintptr(unsafe.Pointer(&valType)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == errorFileNotFound {
		return false, ""
	}
	if ret != errorSuccess {
		return false, ""
	}
	registered = syscall.UTF16ToString(buf)
	want, err := autoStartCommand()
	if err != nil {
		return registered != "", registered
	}
	return strings.EqualFold(strings.TrimSpace(registered), want), registered
}

// setAutoStart registers or removes this executable in the logon Run key.
func setAutoStart(enable bool) error {
	h, err := openRunKey(keyQueryValue | keySetValue)
	if err != nil {
		return err
	}
	defer procRegCloseKey.Call(uintptr(h))

	namePtr, err := syscall.UTF16PtrFromString(autoStartValueName)
	if err != nil {
		return err
	}

	if !enable {
		ret, _, _ := procRegDeleteValueW.Call(uintptr(h), uintptr(unsafe.Pointer(namePtr)))
		if ret != errorSuccess && ret != errorFileNotFound {
			return fmt.Errorf("remove autostart: error %d", ret)
		}
		return nil
	}

	cmd, err := autoStartCommand()
	if err != nil {
		return err
	}
	val, err := syscall.UTF16FromString(cmd)
	if err != nil {
		return err
	}
	ret, _, _ := procRegSetValueExW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(namePtr)),
		0,
		regSZ,
		uintptr(unsafe.Pointer(&val[0])),
		uintptr(len(val)*2), // bytes, including the terminating NUL
	)
	if ret != errorSuccess {
		return fmt.Errorf("set autostart: error %d", ret)
	}
	return nil
}
