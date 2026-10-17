//go:build windows

package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	dir := filepath.Dir(exe)
	if v := os.Getenv("OLCVPN_DIR"); v != "" {
		dir = v
	}

	a := newApp(dir)
	srv, err := a.serve()
	if err != nil {
		log.Fatal(err)
	}
	a.startBackground()

	t := newTray(a)
	a.tray = t
	t.start()
	defer t.stop()

	if browserMode() || !runWindow(a.url, a, t) {
		// No WebView2 runtime (or --browser asked for it): fall back to the
		// default browser and wait for a signal instead of a window close.
		openBrowser(a.url)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		select {
		case <-ctx.Done():
		case <-t.quit:
		}
	}

	a.shutdown(srv)
}

// browserMode reports whether the user asked for the browser UI instead of the
// app window.
func browserMode() bool {
	for _, arg := range os.Args[1:] {
		if arg == "--browser" || arg == "-browser" {
			return true
		}
	}
	return false
}

// trayMode reports whether the app was asked to start straight into the tray
// for this launch only, without touching the saved setting. The autostart
// entry can use it to come up quietly while a manual launch still shows the
// window.
func trayMode() bool {
	for _, arg := range os.Args[1:] {
		if arg == "--tray" || arg == "-tray" {
			return true
		}
	}
	return false
}

func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}
