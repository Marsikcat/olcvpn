//go:build windows

package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// updateAsset is the release file this build updates from.
const updateAsset = "windows-amd64.zip"

const updateAssetMissing = "в релизе %s нет архива для Windows"

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
	err = downloadUpdate(info, tmp)
	tmp.Close()
	if err != nil {
		return err
	}

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
	childAttr(cmd)
	return cmd.Start()
}
