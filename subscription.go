package main

import (
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// subDescriptor is the JSON the subscription QR code carries.
type subDescriptor struct {
	Type string `json:"type"`
	Name string `json:"n"`
	Slug string `json:"s"`
	URL  string `json:"u"`
}

// importResult is what a single import call produced.
type importResult struct {
	Name    string
	SubURL  string
	Servers []Server // серверы подписки; пусто — подписку не трогать
	Manual  []Server // серверы VK TURN — добавляются к своим, ничего не заменяя
	Note    string   // что пришлось пропустить, для пользователя
}

// count is how many servers the import brought in total.
func (r *importResult) count() int { return len(r.Servers) + len(r.Manual) }

// insecureClient talks to the provider panel, which serves its own
// self-signed "olcRTC Admin CA" certificate rather than a public one.
var insecureClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // provider panels use a private CA
		Proxy:           nil,
	},
}

// importAny accepts whatever the user pasted: a subscription JSON descriptor,
// a subscription deep link, a subscription URL, or one or more server links.
func importAny(text string) (*importResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("пустой ввод")
	}

	// «Поделиться» кладёт в один текст и подписку, и серверы VK TURN:
	// VK TURN разбираем отдельно, остальное — как раньше.
	vk, rest, err := splitVKTurnLinks(text)
	if err != nil && len(vk) == 0 {
		return nil, err
	}
	if strings.TrimSpace(rest) == "" {
		if len(vk) == 0 {
			return nil, fmt.Errorf("пустой ввод")
		}
		return &importResult{Manual: vk}, nil
	}
	res, err := importSubscription(rest)
	if err != nil {
		return nil, err
	}
	res.Manual = vk
	return res, nil
}

// importSubscription handles everything except VK TURN links.
func importSubscription(text string) (*importResult, error) {
	if strings.HasPrefix(text, "{") {
		var d subDescriptor
		if err := json.Unmarshal([]byte(text), &d); err == nil && d.URL != "" {
			res, err := fetchSubscription(d.URL)
			if err != nil {
				return nil, err
			}
			if d.Name != "" {
				res.Name = d.Name
			}
			return res, nil
		}
	}

	// olconnect://subscription?url=... — не сервер, а обёртка вокруг адреса
	// подписки: панели раздают именно её, потому что по ней клиент потом
	// обновляет список серверов.
	if name, subURL, ok := subscriptionLink(text); ok {
		res, err := fetchSubscription(subURL)
		if err != nil {
			return nil, err
		}
		if name != "" {
			res.Name = name
		}
		return res, nil
	}

	if containsLink(text) || strings.Contains(text, openfluxScheme) {
		servers, openflux := parseURIList(text)
		if len(servers) == 0 {
			if openflux > 0 {
				return nil, fmt.Errorf("%s", openfluxNote(openflux))
			}
			return nil, fmt.Errorf(
				"ссылка не похожа ни на сервер (olconnect://провайдер@room/комната?key=...), " +
					"ни на подписку (olconnect://subscription?url=...)")
		}
		return &importResult{Name: "Импорт по ссылке", Servers: servers, Note: openfluxNote(openflux)}, nil
	}

	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		return fetchSubscription(text)
	}

	return nil, fmt.Errorf("не похоже ни на ссылку подписки, ни на ссылку сервера")
}

func fetchSubscription(subURL string) (*importResult, error) {
	req, err := http.NewRequest(http.MethodGet, subURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := insecureClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("подписка недоступна: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("подписка вернула HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	servers, openflux := parseURIList(string(body))
	if len(servers) == 0 {
		if openflux > 0 {
			return nil, fmt.Errorf("в подписке только серверы OpenFlux — это другой протокол, olcvpn его не поддерживает")
		}
		return nil, fmt.Errorf("в подписке нет ни одного сервера")
	}
	name := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "#name:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "#name:"))
			break
		}
	}
	return &importResult{Name: name, SubURL: subURL, Servers: servers, Note: openfluxNote(openflux)}, nil
}

// subscriptionLink pulls the subscription address out of an
// olconnect://subscription?url=...&name=... link (or its olcrtc:// twin).
//
// The link also carries mirror_url / mirror_key — an encrypted copy of the
// same list on Yandex Disk, for when the panel itself is unreachable. Its
// format lives in OlConnect_manager's internal/subscription/mirror; the client
// does not read it yet and uses the primary address only.
func subscriptionLink(text string) (name, subURL string, ok bool) {
	for _, field := range strings.Fields(text) {
		if !hasLinkScheme(field) || !strings.Contains(field, "://subscription") {
			continue
		}
		u, err := url.Parse(field)
		if err != nil {
			continue
		}
		q := u.Query()
		target := q.Get("url")
		if target == "" {
			continue
		}
		if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
			continue
		}
		return q.Get("name"), target, true
	}
	return "", "", false
}

// linkSchemes are the deep-link schemes the panel has used. olcrtc:// is the
// original; on 12 September the project was renamed to OlConnect and new links
// switched to olconnect://, while the panel still accepts both — so must we.
var linkSchemes = []string{"olconnect://", "olcrtc://"}

// openfluxScheme marks servers of a different protocol with its own binary.
// Subscriptions can mix them in; they are skipped rather than misparsed.
const openfluxScheme = "openflux://"

func hasLinkScheme(s string) bool {
	for _, p := range linkSchemes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func containsLink(text string) bool {
	for _, p := range linkSchemes {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

// parseURIList extracts every server link and reports how many OpenFlux
// entries it had to leave out.
func parseURIList(text string) (servers []Server, openflux int) {
	for _, raw := range strings.Fields(text) {
		raw = strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(raw, openfluxScheme):
			openflux++
		case hasLinkScheme(raw):
			if s, ok := parseURI(raw); ok {
				servers = append(servers, s)
			}
		}
	}
	return servers, openflux
}

// openfluxNote explains skipped OpenFlux entries, or returns "".
func openfluxNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("пропущено серверов OpenFlux: %d — это другой протокол со своей программой, olcvpn его не поддерживает", n)
}

// parseURI understands both forms the OlConnect panel produces
// (internal/admin/api_instances.go, buildURIWith / buildCompactURIWith):
//
//	olconnect://<carrier>@room/<room>?key=..[&transport=..&vp8_fps=..&vp8_batch=..]&client_id=..#name
//	olconnect://<carrier>@r/<escaped room>?k=..[&t=..&f=..&b=..]&c=..&d=..#name
//
// The long form is what subscriptions serve; the short one goes into QR codes.
// Two conventions matter and are easy to get wrong: the panel leaves the
// transport out entirely when it is datachannel, and the short form also drops
// vp8 fps/batch when they equal 60/8. The room is inserted verbatim, so for
// Jitsi — and now for Telemost too — it is a full https:// address.
func parseURI(raw string) (Server, bool) {
	if !hasLinkScheme(raw) {
		return Server{}, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Server{}, false
	}
	compact := u.Host == "r"
	if u.Host != "room" && !compact {
		return Server{}, false
	}

	q := u.Query()
	pick := func(long, short string) string {
		if v := q.Get(long); v != "" {
			return v
		}
		return q.Get(short)
	}

	s := Server{
		Carrier:   strings.ToLower(u.User.Username()),
		RoomID:    strings.TrimPrefix(u.Path, "/"),
		Key:       pick("key", "k"),
		Transport: pick("transport", "t"),
		ClientID:  pick("client_id", "c"),
		DNS:       pick("dns", "d"),
		Core:      q.Get("core"),
	}
	if s.Carrier == "" || s.RoomID == "" || s.Key == "" {
		return Server{}, false
	}
	if s.Transport == "" {
		s.Transport = "datachannel"
	}

	if s.Transport == "vp8channel" {
		// Ноль значит «как решит ядро»: полная форма просто не пишет
		// параметр, если его не задали на сервере.
		var fps, batch int
		if compact {
			fps, batch = 60, 8
		}
		s.VP8FPS = atoiDefault(pick("vp8_fps", "f"), fps)
		s.VP8Batch = atoiDefault(pick("vp8_batch", "b"), batch)
	}

	name, _ := url.PathUnescape(u.Fragment)
	if name == "" {
		name = s.Carrier + " " + s.RoomID
	}
	s.Name = name

	sum := sha1.Sum([]byte(s.Carrier + "|" + s.RoomID + "|" + s.Transport)) //nolint:gosec // identifier only
	s.ID = hex.EncodeToString(sum[:6])
	return s, true
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
