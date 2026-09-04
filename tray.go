package main

import (
	_ "embed"
	"runtime"

	"fyne.io/systray"
)

//go:embed olcvpn.ico
var trayIcon []byte

// tray owns the notification-area icon and its menu.
type tray struct {
	a    *app
	open chan struct{} // asks the UI thread to show the window
	quit chan struct{} // asks the UI thread to tear everything down
}

func newTray(a *app) *tray {
	return &tray{
		a:    a,
		open: make(chan struct{}, 1),
		quit: make(chan struct{}, 1),
	}
}

// start runs the tray on its own OS thread. systray drives a Win32 message
// loop of its own, and a thread may only pump messages for windows it created,
// so it cannot share the main thread with WebView2.
func (t *tray) start() {
	go func() {
		runtime.LockOSThread()
		systray.Run(t.onReady, func() {})
	}()
}

func (t *tray) onReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("olcvpn")
	systray.SetTooltip("olcvpn — отключено")

	mOpen := systray.AddMenuItem("Открыть", "")
	systray.AddSeparator()
	mToggle := systray.AddMenuItem("Подключить", "")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Выход", "")

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				t.signal(t.open)

			case <-mToggle.ClickedCh:
				if t.a.tun.running() {
					t.a.tun.Stop()
				} else {
					go t.a.connectSelected()
				}

			case <-mQuit.ClickedCh:
				t.signal(t.quit)
				return
			}
		}
	}()

	// Keep the menu and tooltip in step with the tunnel.
	go func() {
		for range t.a.stateChanged() {
			ph, _, _ := t.a.tun.status()
			if t.a.tun.running() {
				mToggle.SetTitle("Отключить")
			} else {
				mToggle.SetTitle("Подключить")
			}
			systray.SetTooltip("olcvpn — " + trayLabel(ph, t.a))
		}
	}()
}

func (t *tray) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func trayLabel(ph phase, a *app) string {
	switch ph {
	case phaseStopped:
		return "отключено"
	case phaseStarting:
		return "подключение"
	case phaseWaiting:
		return "нет связи с сервером"
	case phaseError:
		return "ошибка"
	case phaseProxy, phaseConnected:
		if a.tun.tunActive() {
			return "защищено, весь трафик"
		}
		return "защищено, только прокси"
	}
	return "—"
}

func (t *tray) stop() { systray.Quit() }
