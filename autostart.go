package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
)

const (
	autostartTask   = "olcvpn"
	autostartRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
)

// run executes a console helper without flashing a window.
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // fixed system utilities
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// autostartEnabled reports whether either mechanism is currently registered.
// It spawns schtasks and reg, so callers cache the answer.
func autostartEnabled() bool {
	if _, err := run("schtasks", "/Query", "/TN", autostartTask); err == nil {
		return true
	}
	if out, err := run("reg", "query", autostartRunKey, "/v", autostartTask); err == nil {
		return strings.Contains(out, autostartTask)
	}
	return false
}

// enableAutostart registers the app to start at logon.
//
// A scheduled task is preferred over the Run key because only a task can carry
// "run with highest privileges" — and without elevation the TUN mode cannot
// come up, so a Run-key autostart would silently downgrade to proxy-only.
// Creating such a task itself needs elevation, hence the fallback.
func enableAutostart() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}

	if isAdmin() {
		if err := createAutostartTask(exe); err == nil {
			return "Задача создана: запуск при входе с правами администратора, TUN будет доступен.", nil
		}
	}

	if out, err := run("reg", "add", autostartRunKey,
		"/v", autostartTask, "/t", "REG_SZ", "/d", `"`+exe+`"`, "/f"); err != nil {
		return "", fmt.Errorf("не удалось прописать автозапуск: %s", strings.TrimSpace(out))
	}
	return "Автозапуск включён, но без прав администратора: " +
		"после входа будет доступен только режим «Только прокси». " +
		"Включите его из start.cmd, чтобы получить задачу с правами.", nil
}

// createAutostartTask registers the logon task from XML.
//
// «schtasks /Create /SC ONLOGON» оставляет условия по умолчанию, а они для
// программы, которая работает всё время, не годятся: от батареи задача не
// стартует вовсе, при отключении зарядки Windows её убивает, а через трое
// суток останавливает по лимиту времени. На ноутбуке olcvpn пропадал бы,
// стоило выдернуть шнур.
func createAutostartTask(exe string) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	body := autostartTaskXML(u.Username, exe)

	// schtasks надёжно читает XML только в UTF-16 с BOM.
	units := utf16.Encode([]rune(body))
	buf := make([]byte, 2, 2+2*len(units))
	buf[0], buf[1] = 0xFF, 0xFE
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	f, err := os.CreateTemp("", "olcvpn-task-*.xml")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	_, err = f.Write(buf)
	f.Close()
	if err != nil {
		return err
	}

	if out, err := run("schtasks", "/Create", "/TN", autostartTask, "/XML", path, "/F"); err != nil {
		return fmt.Errorf("schtasks: %s", strings.TrimSpace(out))
	}
	return nil
}

// autostartTaskXML fills the task template for user and exe.
func autostartTaskXML(username, exe string) string {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return fmt.Sprintf(autostartXML, esc(username), esc(username), esc(`"`+exe+`"`), esc(filepath.Dir(exe)))
}

const autostartXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>olcvpn: запуск при входе в Windows</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <WorkingDirectory>%s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`

// repairAutostart rewrites a logon task made by older versions, which kept
// the battery conditions and the three-day limit. Only an elevated process
// can do that; otherwise the old task stays until the user toggles autostart.
func (a *app) repairAutostart() {
	if !isAdmin() {
		return
	}
	out, err := run("schtasks", "/Query", "/TN", autostartTask, "/XML")
	if err != nil {
		return
	}
	if !strings.Contains(out, "<DisallowStartIfOnBatteries>true") &&
		!strings.Contains(out, "<StopIfGoingOnBatteries>true") &&
		strings.Contains(out, "<ExecutionTimeLimit>PT0S") {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if err := createAutostartTask(exe); err != nil {
		a.log.addf("автозапуск: не удалось обновить задачу: %v", err)
		return
	}
	a.log.add("автозапуск: задача обновлена — теперь работает и от батареи, без лимита в трое суток")
}

// disableAutostart removes both mechanisms, whichever is present.
func disableAutostart() error {
	var failed []string
	if _, err := run("schtasks", "/Query", "/TN", autostartTask); err == nil {
		if out, err := run("schtasks", "/Delete", "/TN", autostartTask, "/F"); err != nil {
			failed = append(failed, strings.TrimSpace(out))
		}
	}
	if _, err := run("reg", "query", autostartRunKey, "/v", autostartTask); err == nil {
		if out, err := run("reg", "delete", autostartRunKey, "/v", autostartTask, "/f"); err != nil {
			failed = append(failed, strings.TrimSpace(out))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("не удалось снять автозапуск: %s", strings.Join(failed, "; "))
	}
	return nil
}
