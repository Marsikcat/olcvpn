package main

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2"
)

// runWindow opens the app in its own WebView2 window and blocks until the user
// closes it. It reports false when WebView2 is unavailable, so the caller can
// fall back to the default browser.
func runWindow(url string) (ok bool) {
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

	w.SetSize(430, 620, webview2.HintMin)
	setWindowIcon(w.Window())
	w.Navigate(url)
	w.Run()
	return true
}

const (
	wmSetIcon  = 0x0080
	iconSmall  = 0
	iconBig    = 1
	imageIcon  = 1
	lrLoadFile = 0x0010
)

var (
	shell32      = syscall.NewLazyDLL("shell32.dll")
	user32       = syscall.NewLazyDLL("user32.dll")
	extractIcons = shell32.NewProc("ExtractIconExW")
	sendMessageW = user32.NewProc("SendMessageW")
)

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
	extractIcons.Call(
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
