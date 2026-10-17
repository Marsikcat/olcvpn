//go:build android

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// updateAsset is the release file this build updates from: the APK for its
// processor.
var updateAsset = "android-" + runtime.GOARCH + ".apk"

const updateAssetMissing = "в релизе %s нет APK для Android"

// installUpdate downloads the APK and hands it to the shell, which opens the
// system installer: replacing an app is the system's job, and it asks the
// user itself. Settings stay — they live in the app's data, not in the APK.
func (a *app) installUpdate(info updateInfo) error {
	if info.AssetURL == "" {
		return fmt.Errorf("нечего скачивать")
	}
	a.log.addf("обновление: качаю %s (%.1f МБ)", info.AssetName, float64(info.AssetSize)/(1<<20))

	stage := filepath.Join(a.dir, "update")
	_ = os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	apk := filepath.Join(stage, "olcvpn.apk")
	f, err := os.OpenFile(apk, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = downloadUpdate(info, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(apk)
		return err
	}

	a.log.add("обновление: скачано, открываю установщик")
	a.shell.send(shellMsg{Op: "install", Path: apk})
	return nil
}
