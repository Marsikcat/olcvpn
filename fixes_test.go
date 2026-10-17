package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// Сессия, которую уже закрыл Stop(), не должна при выходе процесса трогать
// следующую: раньше она объявляла её упавшей и гасила её sing-box.
func TestStaleWaitLeavesNewSessionAlone(t *testing.T) {
	cmd := exec.Command("ping", "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skip("нечем запустить фоновый процесс:", err)
	}
	tun := newTunnel(t.TempDir(), newLogBus())
	tun.core = cmd
	done := make(chan struct{})
	go func() { tun.wait(cmd); close(done) }()

	tun.mu.Lock()
	tun.core = nil // так делает Stop()
	tun.mu.Unlock()
	tun.setPhase(phaseProxy, "новое подключение")

	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wait не вернулся")
	}
	if ph, _, _ := tun.status(); ph != phaseProxy {
		t.Errorf("фаза стала %q, а должна была остаться proxy", ph)
	}
}

func TestConfigCloneIsDetached(t *testing.T) {
	c := defaultConfig(filepath.Join(t.TempDir(), "olcvpn.json"))
	vk, _ := newVKTurnServer("", "https://vk.com/call/join/abc", "203.0.113.10:56000", testWG)
	c.Servers = []Server{vk}
	cp := c.clone()
	cp.Servers[0].WG.PrivateKey = "changed"
	cp.SocksPort = 1
	if c.Servers[0].WG.PrivateKey == "changed" || c.SocksPort == 1 {
		t.Error("копия делит данные с оригиналом")
	}
	if cp.Servers[0].WG.PeerPublicKey != testPub {
		t.Error("копия потеряла ключи WireGuard")
	}
}

func TestDecodeQRBase64(t *testing.T) {
	const want = "https://example.org/sub/abc"
	m, err := qrcode.NewQRCodeWriter().Encode(want, gozxing.BarcodeFormat_QR_CODE, 200, 200, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatal(err)
	}
	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	got, err := decodeQRBase64(data)
	if err != nil || got != want {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestVKInteresting(t *testing.T) {
	for line, want := range map[string]bool{
		"listen udp 127.0.0.1:9000: bind: Only one usage of each socket address": true,
		"[STREAM 3] Closed DTLS connection":                                      true,
		"[Captcha Proxy] HTTP GET /not_robot_captcha?session_token=eyJh":         false,
		"[STREAM 9] failed to close TURN allocated connection: write tcp":        false,
		"[STREAM 1] [VK Auth] Connecting Identity - Name: Елена":                 false,
	} {
		if got := vkInteresting(line); got != want {
			t.Errorf("%q: %v, want %v", line, got, want)
		}
	}
}

// Состояние для окна собирается под замком и без ключей.
func TestStateHasNoKeys(t *testing.T) {
	vk, _ := newVKTurnServer("", "https://vk.com/call/join/abc", "203.0.113.10:56000", testWG)
	a := &app{cfg: defaultConfig(filepath.Join(t.TempDir(), "olcvpn.json")), log: newLogBus(),
		tun: newTunnel(t.TempDir(), newLogBus()), kind: map[string]coreKind{}}
	a.cfg.Servers = []Server{vk}
	w := httptest.NewRecorder()
	a.handleState(w, nil)
	var st map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.Body.String(), testPriv) {
		t.Error("в состоянии есть закрытый ключ")
	}
	if st["phase"] != "stopped" {
		t.Errorf("phase = %v", st["phase"])
	}
}
