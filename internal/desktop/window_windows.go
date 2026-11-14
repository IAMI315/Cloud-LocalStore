//go:build windows

package desktop

import (
	_ "embed"
	"errors"
	"fmt"
	"runtime"
	"sync"
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
	sendMessage     = user32.NewProc("SendMessageW")
	createIcon      = user32.NewProc("CreateIconFromResourceEx")
	destroyIcon     = user32.NewProc("DestroyIcon")
	setDPIContext   = user32.NewProc("SetProcessDpiAwarenessContext")
	setDPIAware     = user32.NewProc("SetProcessDPIAware")
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
	wmClose          = 0x0010
	wmCommand        = 0x0111
	wmNull           = 0x0000
	wmSetIcon        = 0x0080
	iconSmall        = 0
	iconBig          = 1
	wmTrayCallback   = 0x8001
	scMinimize       = 0xF020
	scClose          = 0xF060
	swHide           = 0
	swShow           = 5
	swMinimize       = 6
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
	dpiPerMonitorV2  = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
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
	onExit   func()
	exitOnce sync.Once
	oldProc  uintptr
	callback uintptr
	nid      notifyIconDataW
	icon     uintptr
}

//go:embed app-icon.ico
var appIconICO []byte

func showWindow(url string, onExit func()) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	setProcessDPIAwareness()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus:     true,
		WindowOptions: webview2.WindowOptions{Title: "Cloud LocalStore", Width: 1500, Height: 900, Center: true},
	})
	if w == nil {
		return errors.New("could not create the Cloud LocalStore window; install the Microsoft Edge WebView2 Runtime and try again")
	}
	defer w.Destroy()
	tray, err := newTrayController(w, onExit)
	if err != nil {
		return err
	}
	defer tray.close()
	w.SetSize(900, 600, webview2.HintMin)
	w.Navigate(url)
	w.Run()
	return nil
}

// setProcessDPIAwareness must run before creating the native window. Without
// it Windows applies DPI virtualization to the WebView host, so the requested
// dimensions are interpreted in a virtual 96-DPI coordinate space and can be
// clipped or rendered at an unexpected physical size on scaled displays.
func setProcessDPIAwareness() {
	if result, _, _ := setDPIContext.Call(dpiPerMonitorV2); result != 0 {
		return
	}
	_, _, _ = setDPIAware.Call()
}

func newTrayController(w webview2.WebView, onExit func()) (*trayController, error) {
	hwnd := uintptr(w.Window())
	if hwnd == 0 {
		return nil, errors.New("could not obtain the Cloud LocalStore window handle")
	}
	t := &trayController{hwnd: hwnd, webview: w, onExit: onExit}
	t.callback = windows.NewCallback(t.windowProc)
	previous, _, callErr := setWindowProc.Call(hwnd, gwlpWndProc, t.callback)
	if previous == 0 && callErr != windows.ERROR_SUCCESS {
		return nil, fmt.Errorf("install tray window handler: %w", callErr)
	}
	t.oldProc = previous
	icon := loadApplicationIcon()
	if icon == 0 {
		icon, _, _ = loadIconWProc.Call(0, idiApplication)
	}
	if icon != 0 {
		_, _, _ = sendMessage.Call(hwnd, wmSetIcon, iconBig, icon)
		_, _, _ = sendMessage.Call(hwnd, wmSetIcon, iconSmall, icon)
	}
	t.icon = icon
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
	if t.icon != 0 {
		_, _, _ = destroyIcon.Call(t.icon)
		t.icon = 0
	}
	t.hwnd = 0
}

func loadApplicationIcon() uintptr {
	if len(appIconICO) < 22 || appIconICO[0] != 0 || appIconICO[1] != 0 || appIconICO[2] != 1 || appIconICO[3] != 0 {
		return 0
	}
	imageSize := uint32(appIconICO[14]) | uint32(appIconICO[15])<<8 | uint32(appIconICO[16])<<16 | uint32(appIconICO[17])<<24
	imageOffset := uint32(appIconICO[18]) | uint32(appIconICO[19])<<8 | uint32(appIconICO[20])<<16 | uint32(appIconICO[21])<<24
	if imageSize == 0 || imageOffset > uint32(len(appIconICO)) || imageSize > uint32(len(appIconICO))-imageOffset {
		return 0
	}
	imageData := appIconICO[imageOffset : imageOffset+imageSize]
	icon, _, _ := createIcon.Call(
		uintptr(unsafe.Pointer(&imageData[0])),
		uintptr(len(imageData)),
		1,
		0x00030000,
		32,
		32,
		0,
	)
	return icon
}

func (t *trayController) windowProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmSysCommand:
		switch wp & 0xFFF0 {
		case scMinimize:
			// A normal minimize remains represented by a taskbar button. The
			// close button is the action that hides the window to the tray.
			_, _, _ = showWindowProc.Call(hwnd, swMinimize)
			return 0
		case scClose:
			_, _, _ = showWindowProc.Call(hwnd, swHide)
			return 0
		}
	case wmClose:
		// Keep the desktop process and local service alive while the window is
		// closed. The tray menu is the only path that requests a real exit.
		_, _, _ = showWindowProc.Call(hwnd, swHide)
		return 0
	case wmTrayCallback:
		if lp == 0x0202 || lp == 0x0203 { // left button up/double-click
			_, _, _ = showWindowProc.Call(hwnd, swShow)
			_, _, _ = setForeground.Call(hwnd)
			return 0
		}
		if lp == 0x0205 { // right button up
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
			t.requestExit()
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
		t.requestExit()
	}
	_, _, _ = postMessage.Call(t.hwnd, wmNull, 0, 0)
}

func (t *trayController) requestExit() {
	t.exitOnce.Do(func() {
		if t.onExit != nil {
			t.onExit()
		}
		t.webview.Terminate()
	})
}

func showWindowError(err error) {
	message, _ := windows.UTF16PtrFromString(fmt.Sprintf("Cloud LocalStore could not start its window.\n\n%s\n\nInstall the Microsoft Edge WebView2 Runtime, then start the app again.", err))
	title, _ := windows.UTF16PtrFromString("Cloud LocalStore")
	messageBoxWProc.Call(0, uintptr(unsafe.Pointer(message)), uintptr(unsafe.Pointer(title)), 0x10)
}
