//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
