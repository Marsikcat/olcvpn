//go:build android && !cgo

// Подкладывается в cmd/olcrtc ядра при сборке под Android (go build -overlay,
// см. android/build.ps1).

package main

import (
	"os"
	"time"

	"github.com/wlynxg/anet"
)

// Без cgo anet не может узнать версию Android и берёт net.Interfaces(), а её
// Android 11+ приложениям не даёт (netlinkrib: permission denied). Тогда ICE
// остался бы без локальных адресов. Версия выше 11 включает обход через
// RTM_GETADDR, который разрешён.
func init() {
	anet.SetAndroidVersion(14)
	localTime()
}

// localTime: Go в Android всегда считает местным UTC, а пояс телефона olcvpn
// передаёт в TZ — иначе время в строках ядра расходится с журналом.
func localTime() {
	if loc, err := time.LoadLocation(os.Getenv("TZ")); err == nil && os.Getenv("TZ") != "" {
		time.Local = loc
	}
}
