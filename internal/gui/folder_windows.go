//go:build windows

package gui

import (
	"fmt"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	coInitApartmentThreaded = 0x2
	bifReturnOnlyFileSystem = 0x0001
	bifNewDialogStyle       = 0x0040
)

// browseInfo is the Win32 BROWSEINFOW structure. Keep its fields in the
// native order so SHBrowseForFolderW can consume it without cgo.
type browseInfo struct {
	hwndOwner      windows.HWND
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

var (
	shell32                  = windows.NewLazySystemDLL("shell32.dll")
	ole32                    = windows.NewLazySystemDLL("ole32.dll")
	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
)

func selectFolder() (string, error) {
	// Shell folder dialogs need an apartment. Locking this goroutine to its OS
	// thread keeps CoInitializeEx/CoUninitialize paired on the same thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	initialized := false
	if err := windows.CoInitializeEx(0, coInitApartmentThreaded); err != nil {
		// A thread already initialized for another apartment can still invoke
		// this shell API; it owns COM initialization and must not be uninitialized
		// by us in that case.
		if err != syscall.Errno(windows.RPC_E_CHANGED_MODE) {
			return "", fmt.Errorf("initialize shell folder selector: %w", err)
		}
	} else {
		initialized = true
	}
	if initialized {
		defer windows.CoUninitialize()
	}

	title := windows.StringToUTF16Ptr("选择同步目录")
	displayName := make([]uint16, windows.MAX_PATH)
	info := browseInfo{
		hwndOwner:      windows.GetForegroundWindow(),
		pszDisplayName: &displayName[0],
		lpszTitle:      title,
		ulFlags:        bifReturnOnlyFileSystem | bifNewDialogStyle,
	}
	pidl, _, callErr := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&info)))
	if pidl == 0 {
		// A null PIDL is the documented cancel result. Do not turn cancellation
		// into an HTTP error or attempt to inspect the process last-error value.
		return "", nil
	}
	defer procCoTaskMemFree.Call(pidl)

	pathBuffer := make([]uint16, windows.MAX_PATH)
	pathResult, _, pathErr := procSHGetPathFromIDListW.Call(
		pidl,
		uintptr(unsafe.Pointer(&pathBuffer[0])),
	)
	if pathResult == 0 {
		if pathErr == nil {
			pathErr = callErr
		}
		if pathErr == nil {
			pathErr = syscall.EINVAL
		}
		return "", fmt.Errorf("resolve selected folder: %w", pathErr)
	}
	selected := windows.UTF16ToString(pathBuffer)
	if selected == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(selected)
	if err != nil {
		return "", fmt.Errorf("resolve selected folder: %w", err)
	}
	return filepath.Clean(absolute), nil
}
