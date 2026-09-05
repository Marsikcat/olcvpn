package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type app struct {
	dir  string
	cfg  *Config
	log  *logBus
	tun  *Tunnel
	kind map[string]coreKind
	tray *tray
}

func main() {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	dir := filepath.Dir(exe)
	if v := os.Getenv("OLCVPN_DIR"); v != "" {
		dir = v
	}

	bus := newLogBus()
	a := &app{
		dir:  dir,
		cfg:  loadConfig(dir),
		log:  bus,
		tun:  newTunnel(dir, bus),
		kind: map[string]coreKind{},
	}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/import", a.handleImport)
	mux.HandleFunc("/api/import-qr", a.handleImportQR)
	mux.HandleFunc("/api/settings", a.handleSettings)
	mux.HandleFunc("/api/select", a.handleSelect)
	mux.HandleFunc("/api/connect", a.handleConnect)
	mux.HandleFunc("/api/disconnect", a.handleDisconnect)
	mux.HandleFunc("/api/tun", a.handleTUN)
	mux.HandleFunc("/api/where", a.handleWhere)
	mux.HandleFunc("/api/testip", a.handleTestIP)
	mux.HandleFunc("/api/update/check", a.handleUpdateCheck)
	mux.HandleFunc("/api/update/install", a.handleUpdateInstall)
	mux.HandleFunc("/api/logs", a.handleLogs)

	// Prefer a stable port so the UI is always at the same address; fall back
	// to an ephemeral one when it is taken.
	ln, err := net.Listen("tcp", "127.0.0.1:8899")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatal(err)
		}
	}
	url := fmt.Sprintf("http://%s/", ln.Addr().String())
	_ = os.WriteFile(filepath.Join(dir, "olcvpn.url"), []byte(url+"\n"), 0o600)

	a.log.addf("olcvpn запущен, каталог %s", dir)
	if !isAdmin() {
		a.log.add("права администратора отсутствуют — режим TUN будет недоступен")
	}
	for _, c := range a.cores() {
		a.log.addf("найдено ядро: %s (%s)", c.Name, c.Kind)
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Println(err)
		}
	}()

	fmt.Println("olcvpn:", url)

	if a.cfg.Autoconnect {
		go func() {
			if err := a.connectSelected(); err != nil {
				a.log.addf("автоподключение: %v", err)
			}
		}()
	}

	t := newTray(a)
	a.tray = t
	t.start()
	defer t.stop()

	if browserMode() || !runWindow(url, a, t) {
		// No WebView2 runtime (or --browser asked for it): fall back to the
		// default browser and wait for a signal instead of a window close.
		openBrowser(url)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		select {
		case <-ctx.Done():
		case <-t.quit:
		}
	}

	a.tun.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// browserMode reports whether the user asked for the browser UI instead of the
// app window.
func browserMode() bool {
	for _, arg := range os.Args[1:] {
		if arg == "--browser" || arg == "-browser" {
			return true
		}
	}
	return false
}

func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}

// coreInfo describes one usable olcrtc binary found next to the app.
type coreInfo struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Kind     coreKind `json:"-"`
	KindName string   `json:"kind"`
}

// coreRank orders the discovered binaries so the build that matches the
// server's own source tree is offered first.
func coreRank(name string) int {
	switch {
	case strings.Contains(name, "fork"):
		return 0
	case strings.Contains(name, "core"):
		return 1
	case strings.Contains(name, "master"):
		return 3
	default:
		return 2
	}
}

func (a *app) cores() []coreInfo {
	var candidates []string
	for _, pat := range []string{
		filepath.Join(a.dir, "bin", "olcrtc*.exe"),
		filepath.Join(a.dir, "olcrtc*.exe"),
	} {
		found, _ := filepath.Glob(pat)
		candidates = append(candidates, found...)
	}
	seen := map[string]bool{}
	var out []coreInfo
	for _, p := range candidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Stat(p); err != nil {
			continue
		}
		k, ok := a.kind[p]
		if !ok {
			k = detectCoreKind(p)
			a.kind[p] = k
		}
		if k == coreUnknown {
			continue
		}
		out = append(out, coreInfo{Name: filepath.Base(p), Path: p, Kind: k, KindName: k.String()})
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := coreRank(out[i].Name), coreRank(out[j].Name)
		if ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (a *app) pickCore() (coreInfo, error) {
	cores := a.cores()
	if len(cores) == 0 {
		return coreInfo{}, fmt.Errorf("рядом с olcvpn.exe нет ни одного ядра olcrtc")
	}
	if a.cfg.CoreBinary != "" {
		for _, c := range cores {
			if c.Name == a.cfg.CoreBinary || c.Path == a.cfg.CoreBinary {
				return c, nil
			}
		}
	}
	return cores[0], nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func (a *app) handleState(w http.ResponseWriter, _ *http.Request) {
	ph, detail, peer := a.tun.status()
	core, _ := a.pickCore()
	writeJSON(w, http.StatusOK, map[string]any{
		"phase":       string(ph),
		"detail":      detail,
		"peer":        peer,
		"servers":     a.cfg.Servers,
		"selectedId":  a.cfg.SelectedID,
		"subName":     a.cfg.SubName,
		"subUrl":      a.cfg.SubURL,
		"socksHost":   a.cfg.SocksHost,
		"socksPort":   a.cfg.SocksPort,
		"dns":         a.cfg.DNS,
		"useTun":      a.cfg.UseTUN,
		"directIps":   a.cfg.DirectIPs,
		"directHosts": a.cfg.DirectHosts,
		"cores":       a.cores(),
		"core":        core.Name,
		"admin":       isAdmin(),
		"version":     version,
		"theme":       a.cfg.Theme,
		"autoconnect": a.cfg.Autoconnect,
		"trayClose":   a.cfg.TrayClose,
		"autostart":   autostartEnabled(),
		"tunActive":   a.tun.tunActive(),
		"socksAddr":   fmt.Sprintf("%s:%d", a.cfg.SocksHost, a.cfg.SocksPort),
		"rtt":         a.tun.rtt(),
	})
}

func (a *app) handleUpdateCheck(w http.ResponseWriter, _ *http.Request) {
	info, err := checkUpdate()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleUpdateInstall answers immediately and does the work in the background:
// the download takes long enough that the request would time out, and the
// process exits at the end of it anyway. Progress goes to the log stream.
func (a *app) handleUpdateInstall(w http.ResponseWriter, _ *http.Request) {
	info, err := checkUpdate()
	if err != nil {
		writeErr(w, err)
		return
	}
	if !info.Available {
		writeErr(w, fmt.Errorf("уже установлена последняя версия"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latest": info.Latest})

	go func() {
		if err := a.installUpdate(info); err != nil {
			a.log.addf("обновление: %v", err)
			return
		}
		// Файлы подменит скрипт, как только этот процесс исчезнет.
		time.Sleep(500 * time.Millisecond)
		if a.tray != nil {
			a.tray.signal(a.tray.quit)
		}
	}()
}

func (a *app) handleImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	res, err := importAny(body.Text)
	if err != nil {
		writeErr(w, err)
		return
	}
	a.applyImport(res)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(res.Servers)})
}

func (a *app) handleImportQR(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	text, err := decodeQRFile(strings.Trim(strings.TrimSpace(body.Path), `"`))
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := importAny(text)
	if err != nil {
		writeErr(w, err)
		return
	}
	a.applyImport(res)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(res.Servers)})
}

func (a *app) applyImport(res *importResult) {
	a.cfg.Servers = res.Servers
	if res.SubURL != "" {
		a.cfg.SubURL = res.SubURL
	}
	if res.Name != "" {
		a.cfg.SubName = res.Name
	}
	if a.cfg.SelectedID == "" || a.cfg.selected() == nil {
		if len(res.Servers) > 0 {
			a.cfg.SelectedID = res.Servers[0].ID
		}
	}
	_ = a.cfg.save()
	a.log.addf("импортировано серверов: %d", len(res.Servers))
}

func (a *app) handleSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SocksHost   *string `json:"socksHost"`
		SocksPort   *int    `json:"socksPort"`
		DNS         *string `json:"dns"`
		UseTUN      *bool   `json:"useTun"`
		DirectIPs   *string `json:"directIps"`
		DirectHosts *string `json:"directHosts"`
		Core        *string `json:"core"`
		Theme       *string `json:"theme"`
		Autoconnect *bool   `json:"autoconnect"`
		TrayClose   *bool   `json:"trayClose"`
		Autostart   *bool   `json:"autostart"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	if body.SocksHost != nil {
		a.cfg.SocksHost = *body.SocksHost
	}
	if body.SocksPort != nil && *body.SocksPort > 0 {
		a.cfg.SocksPort = *body.SocksPort
	}
	if body.DNS != nil {
		a.cfg.DNS = *body.DNS
	}
	if body.UseTUN != nil {
		a.cfg.UseTUN = *body.UseTUN
	}
	if body.DirectIPs != nil {
		a.cfg.DirectIPs = *body.DirectIPs
	}
	if body.DirectHosts != nil {
		a.cfg.DirectHosts = *body.DirectHosts
	}
	if body.Core != nil {
		a.cfg.CoreBinary = *body.Core
	}
	if body.Theme != nil {
		a.cfg.Theme = *body.Theme
	}
	if body.Autoconnect != nil {
		a.cfg.Autoconnect = *body.Autoconnect
	}
	if body.TrayClose != nil {
		a.cfg.TrayClose = *body.TrayClose
		closeHides.Store(*body.TrayClose)
	}

	note := ""
	if body.Autostart != nil {
		var err error
		if *body.Autostart {
			note, err = enableAutostart()
		} else {
			err = disableAutostart()
			note = "Автозапуск выключен."
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		a.log.add(note)
	}

	_ = a.cfg.save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "autostartNote": note})
}

func (a *app) handleSelect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	a.cfg.SelectedID = body.ID
	_ = a.cfg.save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *app) handleConnect(w http.ResponseWriter, _ *http.Request) {
	if err := a.connectSelected(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// connectSelected starts the currently selected server. Shared by the UI and
// the tray menu.
func (a *app) connectSelected() error {
	core, err := a.pickCore()
	if err != nil {
		return err
	}
	srv := a.cfg.selected()
	if srv == nil {
		return fmt.Errorf("сначала импортируйте подписку и выберите сервер")
	}
	return a.tun.Start(a.cfg, srv, core.Path, core.Kind, a.cfg.UseTUN)
}

// stateChanged emits whenever the tunnel's phase or TUN state changes, so the
// tray can follow along without polling from several places.
func (a *app) stateChanged() <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		var lastPhase phase
		var lastTUN bool
		for range time.Tick(time.Second) {
			ph, _, _ := a.tun.status()
			tun := a.tun.tunActive()
			if ph == lastPhase && tun == lastTUN {
				continue
			}
			lastPhase, lastTUN = ph, tun
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()
	return out
}

func (a *app) handleDisconnect(w http.ResponseWriter, _ *http.Request) {
	a.tun.Stop()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *app) handleTUN(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	a.cfg.UseTUN = body.On
	_ = a.cfg.save()
	if err := a.tun.SetTUN(a.cfg, body.On); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleWhere reports where traffic currently exits, both through the tunnel
// and around it, so the user can see at a glance whether it took effect.
func (a *app) handleWhere(w http.ResponseWriter, _ *http.Request) {
	addr := fmt.Sprintf("%s:%d", a.cfg.SocksHost, a.cfg.SocksPort)
	through, errT := ipInfoThroughSocks(addr)
	if errT != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": errT.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ip":      through.IP,
		"city":    through.City,
		"country": through.Country,
		"org":     through.Org,
	})
}

func (a *app) handleTestIP(w http.ResponseWriter, _ *http.Request) {
	addr := fmt.Sprintf("%s:%d", a.cfg.SocksHost, a.cfg.SocksPort)
	ip, err := publicIPThroughSocks(addr)
	if err != nil {
		a.log.addf("проверка IP: %v", err)
		writeErr(w, err)
		return
	}
	a.log.addf("внешний IP через туннель: %s", ip)
	writeJSON(w, http.StatusOK, map[string]any{"ip": ip})
}

func (a *app) handleLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for _, line := range a.log.history() {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	flusher.Flush()

	ch, cancel := a.log.subscribe()
	defer cancel()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case line := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
