package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"image/png"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// Ядро из релиза называется olcrtc-fork.exe; если его нет в правиле
// «напрямую», в режиме TUN оно заворачивает собственные соединения в себя же.
func TestSingBoxBypassesShippedCore(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"olcrtc-fork.exe", "olcrtc-master.exe"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := singBoxConfig(defaultConfig(filepath.Join(dir, "olcvpn.json")), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"olcrtc-fork.exe", "olcrtc-master.exe", "olcvpn.exe", "sing-box.exe"} {
		if !strings.Contains(string(b), `"`+n+`"`) {
			t.Errorf("%s не идёт мимо туннеля", n)
		}
	}
}

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

func TestTrayStates(t *testing.T) {
	for ph, want := range map[phase]trayState{
		phaseStopped: trayOff, phaseStarting: trayBusy, phaseCaptcha: trayBusy,
		phaseWaiting: trayBusy, phaseProxy: trayOn, phaseConnected: trayOn, phaseError: trayErr,
	} {
		if got := trayStateOf(ph); got != want {
			t.Errorf("%s: %d, want %d", ph, got, want)
		}
	}
}

func TestTrayIcons(t *testing.T) {
	icons := trayIcons()
	if len(icons) != 4 {
		t.Fatalf("вариантов иконки %d, want 4", len(icons))
	}
	// Самый большой кадр каждого варианта: угол с точкой и середина логотипа.
	frame := func(ico []byte) (int, func(x, y int) (r, g, b, a uint8)) {
		var hdr [3]uint16
		_ = binary.Read(bytes.NewReader(ico), binary.LittleEndian, &hdr)
		if int(hdr[2]) != len(trayIconSizes) {
			t.Fatalf("кадров %d, want %d", hdr[2], len(trayIconSizes))
		}
		last := 6 + 16*(len(trayIconSizes)-1)
		size := binary.LittleEndian.Uint32(ico[last+8:])
		off := binary.LittleEndian.Uint32(ico[last+12:])
		img, err := png.Decode(bytes.NewReader(ico[off : off+size]))
		if err != nil {
			t.Fatal(err)
		}
		return img.Bounds().Dx(), func(x, y int) (uint8, uint8, uint8, uint8) {
			r, g, b, a := img.At(x, y).RGBA()
			return uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)
		}
	}

	n, on := frame(icons[trayOn])
	dot := int(float64(n) * 0.70) // внутри точки
	if r, g, b, _ := on(dot, dot); !(g > 150 && r < 120 && b < 150) {
		t.Errorf("«подключено»: в углу не зелёная точка, а %d,%d,%d", r, g, b)
	}
	_, busy := frame(icons[trayBusy])
	if r, g, b, _ := busy(dot, dot); !(r > 180 && g > 120 && b < 100) {
		t.Errorf("«подключение»: в углу не жёлтая точка, а %d,%d,%d", r, g, b)
	}
	_, off := frame(icons[trayOff])
	if r, g, b, _ := off(n/3, n/3); r != g || g != b {
		t.Errorf("«отключено»: логотип не серый: %d,%d,%d", r, g, b)
	}
	if _, _, _, a := off(dot, dot); a == 0 {
		t.Error("«отключено»: логотип пропал")
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

// Задача автозапуска: XML разбирается и не мешает работе от батареи.
func TestAutostartTaskXML(t *testing.T) {
	body := autostartTaskXML(`PC\Иван & Co`, `C:\Program Files\olcvpn\olcvpn.exe`)
	var task struct {
		Settings struct {
			DisallowStartIfOnBatteries bool
			StopIfGoingOnBatteries     bool
			ExecutionTimeLimit         string
		}
		Actions struct {
			Exec struct{ Command, WorkingDirectory string }
		}
	}
	dec := xml.NewDecoder(strings.NewReader(body))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	if err := dec.Decode(&task); err != nil {
		t.Fatal(err)
	}
	s := task.Settings
	if s.DisallowStartIfOnBatteries || s.StopIfGoingOnBatteries || s.ExecutionTimeLimit != "PT0S" {
		t.Errorf("условия задачи: %+v", s)
	}
	if task.Actions.Exec.Command != `"C:\Program Files\olcvpn\olcvpn.exe"` ||
		task.Actions.Exec.WorkingDirectory != `C:\Program Files\olcvpn` {
		t.Errorf("действие: %+v", task.Actions.Exec)
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
