//go:build windows

package desktop

import (
	"errors"

	"golang.org/x/sys/windows"
)

const desktopMutexName = "Local\\CloudLocalStore.Desktop"

func acquireSingleInstance() (func(), error) {
	name, err := windows.UTF16PtrFromString(desktopMutexName)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateMutex(nil, true, name)
	if handle == 0 {
		return nil, err
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return nil, ErrAlreadyRunning
	}
	return func() {
		_ = windows.ReleaseMutex(handle)
		_ = windows.CloseHandle(handle)
	}, nil
}
