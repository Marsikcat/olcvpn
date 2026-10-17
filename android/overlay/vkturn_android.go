//go:build android

// Подкладывается в client/ vk-turn-proxy при сборке под Android (go build
// -overlay, см. android/build.ps1).

package main

import (
	"net"
	"os"
	"time"
)

// В Android нет /etc/resolv.conf, и стандартный резолвер Go без cgo идёт на
// 127.0.0.1:53. Свои запросы клиент и так шлёт через customResolver, а это —
// для остального, что пользуется резолвером по умолчанию.
func init() {
	net.DefaultResolver = customResolver
	// Go в Android всегда считает местным UTC; пояс телефона — в TZ.
	if loc, err := time.LoadLocation(os.Getenv("TZ")); err == nil && os.Getenv("TZ") != "" {
		time.Local = loc
	}
}
