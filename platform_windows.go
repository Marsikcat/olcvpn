//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const platformName = "windows"

// platformApp holds the Windows desktop pieces: the tray icon and the
// WebView2 window.
type platformApp struct {
	tray *tray

	// Капча VK: куда переключить окно (пусто — обратно в приложение) и есть
	// ли вообще окно, а не вкладка браузера.
	nav      chan string
	inWindow atomic.Bool
}

func (a *app) initPlatform() {
	a.nav = make(chan string, 4)
}

// isAdmin reports whether the process is elevated. Opening a raw physical
// drive handle succeeds only for administrators, which is enough of a probe.
// Elevation cannot change while the process runs, so it is asked once.
var isAdmin = sync.OnceValue(func() bool {
	f, err := os.Open(`\\.\PHYSICALDRIVE0`)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
})

// childAttr keeps a console helper from flashing a window.
func childAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// coreGlobs are where olcrtc builds are looked for: bin\ and next to the app.
func coreGlobs(dir string) []string {
	return []string{
		filepath.Join(dir, "bin", "olcrtc*.exe"),
		filepath.Join(dir, "olcrtc*.exe"),
	}
}

const noCoreMsg = "рядом с olcvpn.exe нет ни одного ядра olcrtc"

// vkturnClient returns the vk-turn client binary, or why it is missing.
func vkturnClient(dir string) (string, error) {
	client := filepath.Join(dir, "bin", "vkturn-client.exe")
	if _, err := os.Stat(client); err != nil {
		return "", fmt.Errorf("не найден bin\\vkturn-client.exe")
	}
	return client, nil
}

// ownProcesses lists the executables that must bypass the TUN: ours, plus
// every olcrtc core build lying next to the app. A fixed list once missed
// olcrtc-fork.exe — the very core the release ships — so after the TUN came
// up the core's own reconnects went into its own SOCKS and the link looped.
func ownProcesses(dir string) []string {
	names := []string{"olcvpn.exe", "sing-box.exe", "vkturn-client.exe", "olcrtc.exe"}
	seen := map[string]bool{}
	for _, n := range names {
		seen[strings.ToLower(n)] = true
	}
	for _, pat := range coreGlobs(dir) {
		found, _ := filepath.Glob(pat)
		for _, p := range found {
			n := filepath.Base(p)
			if !seen[strings.ToLower(n)] {
				seen[strings.ToLower(n)] = true
				names = append(names, n)
			}
		}
	}
	return names
}

// ownTraffic is the sing-box match for our own processes: in Windows the TUN
// catches everything, so they are told apart by executable name.
func ownTraffic(dir string) map[string]any {
	return map[string]any{"process_name": ownProcesses(dir)}
}

// tunInbound is the system-wide interface sing-box brings up itself.
func tunInbound() map[string]any {
	return map[string]any{
		"type": "tun", "tag": "tun-in", "interface_name": "olcvpn-tun",
		"address":    []string{"172.19.0.1/30"},
		"auto_route": true, "strict_route": true, "stack": "mixed",
	}
}

// localDNS is the resolver for whatever goes around the tunnel: the system's.
func localDNS() map[string]any {
	return map[string]any{"tag": "local", "address": "local"}
}

// autoDetectInterface: with the TUN holding the default route, sing-box must
// bind its direct connections to the real network card itself.
func autoDetectInterface(withTUN bool) bool { return withTUN }

// execSingBox is sing-box run as bin\sing-box.exe.
type execSingBox struct{ cmd *exec.Cmd }

func (p execSingBox) stop() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
}

// runSingBox starts bin\sing-box.exe with conf. The config holds keys, so it
// is written for the current user only.
func (t *Tunnel) runSingBox(conf []byte, _ bool) (singBoxProc, error) {
	sb := filepath.Join(t.dir, "bin", "sing-box.exe")
	if _, err := os.Stat(sb); err != nil {
		return nil, fmt.Errorf("не найден bin\\sing-box.exe")
	}
	confPath := filepath.Join(t.dir, "sing-box-config.json")
	if err := os.WriteFile(confPath, conf, 0o600); err != nil {
		return nil, err
	}

	cmd := exec.Command(sb, "run", "-c", confPath) //nolint:gosec // paths are app-owned
	cmd.Dir = t.dir
	childAttr(cmd)
	cmd.Env = append(os.Environ(),
		"ENABLE_DEPRECATED_LEGACY_DNS_SERVERS=true",
		"ENABLE_DEPRECATED_MISSING_DOMAIN_RESOLVER=true",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	bindToApp(cmd.Process)

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			t.log.add("sing-box: " + strings.TrimRight(sc.Text(), "\r"))
		}
	}()
	return execSingBox{cmd}, nil
}

// setCloseHides makes the window's close button hide it into the tray.
func setCloseHides(on bool) { closeHides.Store(on) }

// afterUpdateInstall quits so the swap script can replace the files: it waits
// for this process to disappear.
func (a *app) afterUpdateInstall() {
	time.Sleep(500 * time.Millisecond)
	if a.tray != nil {
		a.tray.signal(a.tray.quit)
	}
}

// openShare shows the window straight on the share sheet — for the tray menu.
func (a *app) openShare() {
	target := a.url + "#share"
	if a.inWindow.Load() {
		select {
		case a.nav <- target:
		default:
		}
		return
	}
	openBrowser(target)
}

// onCaptcha routes the VK captcha page to wherever the user is looking: the
// app window when there is one, the default browser otherwise. An empty url
// means the captcha is passed and the window can go back to the app.
func (a *app) onCaptcha(url string) {
	if a.inWindow.Load() {
		select {
		case a.nav <- url:
		default:
		}
		return
	}
	if url != "" {
		openBrowser(url)
	}
}

// coreName is how a core build is shown and saved in the settings.
func coreName(path string) string { return filepath.Base(path) }
