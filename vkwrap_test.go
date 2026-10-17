package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Ключ выдуманный: правильной формы, но ни к чему не подходит.
const testWrapKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func TestSetWrapKey(t *testing.T) {
	var s Server
	if err := s.setWrapKey("  " + strings.ToUpper(testWrapKey) + "\n"); err != nil || s.WrapKey != testWrapKey {
		t.Errorf("ключ в верхнем регистре: %q, %v", s.WrapKey, err)
	}
	if err := s.setWrapKey(""); err != nil || s.WrapKey != "" {
		t.Errorf("пустой ключ должен выключать маскировку: %q, %v", s.WrapKey, err)
	}
	for _, bad := range []string{"abc", testWrapKey[:62], testWrapKey + "00", "zz" + testWrapKey[2:]} {
		s.WrapKey = testWrapKey
		if err := s.setWrapKey(bad); err == nil {
			t.Errorf("%q принят", bad)
		}
		if s.WrapKey != testWrapKey {
			t.Errorf("%q испортил прежний ключ", bad)
		}
	}
}

func TestVKTurnURIKeepsWrapKey(t *testing.T) {
	vk, _ := newVKTurnServer("VK", "https://vk.com/call/join/abc", "203.0.113.10:56010", testWG)
	if err := vk.setWrapKey(testWrapKey); err != nil {
		t.Fatal(err)
	}
	got, err := parseVKTurnURI(vkturnURI(vk))
	if err != nil {
		t.Fatal(err)
	}
	if got.WrapKey != testWrapKey {
		t.Errorf("ключ маскировки потерялся в ссылке: %q", got.WrapKey)
	}

	plain, _ := newVKTurnServer("VK", "https://vk.com/call/join/abc", "203.0.113.10:56000", testWG)
	if strings.Contains(vkturnURI(plain), "wrap=") {
		t.Error("ссылка сервера без маскировки несёт ключ")
	}
	bad := strings.Replace(vkturnURI(plain), "#", "&wrap=nothex#", 1)
	if _, err := parseVKTurnURI(bad); err == nil {
		t.Error("ссылка с негодным ключом принята")
	}
}

// Окно видит, что маскировка включена, но не сам ключ.
func TestStateHidesWrapKey(t *testing.T) {
	vk, _ := newVKTurnServer("", "https://vk.com/call/join/abc", "203.0.113.10:56010", testWG)
	_ = vk.setWrapKey(testWrapKey)
	a := &app{cfg: defaultConfig(filepath.Join(t.TempDir(), "olcvpn.json")), log: newLogBus(),
		tun: newTunnel(t.TempDir(), newLogBus()), kind: map[string]coreKind{}}
	a.cfg.Servers = []Server{vk}
	w := httptest.NewRecorder()
	a.handleState(w, nil)
	if strings.Contains(w.Body.String(), testWrapKey) {
		t.Fatal("в состоянии есть ключ маскировки")
	}
	var st struct {
		Servers []Server `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Servers) != 1 || !st.Servers[0].Wrap {
		t.Errorf("окно не знает, что маскировка включена: %+v", st.Servers)
	}
	if a.cfg.Servers[0].Wrap || a.cfg.Servers[0].WrapKey != testWrapKey {
		t.Error("publicServers испортил сохранённый сервер")
	}
}

// Клиент с маскировкой получает -wrap и больше потоков, а ключ — только
// через окружение.
func TestVKTurnStartPassesWrapKey(t *testing.T) {
	tun, srv := startFakeVKTurn(t, "203.0.113.10:56010", testWrapKey)
	want := "wrap n=16 envkey=true argkey=false"
	waitFor(t, want, func() bool { return tun.savedVKAccess(srv.VKLink) == want })
}
