package main

import (
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2"
)

const (
	wmClose   = 0x0010
	wmSetIcon = 0x0080

	// GWLP_WNDPROC равен -4; как беззнаковый индекс это ^uintptr(3) —
	// прямая запись -4 не помещается в uintptr на этапе компиляции.
	gwlpWndProc = ^uintptr(3)

	swHide    = 0
	swShow    = 5
	swRestore = 9

	iconSmall = 0
	iconBig   = 1
)

var (
	shell32 = syscall.NewLazyDLL("shell32.dll")
	user32  = syscall.NewLazyDLL("user32.dll")

	extractIconExW    = shell32.NewProc("ExtractIconExW")
	sendMessageW      = user32.NewProc("SendMessageW")
	showWindow        = user32.NewProc("ShowWindow")
	setForegroundWin  = user32.NewProc("SetForegroundWindow")
	setWindowLongPtrW = user32.NewProc("SetWindowLongPtrW")
	callWindowProcW   = user32.NewProc("CallWindowProcW")
	origWndProc       uintptr
	closeHides        atomic.Bool // "закрыть" сворачивает вместо выхода
	quitting          atomic.Bool
)

// runWindow opens the app in its own WebView2 window and blocks until it is
// closed or the tray asks to quit. It reports false when WebView2 is
// unavailable, so the caller can fall back to the default browser.
func runWindow(url string, a *app, t *tray) (ok bool) {
	runtime.LockOSThread()

	defer func() {
		// A missing or broken WebView2 runtime surfaces as a panic from the
		// COM bootstrap rather than an error value.
		if r := recover(); r != nil {
			ok = false
		}
	}()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "olcvpn",
			Width:  520,
			Height: 900,
			Center: true,
		},
	})
	if w == nil {
		return false
	}
	defer w.Destroy()

	hwnd := w.Window()
	w.SetSize(430, 620, webview2.HintMin)
	setWindowIcon(hwnd)
	closeHides.Store(a.cfg.TrayClose)
	subclass(hwnd)

	// The tray lives on another thread; every touch of the window has to be
	// marshalled back onto this one.
	go func() {
		for {
			select {
			case <-t.open:
				w.Dispatch(func() { showWindowNow(hwnd) })
			case <-t.quit:
				quitting.Store(true)
				w.Dispatch(w.Terminate)
				return
			}
		}
	}()

	if a.cfg.StartHidden {
		w.Dispatch(func() { hideWindow(hwnd) })
	}

	w.Navigate(url)
	w.Run()
	return true
}

// subclass installs our own window procedure so the close button can hide the
// window instead of ending the process. go-webview2 exposes no close hook, and
// swallowing WM_CLOSE is the only place the decision can be made.
func subclass(hwnd unsafe.Pointer) {
	if hwnd == nil {
		return
	}
	proc := syscall.NewCallback(func(h, msg, wparam, lparam uintptr) uintptr {
		if msg == wmClose && closeHides.Load() && !quitting.Load() {
			showWindowCmd(h, swHide)
			return 0
		}
		ret, _, _ := callWindowProcW.Call(origWndProc, h, msg, wparam, lparam)
		return ret
	})
	origWndProc, _, _ = setWindowLongPtrW.Call(uintptr(hwnd), gwlpWndProc, proc)
}

func showWindowCmd(hwnd uintptr, cmd int) {
	showWindow.Call(hwnd, uintptr(cmd))
}

func showWindowNow(hwnd unsafe.Pointer) {
	showWindowCmd(uintptr(hwnd), swShow)
	showWindowCmd(uintptr(hwnd), swRestore)
	setForegroundWin.Call(uintptr(hwnd))
}

func hideWindow(hwnd unsafe.Pointer) {
	showWindowCmd(uintptr(hwnd), swHide)
}

// setWindowIcon puts the executable's own icon in the title bar and Alt-Tab
// list. Reading it back out of the running binary by index avoids depending on
// whichever resource IDs the resource compiler happened to assign.
func setWindowIcon(hwnd unsafe.Pointer) {
	if hwnd == nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	path, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return
	}

	var large, small uintptr
	extractIconExW.Call(
		uintptr(unsafe.Pointer(path)),
		0, // first icon group in the file
		uintptr(unsafe.Pointer(&large)),
		uintptr(unsafe.Pointer(&small)),
		1,
	)
	if large != 0 {
		sendMessageW.Call(uintptr(hwnd), wmSetIcon, iconBig, large)
	}
	if small != 0 {
		sendMessageW.Call(uintptr(hwnd), wmSetIcon, iconSmall, small)
	}
}
