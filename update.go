package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is stamped at build time:
//
//	go build -ldflags="-X main.version=v0.1.1"
var version = "dev"

const (
	updateRepo    = "Marsikcat/olcvpn"
	updateAPI     = "https://api.github.com/repos/" + updateRepo + "/releases/latest"
	updateMaxSize = 300 << 20 // разумный потолок для архива релиза
)

// release is the part of GitHub's answer we care about.
type release struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// updateInfo is what the UI shows.
type updateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	Notes     string `json:"notes"`
	PageURL   string `json:"pageUrl"`
	AssetURL  string `json:"assetUrl"`
	AssetName string `json:"assetName"`
	AssetSize int64  `json:"assetSize"`
}

// updateClient goes straight out, never through the tunnel the app itself
// manages: an update must stay reachable exactly when the tunnel is broken.
var updateClient = &http.Client{
	Timeout:   3 * time.Minute,
	Transport: &http.Transport{Proxy: nil},
}

func checkUpdate() (updateInfo, error) {
	info := updateInfo{Current: version}

	req, err := http.NewRequest(http.MethodGet, updateAPI, nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "olcvpn/"+version)

	resp, err := updateClient.Do(req)
	if err != nil {
		return info, fmt.Errorf("не удалось связаться с GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("GitHub ответил HTTP %d", resp.StatusCode)
	}

	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return info, err
	}

	info.Latest = rel.TagName
	info.Notes = rel.Body
	info.PageURL = rel.HTMLURL
	info.Available = newerVersion(rel.TagName, version)

	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, "windows-amd64.zip") {
			info.AssetURL, info.AssetName, info.AssetSize = a.URL, a.Name, a.Size
			break
		}
	}
	if info.Available && info.AssetURL == "" {
		return info, fmt.Errorf("в релизе %s нет архива для Windows", rel.TagName)
	}
	return info, nil
}

// newerVersion compares two vX.Y.Z tags numerically. A build that was not
// stamped with a version ("dev") is always treated as older, so a developer
// build still sees what is published.
func newerVersion(latest, current string) bool {
	if latest == "" {
		return false
	}
	if current == "dev" || current == "" {
		return true
	}
	l, c := splitVersion(latest), splitVersion(current)
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func splitVersion(v string) [3]int {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		digits := parts[i]
		if idx := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); idx >= 0 {
			digits = digits[:idx]
		}
		out[i], _ = strconv.Atoi(digits)
	}
	return out
}

// installUpdate downloads the release archive, unpacks it beside the app and
// hands the swap to a small script.
//
// A running executable cannot overwrite itself, so the actual copy has to
// happen after this process is gone — the script waits for the PID to
// disappear, replaces the files and starts the new build.
func (a *app) installUpdate(info updateInfo) error {
	if info.AssetURL == "" {
		return fmt.Errorf("нечего скачивать")
	}

	a.log.addf("обновление: качаю %s (%.1f МБ)", info.AssetName, float64(info.AssetSize)/(1<<20))

	tmp, err := os.CreateTemp("", "olcvpn-update-*.zip")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	resp, err := updateClient.Get(info.AssetURL)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("скачивание не удалось: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("скачивание не удалось: HTTP %d", resp.StatusCode)
	}
	if _, err := io.Copy(tmp, io.LimitReader(resp.Body, updateMaxSize)); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	stage := filepath.Join(a.dir, "update")
	_ = os.RemoveAll(stage)
	root, err := unzip(tmpName, stage)
	if err != nil {
		return fmt.Errorf("архив не распаковался: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "olcvpn.exe")); err != nil {
		return fmt.Errorf("в архиве нет olcvpn.exe")
	}

	a.log.add("обновление: распаковано, перезапускаюсь")
	a.tun.Stop()

	if err := writeSwapScript(a.dir, root); err != nil {
		return err
	}
	return launchSwapScript(a.dir)
}

// unzip extracts the archive and returns the directory that actually holds the
// files: release archives wrap everything in one versioned folder.
func unzip(archive, dest string) (string, error) {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return "", err
	}
	defer r.Close()

	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return "", fmt.Errorf("подозрительный путь в архиве: %s", f.Name)
		}
		target := filepath.Join(dest, name)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}

		src, err := f.Open()
		if err != nil {
			return "", err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			src.Close()
			return "", err
		}
		_, err = io.Copy(out, io.LimitReader(src, updateMaxSize))
		src.Close()
		out.Close()
		if err != nil {
			return "", err
		}
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(dest, entries[0].Name()), nil
	}
	return dest, nil
}

const swapScript = `@echo off
rem Подменяет файлы olcvpn после выхода старого процесса и запускает новый.
:wait
tasklist /FI "PID eq %d" 2>nul | find "%d" >nul
if not errorlevel 1 (
  timeout /t 1 /nobreak >nul
  goto wait
)
xcopy /E /Y /I /Q "%s\*" "%s" >nul
rd /s /q "%s" >nul 2>&1
start "" "%s"
del "%%~f0"
`

func writeSwapScript(dir, root string) error {
	pid := os.Getpid()
	body := fmt.Sprintf(swapScript,
		pid, pid,
		root, dir,
		filepath.Join(dir, "update"),
		filepath.Join(dir, "olcvpn.exe"),
	)
	return os.WriteFile(filepath.Join(dir, "olcvpn-update.cmd"), []byte(body), 0o755)
}

func launchSwapScript(dir string) error {
	script := filepath.Join(dir, "olcvpn-update.cmd")
	cmd := exec.Command("cmd", "/c", script) //nolint:gosec // path is app-owned
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}
