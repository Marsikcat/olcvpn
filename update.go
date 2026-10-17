package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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
		if strings.HasSuffix(a.Name, updateAsset) {
			info.AssetURL, info.AssetName, info.AssetSize = a.URL, a.Name, a.Size
			break
		}
	}
	if info.Available && info.AssetURL == "" {
		return info, fmt.Errorf(updateAssetMissing, rel.TagName)
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

// watchUpdates checks once at startup and, if that failed because nothing was
// reachable yet, again as soon as the tunnel first carries traffic. A machine
// that boots with the app is usually offline for the first seconds, and on a
// censored network the tunnel may be the only way out at all — so the moment
// it comes up is exactly when the check has a chance to succeed.
func (a *app) watchUpdates() {
	if a.tryUpdateCheck() {
		return
	}
	a.log.add("обновления: сети нет, проверю после подключения")

	done := make(chan struct{})
	defer close(done)
	for range a.stateChanged(done) {
		if _, _, peer := a.tun.status(); !peer {
			continue
		}
		if a.tryUpdateCheck() {
			return
		}
	}
}

// tryUpdateCheck reports whether GitHub answered at all; an available update is
// remembered for the UI and mentioned in the log.
func (a *app) tryUpdateCheck() bool {
	info, err := checkUpdate()
	if err != nil {
		return false
	}

	a.updMu.Lock()
	a.upd = info
	a.updMu.Unlock()

	if info.Available {
		a.log.addf("доступно обновление %s (установлена %s)", info.Latest, info.Current)
	} else {
		a.log.addf("обновлений нет, установлена %s", info.Current)
	}
	return true
}

// lastUpdateCheck returns what the background check found, if anything.
func (a *app) lastUpdateCheck() updateInfo {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	return a.upd
}

// downloadUpdate saves the release asset into dst. It never goes through the
// tunnel: see updateClient.
func downloadUpdate(info updateInfo, dst io.Writer) error {
	resp, err := updateClient.Get(info.AssetURL)
	if err != nil {
		return fmt.Errorf("скачивание не удалось: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("скачивание не удалось: HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(dst, io.LimitReader(resp.Body, updateMaxSize))
	return err
}
