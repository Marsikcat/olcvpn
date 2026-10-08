package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Ключи выдуманные: правильной формы, но ни к чему не подходят.
const (
	testPriv = "cFakeClientPrivateKeyAAAAAAAAAAAAAAAAAAAAA0="
	testPub  = "yFakeServerPublicKeyAAAAAAAAAAAAAAAAAAAAAA0="
	testWG   = `
# комментарий
[Interface]
PrivateKey = ` + testPriv + `
Address = 10.77.0.2/32, fd00::2/128
DNS = 1.1.1.1, 8.8.8.8
MTU = 1280

[Peer]
PublicKey = ` + testPub + `
Endpoint = 127.0.0.1:9000
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25
`
)

func TestParseWireGuardConf(t *testing.T) {
	wg, err := parseWireGuardConf(testWG)
	if err != nil {
		t.Fatal(err)
	}
	if wg.PrivateKey != testPriv || wg.PeerPublicKey != testPub {
		t.Errorf("ключи разобраны неверно: %+v", wg)
	}
	if len(wg.Address) != 2 || wg.Address[0] != "10.77.0.2/32" {
		t.Errorf("Address = %v", wg.Address)
	}
	if wg.DNS != "1.1.1.1" || wg.MTU != 1280 || wg.Keepalive != 25 {
		t.Errorf("DNS/MTU/keepalive = %q/%d/%d", wg.DNS, wg.MTU, wg.Keepalive)
	}
}

func TestParseWireGuardConfRejectsIncomplete(t *testing.T) {
	for name, text := range map[string]string{
		"без PrivateKey": "[Interface]\nAddress = 10.0.0.2/32\n[Peer]\nPublicKey = " + testPub,
		"без Address":    "[Interface]\nPrivateKey = " + testPriv + "\n[Peer]\nPublicKey = " + testPub,
		"без PublicKey":  "[Interface]\nPrivateKey = " + testPriv + "\nAddress = 10.0.0.2/32",
		"пусто":          "",
	} {
		if _, err := parseWireGuardConf(text); err == nil {
			t.Errorf("%s: ошибки нет, а должна быть", name)
		}
	}
}

func TestNewVKTurnServerValidatesLink(t *testing.T) {
	good := []string{
		"https://vk.com/call/join/7a0CaOUkyad0RuGwyWAucx9iSPsigoShqfqRYkjpWWk",
		"https://vk.ru/call/join/abc_DEF-123",
		"https://m.vk.com/call/join/abc",
	}
	for _, link := range good {
		if _, err := newVKTurnServer("", link, "203.0.113.10:56000", testWG); err != nil {
			t.Errorf("%s: %v", link, err)
		}
	}
	bad := []string{
		"https://evil.example/call/join/abc",
		"https://vk.com/im",
		"vk.com/call/join/abc",
		"https://vk.com.evil.example/call/join/abc",
	}
	for _, link := range bad {
		if _, err := newVKTurnServer("", link, "203.0.113.10:56000", testWG); err == nil {
			t.Errorf("%s: принята, хотя не должна", link)
		}
	}
	if _, err := newVKTurnServer("", good[0], "203.0.113.10", testWG); err == nil {
		t.Error("адрес без порта принят")
	}
}

// Импорт и удаление подписки не должны трогать серверы, добавленные руками.
func TestSubscriptionKeepsVKTurnServers(t *testing.T) {
	vk, err := newVKTurnServer("мой VK", "https://vk.com/call/join/abc", "203.0.113.10:56000", testWG)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: defaultConfig(t.TempDir() + "/olcvpn.json"), log: newLogBus(), tun: newTunnel(t.TempDir(), newLogBus())}
	a.cfg.Servers = []Server{{ID: "old", Name: "старый"}, vk}

	a.applyImport(&importResult{Name: "sub", SubURL: "https://example.org/sub/x", Servers: []Server{{ID: "new", Name: "новый"}}})

	ids := map[string]bool{}
	for _, s := range a.cfg.Servers {
		ids[s.ID] = true
	}
	if !ids["new"] || !ids[vk.ID] || ids["old"] {
		t.Fatalf("после импорта: %v", ids)
	}
	if got := len(a.cfg.subscriptionServers()); got != 1 {
		t.Errorf("серверов подписки = %d, want 1", got)
	}
}

func TestPublicServersHideKeys(t *testing.T) {
	vk, _ := newVKTurnServer("", "https://vk.com/call/join/abc", "203.0.113.10:56000", testWG)
	a := &app{cfg: defaultConfig(t.TempDir() + "/olcvpn.json")}
	a.cfg.Servers = []Server{{ID: "x", Key: "deadbeef"}, vk}

	b, _ := json.Marshal(a.publicServers())
	s := string(b)
	for _, secret := range []string{testPriv, "deadbeef"} {
		if strings.Contains(s, secret) {
			t.Errorf("в ответе для интерфейса есть ключ: %s", secret)
		}
	}
	if a.cfg.Servers[1].WG == nil {
		t.Error("publicServers испортил исходный конфиг")
	}
}

func TestVKTurnSingBoxConfig(t *testing.T) {
	vk, _ := newVKTurnServer("", "https://vk.com/call/join/abc", "203.0.113.10:56000", testWG)
	cfg := defaultConfig(t.TempDir() + "/olcvpn.json")

	for _, tun := range []bool{false, true} {
		b, err := vkturnSingBoxConfig(cfg, &vk, t.TempDir(), tun)
		if err != nil {
			t.Fatal(err)
		}
		var conf map[string]any
		if err := json.Unmarshal(b, &conf); err != nil {
			t.Fatal(err)
		}
		inbounds := conf["inbounds"].([]any)
		if want := map[bool]int{false: 1, true: 2}[tun]; len(inbounds) != want {
			t.Errorf("tun=%v: входов %d, want %d", tun, len(inbounds), want)
		}
		// Клиент VK TURN обязан ходить мимо TUN, иначе петля. А без TUN
		// правило по процессу вредно: проверка IP через прокси ушла бы мимо.
		if got := strings.Contains(string(b), "vkturn-client.exe"); got != tun {
			t.Errorf("tun=%v: правило direct для vkturn-client.exe есть=%v", tun, got)
		}
		// Без перехвата DNS на адрес туннеля у системы не открывается ничего.
		if got := strings.Contains(string(b), `"172.19.0.2/32"`); got != tun {
			t.Errorf("tun=%v: перехват DNS на 172.19.0.2 есть=%v", tun, got)
		}
		// Свои процессы должны уйти напрямую раньше, чем общий перехват DNS.
		if tun {
			s := string(b)
			if strings.Index(s, `"vkturn-client.exe"`) > strings.Index(s, `"protocol": "dns"`) {
				t.Error("правило для vkturn-client.exe стоит после перехвата DNS")
			}
		}
		// WireGuard шлёт на 127.0.0.1 — сокет должен быть на loopback.
		if !strings.Contains(string(b), `"inet4_bind_address": "127.0.0.1"`) {
			t.Errorf("tun=%v: сокет WireGuard не привязан к 127.0.0.1", tun)
		}

		// Самая строгая проверка — сам sing-box, если он лежит рядом.
		sb := filepath.Join("..", "bin", "sing-box.exe")
		if _, err := os.Stat(sb); err != nil {
			continue
		}
		path := filepath.Join(t.TempDir(), "sb.json")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(sb, "check", "-c", path)
		cmd.Env = append(os.Environ(),
			"ENABLE_DEPRECATED_LEGACY_DNS_SERVERS=true",
			"ENABLE_DEPRECATED_MISSING_DOMAIN_RESOLVER=true",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("tun=%v: sing-box check: %v\n%s", tun, err, out)
		}
	}
}
