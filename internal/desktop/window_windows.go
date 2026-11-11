//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	messageBoxWProc = user32.NewProc("MessageBoxW")
	loadIconWProc   = user32.NewProc("LoadIconW")
	setWindowProc   = user32.NewProc("SetWindowLongPtrW")
	callWindowProc  = user32.NewProc("CallWindowProcW")
	showWindowProc  = user32.NewProc("ShowWindow")
	setForeground   = user32.NewProc("SetForegroundWindow")
	getCursorPos    = user32.NewProc("GetCursorPos")
	createMenu      = user32.NewProc("CreatePopupMenu")
	destroyMenu     = user32.NewProc("DestroyMenu")
	appendMenu      = user32.NewProc("AppendMenuW")
	trackPopupMenu  = user32.NewProc("TrackPopupMenu")
	postMessage     = user32.NewProc("PostMessageW")
	shell32         = windows.NewLazySystemDLL("shell32.dll")
	shellNotify     = shell32.NewProc("Shell_NotifyIconW")
)

const (
	wmSysCommand     = 0x0112
	wmCommand        = 0x0111
	wmNull           = 0x0000
	wmTrayCallback   = 0x8001
	scMinimize       = 0xF020
	swHide           = 0
	swShow           = 5
	tpmReturnCommand = 0x0100
	tpmRightButton   = 0x0002
	mfString         = 0x0000
	nimAdd           = 0x0000
	nimDelete        = 0x0002
	nifMessage       = 0x0001
	nifIcon          = 0x0002
	nifTip           = 0x0004
	idiApplication   = 32512
	trayShowCommand  = 1001
	trayExitCommand  = 1002
)

var gwlpWndProc = ^uintptr(3) // GWL_WNDPROC (-4), pointer-width representation.

type trayPoint struct{ X, Y int32 }

// notifyIconDataW mirrors the Windows NOTIFYICONDATAW layout used by
// Shell_NotifyIconW on 64-bit Windows.
type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	CallbackMessage  uint32
	HIcon            uintptr
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	Guid             windows.GUID
	BalloonIcon      uintptr
}

type trayController struct {
	hwnd     uintptr
	webview  webview2.WebView
	oldProc  uintptr
	callback uintptr
	nid      notifyIconDataW
}

func showWindow(url string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus:     true,
		WindowOptions: webview2.WindowOptions{Title: "Cloud LocalStore", Width: 1120, Height: 760, Center: true},
	})
	if w == nil {
		return errors.New("could not create the Cloud LocalStore window; install the Microsoft Edge WebView2 Runtime and try again")
	}
	defer w.Destroy()
	tray, err := newTrayController(w)
	if err != nil {
		return err
	}
	defer tray.close()
	w.SetSize(1120, 760, webview2.HintMin)
	w.Navigate(url)
	w.Run()
	return nil
}

func newTrayController(w webview2.WebView) (*trayController, error) {
	hwnd := uintptr(w.Window())
	if hwnd == 0 {
		return nil, errors.New("could not obtain the Cloud LocalStore window handle")
	}
	t := &trayController{hwnd: hwnd, webview: w}
	t.callback = windows.NewCallback(t.windowProc)
	previous, _, callErr := setWindowProc.Call(hwnd, gwlpWndProc, t.callback)
	if previous == 0 && callErr != windows.ERROR_SUCCESS {
		return nil, fmt.Errorf("install tray window handler: %w", callErr)
	}
	t.oldProc = previous
	icon, _, _ := loadIconWProc.Call(0, idiApplication)
	t.nid = notifyIconDataW{CbSize: uint32(unsafe.Sizeof(notifyIconDataW{})), HWnd: hwnd, UID: 1, UFlags: nifMessage | nifIcon | nifTip, CallbackMessage: wmTrayCallback, HIcon: icon}
	tip, _ := windows.UTF16FromString("Cloud LocalStore")
	copy(t.nid.Tip[:], tip)
	if ok, _, err := shellNotify.Call(nimAdd, uintptr(unsafe.Pointer(&t.nid))); ok == 0 {
		return nil, fmt.Errorf("create notification area icon: %w", err)
	}
	return t, nil
}

func (t *trayController) close() {
	if t.hwnd == 0 {
		return
	}
	_, _, _ = shellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&t.nid)))
	if t.oldProc != 0 {
		_, _, _ = setWindowProc.Call(t.hwnd, gwlpWndProc, t.oldProc)
	}
	t.hwnd = 0
}

func (t *trayController) windowProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmSysCommand:
		if wp&0xFFF0 == scMinimize {
			_, _, _ = showWindowProc.Call(hwnd, swHide)
			return 0
		}
	case wmTrayCallback:
		if lp == 0x0202 || lp == 0x0205 { // left/right button up
			t.showMenu()
			return 0
		}
	case wmCommand:
		switch wp & 0xFFFF {
		case trayShowCommand:
			_, _, _ = showWindowProc.Call(hwnd, swShow)
			_, _, _ = setForeground.Call(hwnd)
			return 0
		case trayExitCommand:
			t.webview.Terminate()
			return 0
		}
	}
	if t.oldProc == 0 {
		return 0
	}
	result, _, _ := callWindowProc.Call(t.oldProc, hwnd, msg, wp, lp)
	return result
}

func (t *trayController) showMenu() {
	menu, _, _ := createMenu.Call()
	if menu == 0 {
		return
	}
	defer destroyMenu.Call(menu)
	showText, _ := windows.UTF16FromString("显示窗口")
	exitText, _ := windows.UTF16FromString("退出程序")
	_, _, _ = appendMenu.Call(menu, mfString, trayShowCommand, uintptr(unsafe.Pointer(&showText[0])))
	_, _, _ = appendMenu.Call(menu, mfString, trayExitCommand, uintptr(unsafe.Pointer(&exitText[0])))
	var point trayPoint
	_, _, _ = getCursorPos.Call(uintptr(unsafe.Pointer(&point)))
	command, _, _ := trackPopupMenu.Call(menu, tpmReturnCommand|tpmRightButton, uintptr(point.X), uintptr(point.Y), 0, t.hwnd, 0)
	if command == trayShowCommand {
		_, _, _ = showWindowProc.Call(t.hwnd, swShow)
		_, _, _ = setForeground.Call(t.hwnd)
	} else if command == trayExitCommand {
		// TrackPopupMenu returns the command synchronously, so terminate the
		// WebView message loop after the menu closes.
		t.webview.Terminate()
	}
	_, _, _ = postMessage.Call(t.hwnd, wmNull, 0, 0)
}

func showWindowError(err error) {
	message, _ := windows.UTF16PtrFromString(fmt.Sprintf("Cloud LocalStore could not start its window.\n\n%s\n\nInstall the Microsoft Edge WebView2 Runtime, then start the app again.", err))
	title, _ := windows.UTF16PtrFromString("Cloud LocalStore")
	messageBoxWProc.Call(0, uintptr(unsafe.Pointer(message)), uintptr(unsafe.Pointer(title)), 0x10)
}
