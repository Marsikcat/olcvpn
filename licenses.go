package main

import (
	"embed"
	"net/http"
	"strings"
)

// Лицензии, которые показывает окно «Лицензии»: своя (WTFPL), список
// сторонних компонентов и полный текст GPL-3.0 — под ней sing-box и клиент
// VK TURN, а сборка для Android, где sing-box встроен в бэкенд, целиком.
// В APK те же файлы лежат ещё и в assets/licenses (см. android/app/build.gradle.kts).
//
//go:embed LICENSE THIRD-PARTY.md licenses/GPL-3.0.txt
var licenseFS embed.FS

var licenseFiles = map[string]string{
	"olcvpn":      "LICENSE",
	"third-party": "THIRD-PARTY.md",
	"gpl-3.0":     "licenses/GPL-3.0.txt",
}

// handleLicense отдаёт /licenses/<имя> простым текстом: по расширению
// THIRD-PARTY.md WebView2 мог бы и скачать файл вместо того, чтобы показать.
func handleLicense(w http.ResponseWriter, r *http.Request) {
	name, ok := licenseFiles[strings.TrimPrefix(r.URL.Path, "/licenses/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := licenseFS.ReadFile(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}
