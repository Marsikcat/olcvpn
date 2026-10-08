package main

import (
	"fmt"
	"net/http"
	"strings"
)

// refreshDiff is what changed between the stored server list and the one the
// panel serves now.
type refreshDiff struct {
	Count   int    `json:"count"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Changed int    `json:"changed"`
	Note    string `json:"note,omitempty"`
	// Reconnect is set when the server the tunnel is running on was removed
	// or changed: the running core still uses the old key or room.
	Reconnect bool `json:"reconnect"`
}

func (d refreshDiff) String() string {
	if d.Added == 0 && d.Removed == 0 && d.Changed == 0 {
		return fmt.Sprintf("подписка актуальна, серверов: %d", d.Count)
	}
	var parts []string
	if d.Added > 0 {
		parts = append(parts, fmt.Sprintf("новых %d", d.Added))
	}
	if d.Changed > 0 {
		parts = append(parts, fmt.Sprintf("изменилось %d", d.Changed))
	}
	if d.Removed > 0 {
		parts = append(parts, fmt.Sprintf("удалено %d", d.Removed))
	}
	return "подписка обновлена: " + strings.Join(parts, ", ")
}

// refreshSubscription pulls the server list again from the stored address.
//
// Servers are matched by ID (carrier, room and transport), so a server whose
// key or client ID was rotated on the panel counts as changed rather than as
// one removed and one added — and stays selected.
func (a *app) refreshSubscription() (refreshDiff, error) {
	a.cfgMu.Lock()
	subURL := a.cfg.SubURL
	hasSubs := len(a.cfg.subscriptionServers()) > 0
	a.cfgMu.Unlock()
	if subURL == "" && !hasSubs {
		return refreshDiff{}, fmt.Errorf("подписка не добавлена")
	}
	if subURL == "" {
		return refreshDiff{}, fmt.Errorf(
			"обновлять нечего: серверы добавлены ссылкой на сервер, а не подпиской")
	}

	// Сеть — без замка: панель может отвечать секундами.
	res, err := fetchSubscription(subURL)
	if err != nil {
		return refreshDiff{}, err
	}

	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	// Пока шёл запрос, подписку могли удалить или заменить другой — тогда
	// этот ответ устарел, и применять его значит воскресить старую.
	if a.cfg.SubURL != subURL {
		return refreshDiff{}, fmt.Errorf("подписка сменилась, пока обновлялась — обновите ещё раз")
	}

	subs := a.cfg.subscriptionServers()
	old := make(map[string]Server, len(subs))
	for _, s := range subs {
		old[s.ID] = s
	}
	// Сравниваем с сервером, на котором туннель работает на самом деле, а
	// не с выбранным: выбор могли сменить уже после подключения.
	running := a.tun.serverID()

	d := refreshDiff{Count: len(res.Servers), Note: res.Note}
	seen := make(map[string]bool, len(res.Servers))
	for _, s := range res.Servers {
		seen[s.ID] = true
		prev, ok := old[s.ID]
		switch {
		case !ok:
			d.Added++
		case prev != s:
			d.Changed++
			if s.ID == running {
				d.Reconnect = true
			}
		}
	}
	for id := range old {
		if !seen[id] {
			d.Removed++
			if id == running {
				d.Reconnect = true
			}
		}
	}

	// Имя приходит из ссылки на подписку, а не из самого списка: если список
	// своего #name: не несёт, оставляем прежнее.
	if res.Name == "" {
		res.Name = a.cfg.SubName
	}
	a.applyImportLocked(res)

	a.log.add(d.String())
	if d.Reconnect {
		a.log.add("сервер текущего подключения изменился — переподключитесь, чтобы применить")
	}
	return d, nil
}

func (a *app) handleRefreshSubscription(w http.ResponseWriter, _ *http.Request) {
	d, err := a.refreshSubscription()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// refreshOnStart brings the list up to date once per launch. Panels rotate keys
// and move rooms; a list that is a week old may point at nothing. Failure is
// not an error here — the stored list keeps working until the panel answers.
func (a *app) refreshOnStart() {
	a.cfgMu.Lock()
	subURL := a.cfg.SubURL
	a.cfgMu.Unlock()
	if subURL == "" {
		return
	}
	if _, err := a.refreshSubscription(); err != nil {
		a.log.addf("подписка не обновилась при запуске, работаю по сохранённой: %v", err)
	}
}
