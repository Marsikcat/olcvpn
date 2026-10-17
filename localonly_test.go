package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalOnly(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := localOnly("127.0.0.1:8899", "", ok)

	cases := []struct {
		name, method, host, path, ct string
		want                         int
	}{
		{"своя страница", "GET", "127.0.0.1:8899", "/", "", 200},
		{"состояние", "GET", "127.0.0.1:8899", "/api/state", "", 200},
		{"localhost", "GET", "localhost:8899", "/api/state", "", 200},
		{"JSON POST", "POST", "127.0.0.1:8899", "/api/vkturn/add", "application/json", 200},
		{"DNS rebinding", "GET", "evil.example:8899", "/api/state", "", 403},
		{"форма с чужого сайта", "POST", "127.0.0.1:8899", "/api/vkturn/add", "text/plain", 403},
		{"GET вместо POST", "GET", "127.0.0.1:8899", "/api/disconnect", "", 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://"+c.host+c.path, strings.NewReader("{}"))
		r.Host = c.host
		if c.ct != "" {
			r.Header.Set("Content-Type", c.ct)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: код %d, want %d", c.name, w.Code, c.want)
		}
	}
}

// В Android к 127.0.0.1 может постучаться любое приложение на телефоне, так
// что без токена из cookie не отвечает даже страница.
func TestLocalOnlyToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := localOnly("127.0.0.1:8899", "s3cret", ok)
	for _, c := range []struct {
		name, cookie string
		want         int
	}{
		{"без токена", "", 403},
		{"чужой токен", "nope", 403},
		{"свой токен", "s3cret", 200},
	} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8899/api/state", nil)
		if c.cookie != "" {
			r.AddCookie(&http.Cookie{Name: tokenCookie, Value: c.cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: код %d, want %d", c.name, w.Code, c.want)
		}
	}
}
