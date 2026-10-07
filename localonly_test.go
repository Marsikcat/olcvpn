package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalOnly(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := localOnly("127.0.0.1:8899", ok)

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
