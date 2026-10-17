package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Окно «Лицензии» берёт тексты у бэкенда — на ПК и в APK одинаково.
func TestLicenses(t *testing.T) {
	for _, c := range []struct{ name, want string }{
		{"olcvpn", "DO WHAT THE FUCK YOU WANT TO PUBLIC LICENSE"},
		{"third-party", "Сторонние компоненты"},
		{"gpl-3.0", "GNU GENERAL PUBLIC LICENSE\n                       Version 3, 29 June 2007"},
	} {
		w := httptest.NewRecorder()
		handleLicense(w, httptest.NewRequest("GET", "/licenses/"+c.name, nil))
		if w.Code != 200 {
			t.Errorf("%s: код %d", c.name, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", c.name, ct)
		}
		if !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: нет %q", c.name, c.want)
		}
	}
	// Полный текст, а не обрывок: последний абзац GPL-3.0 на месте.
	w := httptest.NewRecorder()
	handleLicense(w, httptest.NewRequest("GET", "/licenses/gpl-3.0", nil))
	if !strings.Contains(w.Body.String(), "why-not-lgpl.html") {
		t.Error("gpl-3.0: текст обрезан")
	}

	for _, p := range []string{"/licenses/", "/licenses/LICENSE", "/licenses/../main.go"} {
		w := httptest.NewRecorder()
		handleLicense(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 404 {
			t.Errorf("%s: код %d, want 404", p, w.Code)
		}
	}
}
