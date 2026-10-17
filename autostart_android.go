//go:build android

package main

import (
	"os"
	"path/filepath"
)

// Автозапуск в Android — это флаг-файл в каталоге приложения: приёмник
// BOOT_COMPLETED оболочки смотрит на него после перезагрузки телефона и
// поднимает olcvpn, а тот, если включено «Подключаться сразу», подключается.

// appDir is the app's private directory; main sets it.
var appDir string

func autostartFlag() string { return filepath.Join(appDir, "autostart") }

func autostartEnabled() bool {
	_, err := os.Stat(autostartFlag())
	return err == nil
}

func enableAutostart() (string, error) {
	if err := os.WriteFile(autostartFlag(), nil, 0o600); err != nil {
		return "", err
	}
	return "Автозапуск включён: olcvpn запустится после перезагрузки телефона. " +
		"Чтобы он сразу и подключался, включите «Подключаться сразу при запуске».", nil
}

func disableAutostart() error {
	if err := os.Remove(autostartFlag()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (a *app) repairAutostart() {}
