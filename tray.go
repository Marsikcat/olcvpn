package main

import (
	_ "embed"
	"runtime"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
)

//go:embed olcvpn.ico
var trayIcon []byte

var getDoubleClickTime = syscall.NewLazyDLL("user32.dll").NewProc("GetDoubleClickTime")

// tray owns the notification-area icon and its menu.
type tray struct {
	a    *app
	open chan struct{} // asks the UI thread to show the window
	quit chan struct{} // asks the UI thread to tear everything down

	tapMu   sync.Mutex
	lastTap time.Time
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
	systray.SetOnTapped(t.onTap)

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

// onTap turns systray's plain left-click callback into a double-click.
//
// This build of systray only forwards WM_LBUTTONUP, so a double-click arrives
// as two ordinary taps and has to be recognised by timing. The threshold is
// the one the user set for the whole system, not a number of our own, so the
// icon reacts exactly like every other double-click on the machine.
func (t *tray) onTap() {
	now := time.Now()

	t.tapMu.Lock()
	double := !t.lastTap.IsZero() && now.Sub(t.lastTap) <= doubleClickTime()
	if double {
		// Сбрасываем, иначе третий клик подряд сойдёт за ещё один двойной.
		t.lastTap = time.Time{}
	} else {
		t.lastTap = now
	}
	t.tapMu.Unlock()

	if double {
		t.signal(t.open)
	}
}

// doubleClickTime reports the system's double-click interval.
func doubleClickTime() time.Duration {
	ms, _, _ := getDoubleClickTime.Call()
	if ms == 0 {
		ms = 500 // документированное значение по умолчанию
	}
	return time.Duration(ms) * time.Millisecond
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
