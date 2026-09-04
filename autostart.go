package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const (
	autostartTask   = "olcvpn"
	autostartRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
)

// run executes a console helper without flashing a window.
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // fixed system utilities
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// autostartEnabled reports whether either mechanism is currently registered.
func autostartEnabled() bool {
	if _, err := run("schtasks", "/Query", "/TN", autostartTask); err == nil {
		return true
	}
	if out, err := run("reg", "query", autostartRunKey, "/v", autostartTask); err == nil {
		return strings.Contains(out, autostartTask)
	}
	return false
}

// enableAutostart registers the app to start at logon.
//
// A scheduled task is preferred over the Run key because only a task can carry
// "run with highest privileges" — and without elevation the TUN mode cannot
// come up, so a Run-key autostart would silently downgrade to proxy-only.
// Creating such a task itself needs elevation, hence the fallback.
func enableAutostart() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}

	if isAdmin() {
		if _, err := run("schtasks",
			"/Create", "/TN", autostartTask,
			"/TR", `"`+exe+`"`,
			"/SC", "ONLOGON",
			"/RL", "HIGHEST",
			"/F",
		); err == nil {
			return "Задача создана: запуск при входе с правами администратора, TUN будет доступен.", nil
		}
	}

	if out, err := run("reg", "add", autostartRunKey,
		"/v", autostartTask, "/t", "REG_SZ", "/d", exe, "/f"); err != nil {
		return "", fmt.Errorf("не удалось прописать автозапуск: %s", strings.TrimSpace(out))
	}
	return "Автозапуск включён, но без прав администратора: " +
		"после входа будет доступен только режим «Только прокси». " +
		"Включите его из start.cmd, чтобы получить задачу с правами.", nil
}

// disableAutostart removes both mechanisms, whichever is present.
func disableAutostart() error {
	var failed []string
	if _, err := run("schtasks", "/Query", "/TN", autostartTask); err == nil {
		if out, err := run("schtasks", "/Delete", "/TN", autostartTask, "/F"); err != nil {
			failed = append(failed, strings.TrimSpace(out))
		}
	}
	if _, err := run("reg", "query", autostartRunKey, "/v", autostartTask); err == nil {
		if out, err := run("reg", "delete", autostartRunKey, "/v", autostartTask, "/f"); err != nil {
			failed = append(failed, strings.TrimSpace(out))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("не удалось снять автозапуск: %s", strings.Join(failed, "; "))
	}
	return nil
}
