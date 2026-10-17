package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Тестовый бинарник сам изображает vkturn-client, если его так запустить.
func TestMain(m *testing.M) {
	if os.Getenv("OLCVPN_FAKE_VKTURN") == "1" {
		fakeVKTurnClient()
		return
	}
	os.Exit(m.Run())
}

// fakeVKTurnClient ведёт себя как клиент с -access-pipe: читает доступ из
// stdin и сообщает новый. Первый запуск — как после капчи.
func fakeVKTurnClient() {
	pipe, wrap, n := false, false, ""
	envKey := os.Getenv("VKTURN_WRAP_KEY")
	argKey := false
	for i, a := range os.Args[1:] {
		pipe = pipe || a == "-access-pipe"
		wrap = wrap || a == "-wrap"
		argKey = argKey || (envKey != "" && strings.Contains(a, envKey))
		if a == "-n" && i+2 < len(os.Args) {
			n = os.Args[i+2]
		}
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	switch {
	case wrap:
		// Отчёт о маскировке — тем же каналом, что и доступ к звонку.
		fmt.Printf("%swrap n=%s envkey=%t argkey=%t\n", vkAccessPrefix, n, len(envKey) == 64, argKey)
	case !pipe:
		fmt.Println("клиента запустили без -access-pipe")
		os.Exit(2)
	case line == "":
		fmt.Println(vkAccessPrefix + "first-access")
	default:
		fmt.Println(vkAccessPrefix + line + "+again")
	}
	time.Sleep(300 * time.Millisecond) // дать olcvpn дочитать вывод
	os.Exit(0)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождался: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeVKTurnSetup puts the test binary in place of bin\vkturn-client.exe.
func fakeVKTurnSetup(t *testing.T) (*Tunnel, *logBus) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, exe, filepath.Join(dir, "bin", "vkturn-client.exe"))
	t.Setenv("OLCVPN_FAKE_VKTURN", "1")
	bus := newLogBus()
	return newTunnel(dir, bus), bus
}

// startFakeVKTurn starts the fake client for a server at peer, masked when
// wrapKey is set.
func startFakeVKTurn(t *testing.T, peer, wrapKey string) (*Tunnel, *Server) {
	t.Helper()
	tun, _ := fakeVKTurnSetup(t)
	srv, err := newVKTurnServer("", "https://vk.com/call/join/abc", peer, testWG)
	if err == nil {
		err = srv.setWrapKey(wrapKey)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := tun.StartVKTurn(&Config{SocksHost: "127.0.0.1", SocksPort: 18808}, &srv, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitFor(t, "выход клиента", func() bool { return !tun.running() }) })
	return tun, &srv
}

// Доступ к звонку переживает перезапуск клиента VK TURN: то, что сообщил
// один запуск, следующий получает через stdin — и в журнал это не попадает.
func TestVKAccessSurvivesClientRestart(t *testing.T) {
	tun, bus := fakeVKTurnSetup(t)
	srv, err := newVKTurnServer("", "https://vk.com/call/join/abc", "203.0.113.10:56000", testWG)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{SocksHost: "127.0.0.1", SocksPort: 18808}

	run := func(want string) {
		t.Helper()
		if err := tun.StartVKTurn(cfg, &srv, false); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "доступ "+want, func() bool { return tun.savedVKAccess(srv.VKLink) == want })
		waitFor(t, "выход клиента", func() bool { return !tun.running() })
	}
	run("first-access")
	run("first-access+again")

	for _, l := range bus.history() {
		if strings.Contains(l, "first-access") || strings.Contains(l, vkAccessPrefix) {
			t.Errorf("доступ к звонку попал в журнал: %q", l)
		}
	}

	// Сервер с тем же звонком, но другим адресом, получает тот же доступ.
	other, _ := newVKTurnServer("", srv.VKLink, "203.0.113.10:56010", testWG)
	if err := tun.StartVKTurn(cfg, &other, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "общий доступ", func() bool { return tun.savedVKAccess(srv.VKLink) == "first-access+again+again" })
	waitFor(t, "выход клиента", func() bool { return !tun.running() })

	tun.keepVKAccess(srv.VKLink, "-")
	if got := tun.savedVKAccess(srv.VKLink); got != "" {
		t.Errorf("после отказа VK доступ остался: %q", got)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// Клиент повторяет «[VK Auth] Success» при каждом обновлении учёток, раз в
// десять минут. Посреди сессии это не повод ни перезагружать окно, ни
// показывать «подключаюсь».
func TestVKAuthRefreshKeepsSession(t *testing.T) {
	tun := newTunnel(t.TempDir(), newLogBus())
	var navs []string
	tun.onCaptcha = func(u string) { navs = append(navs, u) }
	srv := &Server{ID: "x", Kind: kindVKTurn}
	tun.peerSeen = true
	tun.singbox = fakeSingBox{} // sing-box как будто поднят
	tun.setPhase(phaseProxy, "VK TURN")
	feed := func(lines ...string) {
		tun.pumpVKTurn(strings.NewReader(strings.Join(lines, "\n")), &Config{}, srv, false)
	}

	feed("[STREAM 0] [VK Auth] Success with saved call access")
	if ph, _, _ := tun.status(); ph != phaseProxy {
		t.Errorf("обновление учёток сменило фазу на %q", ph)
	}
	if len(navs) != 0 {
		t.Errorf("окно дёрнули без капчи: %q", navs)
	}

	// Капча посреди сессии: после неё окно возвращается, фаза — рабочая.
	feed("Open this URL in your browser: http://127.0.0.1:8765/captcha",
		"[STREAM 0] [VK Auth] Success with client_id=6287487")
	if ph, _, _ := tun.status(); ph != phaseProxy {
		t.Errorf("после капчи фаза %q, а должна быть proxy", ph)
	}
	if len(navs) != 2 || navs[0] == "" || navs[1] != "" {
		t.Errorf("переходы окна: %q", navs)
	}
}

func TestVKAccessLife(t *testing.T) {
	for line, want := range map[string]string{
		"[VK Auth] Call access saved for 24h0m0s: credential refreshes and reconnects skip the captcha":         " (VK выдал его на 24 ч)",
		"[VK Auth] Call access saved for 1h30m0s: credential refreshes and reconnects skip the captcha":         " (VK выдал его на 1 ч 30 мин)",
		"[VK Auth] Call access saved for 45m0s: credential refreshes and reconnects skip the captcha":           " (VK выдал его на 45 мин)",
		"[VK Auth] Call access saved until VK refuses it: credential refreshes and reconnects skip the captcha": "",
	} {
		if got := vkAccessLife(line); got != want {
			t.Errorf("%q → %q, want %q", line, got, want)
		}
	}
}

// fakeSingBox stands in for a running sing-box.
type fakeSingBox struct{}

func (fakeSingBox) stop() {}
