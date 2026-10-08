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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type app struct {
	dir  string
	log  *logBus
	tun  *Tunnel
	tray *tray

	// cfgMu охраняет cfg. HTTP-обработчики работают параллельно, а с ними —
	// обновление подписки при запуске, автоподключение и трей; без замка
	// опрос состояния раз в полторы секунды читал список серверов в тот
	// самый миг, когда его подменял импорт.
	cfgMu sync.Mutex
	cfg   *Config

	kindMu sync.Mutex
	kind   map[string]coreKind

	// autostart кешируется: проверка запускает schtasks и reg, а окно
	// спрашивает состояние каждые полторы секунды, даже свёрнутое в трей.
	autostart atomic.Bool

	updMu sync.Mutex
	upd   updateInfo

	// Капча VK: куда переключить окно (пусто — обратно в приложение) и есть
	// ли вообще окно, а не вкладка браузера.
	nav      chan string
	inWindow atomic.Bool
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
		nav:  make(chan string, 4),
	}
	a.tun.onCaptcha = a.onCaptcha

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
	mux.HandleFunc("/api/subscription/delete", a.handleDeleteSubscription)
	mux.HandleFunc("/api/subscription/refresh", a.handleRefreshSubscription)
	mux.HandleFunc("/api/vkturn/add", a.handleVKTurnAdd)
	mux.HandleFunc("/api/server/delete", a.handleServerDelete)
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

	srv := &http.Server{Handler: localOnly(ln.Addr().String(), mux), ReadHeaderTimeout: 10 * time.Second}
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

	go a.watchUpdates()
	go a.refreshOnStart()
	go func() {
		a.autostart.Store(autostartEnabled())
		a.repairAutostart()
	}()

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
		a.kindMu.Lock()
		k, ok := a.kind[p]
		a.kindMu.Unlock()
		if !ok {
			k = detectCoreKind(p)
			a.kindMu.Lock()
			a.kind[p] = k
			a.kindMu.Unlock()
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
	a.cfgMu.Lock()
	want := a.cfg.CoreBinary
	a.cfgMu.Unlock()
	return a.pickCoreNamed(want)
}

func (a *app) pickCoreNamed(want string) (coreInfo, error) {
	cores := a.cores()
	if len(cores) == 0 {
		return coreInfo{}, fmt.Errorf("рядом с olcvpn.exe нет ни одного ядра olcrtc")
	}
	if want != "" {
		for _, c := range cores {
			if c.Name == want || c.Path == want {
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
	a.cfgMu.Lock()
	st := map[string]any{
		"servers":     a.publicServers(),
		"selectedId":  a.cfg.SelectedID,
		"subName":     a.cfg.SubName,
		"subUrl":      a.cfg.SubURL,
		"subUpdated":  a.cfg.SubUpdated,
		"socksHost":   a.cfg.SocksHost,
		"socksPort":   a.cfg.SocksPort,
		"dns":         a.cfg.DNS,
		"useTun":      a.cfg.UseTUN,
		"directIps":   a.cfg.DirectIPs,
		"directHosts": a.cfg.DirectHosts,
		"theme":       a.cfg.Theme,
		"autoconnect": a.cfg.Autoconnect,
		"trayClose":   a.cfg.TrayClose,
		"startHidden": a.cfg.StartHidden,
		"socksAddr":   fmt.Sprintf("%s:%d", a.cfg.SocksHost, a.cfg.SocksPort),
	}
	coreWant := a.cfg.CoreBinary
	a.cfgMu.Unlock()

	ph, detail, peer := a.tun.status()
	core, _ := a.pickCoreNamed(coreWant)
	upd := a.lastUpdateCheck()
	st["phase"] = string(ph)
	st["detail"] = detail
	st["peer"] = peer
	st["captchaUrl"] = a.tun.pendingCaptcha()
	st["cores"] = a.cores()
	st["core"] = core.Name
	st["admin"] = isAdmin()
	st["version"] = version
	st["autostart"] = a.autostart.Load()
	st["updateLatest"] = upd.Latest
	st["updateAvailable"] = upd.Available
	st["tunActive"] = a.tun.tunActive()
	st["rtt"] = a.tun.rtt()
	writeJSON(w, http.StatusOK, st)
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(res.Servers), "note": res.Note})
}

func (a *app) handleImportQR(w http.ResponseWriter, r *http.Request) {
	// Картинку окно присылает целиком, base64 в JSON: путь к файлу
	// пользователю пришлось бы вводить руками.
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	var body struct {
		Path string `json:"path"`
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	var (
		text string
		err  error
	)
	if body.Data != "" {
		text, err = decodeQRBase64(body.Data)
	} else {
		text, err = decodeQRFile(strings.Trim(strings.TrimSpace(body.Path), `"`))
	}
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(res.Servers), "note": res.Note})
}

// applyImport replaces the current subscription with the imported one.
//
// Everything about the old one goes: keeping its address while showing the new
// servers would make a later refresh silently pull the old list back.
func (a *app) applyImport(res *importResult) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	a.applyImportLocked(res)
}

// applyImportLocked is applyImport for callers already holding cfgMu.
func (a *app) applyImportLocked(res *importResult) {
	// Серверы VK TURN добавлены руками и к подписке отношения не имеют.
	a.cfg.Servers = append(append([]Server{}, res.Servers...), a.cfg.manualServers()...)
	a.cfg.SubURL = res.SubURL
	a.cfg.SubName = res.Name
	a.cfg.SubUpdated = time.Time{}
	if res.SubURL != "" {
		a.cfg.SubUpdated = time.Now()
	}
	if !a.cfg.hasServer(a.cfg.SelectedID) && len(res.Servers) > 0 {
		a.cfg.SelectedID = res.Servers[0].ID
	}
	_ = a.cfg.save()
	a.log.addf("импортировано серверов: %d", len(res.Servers))
	if res.Note != "" {
		a.log.add(res.Note)
	}
}

// handleDeleteSubscription forgets the subscription and everything derived
// from it. The tunnel goes down first: it is running on one of these servers.
func (a *app) handleDeleteSubscription(w http.ResponseWriter, _ *http.Request) {
	// Повторный клик, пока окно не успело обновиться, не должен писать в
	// журнал «подписка удалена» на пустом месте.
	a.cfgMu.Lock()
	empty := len(a.cfg.subscriptionServers()) == 0 && a.cfg.SubURL == ""
	a.cfgMu.Unlock()
	if empty {
		writeErr(w, fmt.Errorf("подписка не добавлена"))
		return
	}
	// Остановка ждёт ядро до трёх секунд — без замка, чтобы окно не
	// подвисало на опросе состояния.
	if a.tun.running() && !a.tun.onVKTurn() {
		a.tun.Stop()
	}

	a.cfgMu.Lock()
	name := a.cfg.SubName
	a.cfg.Servers = a.cfg.manualServers()
	if !a.cfg.hasServer(a.cfg.SelectedID) {
		a.cfg.SelectedID = ""
	}
	a.cfg.SubURL = ""
	a.cfg.SubName = ""
	a.cfg.SubUpdated = time.Time{}
	err := a.cfg.save()
	a.cfgMu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}

	// Сгенерированный конфиг ядра хранит ключ последнего сервера — после
	// удаления подписки ему незачем лежать на диске.
	_ = os.Remove(filepath.Join(a.dir, "core-config.yaml"))

	if name == "" {
		name = "без названия"
	}
	a.log.addf("подписка удалена: %s", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
		StartHidden *bool   `json:"startHidden"`
		Autostart   *bool   `json:"autostart"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	if body.SocksPort != nil && (*body.SocksPort < 1 || *body.SocksPort > 65535) {
		writeErr(w, fmt.Errorf("порт прокси должен быть от 1 до 65535"))
		return
	}

	// Автозапуск — это вызовы schtasks и reg, их делаем без замка.
	note := ""
	if body.Autostart != nil {
		var err error
		if *body.Autostart {
			note, err = enableAutostart()
		} else {
			err = disableAutostart()
			note = "Автозапуск выключен."
		}
		a.autostart.Store(autostartEnabled())
		if err != nil {
			writeErr(w, err)
			return
		}
		a.log.add(note)
	}

	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if body.SocksHost != nil {
		a.cfg.SocksHost = *body.SocksHost
	}
	if body.SocksPort != nil {
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
	if body.StartHidden != nil {
		a.cfg.StartHidden = *body.StartHidden
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
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if !a.cfg.hasServer(body.ID) {
		writeErr(w, fmt.Errorf("сервер не найден"))
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
	// Туннель читает настройки и после старта — когда поднимает sing-box, —
	// поэтому получает свою копию, а не общий конфиг.
	a.cfgMu.Lock()
	snap := a.cfg.clone()
	a.cfgMu.Unlock()

	srv := snap.selected()
	if srv == nil {
		return fmt.Errorf("сначала импортируйте подписку или добавьте VK TURN и выберите сервер")
	}
	if srv.isVKTurn() {
		return a.tun.StartVKTurn(snap, srv, snap.UseTUN)
	}
	core, err := a.pickCoreNamed(snap.CoreBinary)
	if err != nil {
		return err
	}
	return a.tun.Start(snap, srv, core.Path, core.Kind, snap.UseTUN)
}

// socksAddr is the local proxy address the tunnel listens on.
func (a *app) socksAddr() string {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return fmt.Sprintf("%s:%d", a.cfg.SocksHost, a.cfg.SocksPort)
}

// stateChanged emits whenever the tunnel's phase or TUN state changes, so the
// tray can follow along without polling from several places. It stops when
// done is closed; nil means never.
func (a *app) stateChanged(done <-chan struct{}) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		var lastPhase phase
		var lastTUN bool
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
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
	a.cfgMu.Lock()
	a.cfg.UseTUN = body.On
	_ = a.cfg.save()
	snap := a.cfg.clone()
	a.cfgMu.Unlock()
	if err := a.tun.SetTUN(snap, body.On); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleWhere reports where traffic currently exits, both through the tunnel
// and around it, so the user can see at a glance whether it took effect.
func (a *app) handleWhere(w http.ResponseWriter, _ *http.Request) {
	through, errT := ipInfoThroughSocks(a.socksAddr())
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
	ip, err := publicIPThroughSocks(a.socksAddr())
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

// trayMode reports whether the app was asked to start straight into the tray
// for this launch only, without touching the saved setting. The autostart
// entry can use it to come up quietly while a manual launch still shows the
// window.
func trayMode() bool {
	for _, arg := range os.Args[1:] {
		if arg == "--tray" || arg == "-tray" {
			return true
		}
	}
	return false
}
