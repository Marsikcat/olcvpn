//go:build android

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // на случай, если системная база поясов не прочитается
)

// main runs the backend for the Android shell. The shell starts it with:
//
//	OLCVPN_DIR     личный каталог приложения: настройки, data/ ядра, сокет
//	OLCVPN_LIBDIR  каталог нативных библиотек, где лежат ядро и клиент VK TURN
//	OLCVPN_TOKEN   секрет, без которого интерфейс не отвечает (см. localOnly)
//	OLCVPN_DNS     DNS текущей сети через запятую
//
// and keeps our stdin open: it closes when the app process goes away, and
// then the backend goes too.
func main() {
	dir := os.Getenv("OLCVPN_DIR")
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		dir = filepath.Dir(exe)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	appDir = dir
	// Go в Android всегда считает местным временем UTC; пояс телефона
	// оболочка передаёт в TZ.
	if loc, err := time.LoadLocation(os.Getenv("TZ")); err == nil && os.Getenv("TZ") != "" {
		time.Local = loc
	}
	setSystemDNS(strings.Split(os.Getenv("OLCVPN_DNS"), ","))

	// Конфиги sing-box — те же, что на ПК, со старой схемой DNS-серверов.
	_ = os.Setenv("ENABLE_DEPRECATED_LEGACY_DNS_SERVERS", "true")
	_ = os.Setenv("ENABLE_DEPRECATED_MISSING_DOMAIN_RESOLVER", "true")

	a := newApp(dir)
	tunShell = a.shell
	// Журнал — ещё и в stdout: оболочка пишет его в logcat, и по adb видно
	// то же, что в «Журнале» приложения. Ключей в нём нет.
	logs, _ := a.log.subscribe()
	go func() {
		for line := range logs {
			fmt.Println(line)
		}
	}()
	srv, err := a.serve()
	if err != nil {
		log.Fatal(err)
	}
	// Сокет — после интерфейса: первым делом оболочка получает его адрес.
	if err := a.shell.listen(filepath.Join(dir, "olcvpn.sock")); err != nil {
		log.Fatal(err)
	}
	a.startBackground()
	go a.shell.watchState()
	go a.shell.watchSpeed()

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(done)
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-done:
	case <-sig:
	}
	a.shutdown(srv)
}
