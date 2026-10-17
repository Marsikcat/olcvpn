//go:build android

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// В Android olcvpn — это тот же бэкенд, что и на ПК, только запускает его
// оболочка приложения (android/). Она же держит VpnService: системный TUN
// создаёт она и передаёт его дескриптор сюда, а интерфейс — та же страница
// web/ в WebView.

const platformName = "android"

// platformApp holds the link to the Android shell.
type platformApp struct {
	shell *shell
}

func (a *app) initPlatform() {
	a.shell = newShell(a)
}

// isAdmin: в Android права администратора не нужны — режим «Весь трафик»
// даёт VpnService, а разрешение на него оболочка спрашивает сама.
func isAdmin() bool { return true }

// childAttr makes a child die with olcvpn — what the job object does in
// Windows. Without it a core could outlive a crashed backend and keep its
// SOCKS port busy for the next one.
func childAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func bindToApp(*os.Process) {}

// libDir is where the shell's native libraries are unpacked. Executables are
// shipped as lib*.so there: the only place an app may run its own binaries
// from since Android 10.
func libDir() string {
	if v := os.Getenv("OLCVPN_LIBDIR"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func coreGlobs(string) []string {
	return []string{filepath.Join(libDir(), "libolcrtc*.so")}
}

const noCoreMsg = "в сборке нет ядра olcrtc — переустановите olcvpn"

func vkturnClient(string) (string, error) {
	client := filepath.Join(libDir(), "libvkturn.so")
	if _, err := os.Stat(client); err != nil {
		return "", fmt.Errorf("в сборке нет клиента VK TURN — переустановите olcvpn")
	}
	return client, nil
}

// ownTraffic: свои процессы в Android в туннель не попадают вовсе —
// оболочка исключает всё приложение из VpnService, так что правило по имени
// процесса не нужно.
func ownTraffic(string) map[string]any { return nil }

// tunInbound uses the interface the shell opened. Addresses and routes go to
// VpnService.Builder as they are; IPv6 is captured too, or apps on a dual-stack
// mobile network would go around the tunnel over it.
func tunInbound() map[string]any {
	return map[string]any{
		"type": "tun", "tag": "tun-in",
		"address":    []string{"172.19.0.1/30", "fdfe:dcba:9876::1/126"},
		"mtu":        tunMTU,
		"auto_route": true, "stack": "gvisor",
	}
}

const tunMTU = 1500

// localDNS is the resolver for what goes around the tunnel. sing-box cannot
// read the system's one in Android (there is no /etc/resolv.conf), so it is
// the network's own DNS the shell reported, or a public one.
func localDNS() map[string]any {
	return map[string]any{"tag": "local", "address": systemDNS()}
}

// autoDetectInterface: в Android соединения olcvpn и так идут мимо
// туннеля, а следить за интерфейсами через netlink приложению не дают.
func autoDetectInterface(bool) bool { return false }

// setCloseHides: трея и кнопки «закрыть» в Android нет.
func setCloseHides(bool) {}

// afterUpdateInstall: APK ставит системный установщик, он сам и закроет
// старую версию.
func (a *app) afterUpdateInstall() {}

// onCaptcha asks the shell to show the VK captcha page in its WebView, or —
// with an empty url — to go back to the app.
func (a *app) onCaptcha(url string) {
	a.shell.send(shellMsg{Op: "captcha", URL: url})
}

// coreName shows libolcrtc.so as plain "olcrtc": the lib…so wrapping is only
// there for the installer.
func coreName(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "lib"), ".so")
}
