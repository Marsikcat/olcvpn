package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// handleVKTurnAdd saves a VK TURN server from what the user typed: the call
// invite, the vk-turn-proxy address and the WireGuard client config.
func (a *app) handleVKTurnAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Link string `json:"link"`
		Peer string `json:"peer"`
		WG   string `json:"wg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	srv, err := newVKTurnServer(body.Name, body.Link, body.Peer, body.WG)
	if err != nil {
		writeErr(w, err)
		return
	}

	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	replaced := false
	for i := range a.cfg.Servers {
		if a.cfg.Servers[i].ID == srv.ID {
			a.cfg.Servers[i] = srv
			replaced = true
		}
	}
	if !replaced {
		a.cfg.Servers = append(a.cfg.Servers, srv)
	}
	a.cfg.SelectedID = srv.ID
	if err := a.cfg.save(); err != nil {
		writeErr(w, err)
		return
	}
	a.log.addf("VK TURN: сервер %q сохранён", srv.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": srv.ID})
}

// handleServerDelete removes a VK TURN entry. Subscription servers are owned
// by the subscription and go away only with it.
func (a *app) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	a.cfgMu.Lock()
	srv, ok := a.cfg.server(body.ID)
	a.cfgMu.Unlock()
	if !ok {
		writeErr(w, fmt.Errorf("сервер не найден"))
		return
	}
	if !srv.isVKTurn() {
		writeErr(w, fmt.Errorf("серверы подписки удаляются вместе с подпиской"))
		return
	}

	// Остановка ждёт процессы до трёх секунд — без замка на настройках.
	if a.tun.serverID() == srv.ID {
		a.tun.Stop()
	}

	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	idx := -1
	for i := range a.cfg.Servers {
		if a.cfg.Servers[i].ID == srv.ID {
			idx = i
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true}) // уже удалён
		return
	}
	a.cfg.Servers = append(a.cfg.Servers[:idx:idx], a.cfg.Servers[idx+1:]...)
	if !a.cfg.hasServer(a.cfg.SelectedID) {
		a.cfg.SelectedID = ""
		if len(a.cfg.Servers) > 0 {
			a.cfg.SelectedID = a.cfg.Servers[0].ID
		}
	}
	if err := a.cfg.save(); err != nil {
		writeErr(w, err)
		return
	}
	a.log.addf("VK TURN: сервер %q удалён", srv.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// publicServers is what the UI gets: everything needed to draw the list, but
// no key material. The page runs locally, yet keys have no business there.
// The caller holds cfgMu.
func (a *app) publicServers() []Server {
	out := make([]Server, 0, len(a.cfg.Servers))
	for _, s := range a.cfg.Servers {
		s.Key = ""
		s.WG = nil
		out = append(out, s)
	}
	return out
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
