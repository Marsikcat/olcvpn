package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// Поделиться серверами — это текст, по ссылке на строку: подписка или её
// серверы в обычном формате панели (их понимает и OlConnect на телефоне),
// плюс серверы VK TURN в своём формате olcvpn://vkturn. Получатель вставляет
// текст в поле импорта или открывает QR-код — импорт разбирает всё сразу.

const vkturnScheme = "olcvpn://vkturn"

// subscriptionShareLink wraps the subscription address the same way the
// panel does, so the link keeps working for refreshes on the other side.
func subscriptionShareLink(subURL, name string) string {
	q := url.Values{}
	q.Set("url", subURL)
	if name != "" {
		q.Set("name", name)
	}
	return "olconnect://subscription?" + q.Encode()
}

// roomEscaper keeps the room verbatim, as the panel writes it, except for the
// few characters that would end the path early.
var roomEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23", " ", "%20")

// serverURI is the long panel form of one olcrtc server — the inverse of
// parseURI.
func serverURI(s Server) string {
	q := url.Values{}
	q.Set("key", s.Key)
	// Панель не пишет transport, когда он datachannel; parseURI так и ждёт.
	if s.Transport != "" && s.Transport != "datachannel" {
		q.Set("transport", s.Transport)
	}
	if s.Transport == "vp8channel" {
		if s.VP8FPS > 0 {
			q.Set("vp8_fps", strconv.Itoa(s.VP8FPS))
		}
		if s.VP8Batch > 0 {
			q.Set("vp8_batch", strconv.Itoa(s.VP8Batch))
		}
	}
	if s.ClientID != "" {
		q.Set("client_id", s.ClientID)
	}
	if s.DNS != "" {
		q.Set("dns", s.DNS)
	}
	if s.Core != "" {
		q.Set("core", s.Core)
	}
	return "olconnect://" + url.PathEscape(s.Carrier) + "@room/" + roomEscaper.Replace(s.RoomID) +
		"?" + q.Encode() + "#" + url.PathEscape(s.Name)
}

// vkturnURI packs a VK TURN server, WireGuard keys included.
func vkturnURI(s Server) string {
	q := url.Values{}
	q.Set("link", s.VKLink)
	q.Set("peer", s.Peer)
	if wg := s.WG; wg != nil {
		q.Set("pk", wg.PrivateKey)
		q.Set("addr", strings.Join(wg.Address, ","))
		q.Set("pub", wg.PeerPublicKey)
		if wg.PresharedKey != "" {
			q.Set("psk", wg.PresharedKey)
		}
		if wg.MTU > 0 {
			q.Set("mtu", strconv.Itoa(wg.MTU))
		}
		if wg.DNS != "" {
			q.Set("dns", wg.DNS)
		}
		if wg.Keepalive > 0 {
			q.Set("ka", strconv.Itoa(wg.Keepalive))
		}
	}
	return vkturnScheme + "?" + q.Encode() + "#" + url.PathEscape(s.Name)
}

// parseVKTurnURI reads a link made by vkturnURI. It goes through the same
// checks as a server typed in by hand, so a link cannot smuggle in, say, a
// "call link" pointing somewhere other than VK.
func parseVKTurnURI(raw string) (Server, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "olcvpn" || u.Host != "vkturn" {
		return Server{}, fmt.Errorf("ссылка VK TURN не разобрана")
	}
	q := u.Query()
	var conf strings.Builder
	fmt.Fprintf(&conf, "[Interface]\nPrivateKey = %s\nAddress = %s\n", q.Get("pk"), q.Get("addr"))
	if v := q.Get("dns"); v != "" {
		fmt.Fprintf(&conf, "DNS = %s\n", v)
	}
	if v := q.Get("mtu"); v != "" {
		fmt.Fprintf(&conf, "MTU = %s\n", v)
	}
	fmt.Fprintf(&conf, "[Peer]\nPublicKey = %s\n", q.Get("pub"))
	if v := q.Get("psk"); v != "" {
		fmt.Fprintf(&conf, "PresharedKey = %s\n", v)
	}
	if v := q.Get("ka"); v != "" {
		fmt.Fprintf(&conf, "PersistentKeepalive = %s\n", v)
	}
	return newVKTurnServer(u.Fragment, q.Get("link"), q.Get("peer"), conf.String())
}

// splitVKTurnLinks takes the VK TURN links out of pasted text and returns
// them parsed, plus whatever text is left for the subscription importer.
func splitVKTurnLinks(text string) (servers []Server, rest string, err error) {
	// Без ссылок VK TURN текст уходит дальше нетронутым: разбивка по
	// пробелам испортила бы, например, JSON из QR с пробелом в имени.
	if !strings.Contains(text, vkturnScheme) {
		return nil, text, nil
	}
	var other []string
	for _, field := range strings.Fields(text) {
		if !strings.HasPrefix(field, vkturnScheme) {
			other = append(other, field)
			continue
		}
		s, perr := parseVKTurnURI(field)
		if perr != nil {
			if err == nil {
				err = perr
			}
			continue
		}
		servers = append(servers, s)
	}
	return servers, strings.Join(other, "\n"), err
}

// shareItem is one line the user can include or leave out.
type shareItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // sub | server | vkturn
	Name string `json:"name"`
	Note string `json:"note"`
	link string
}

// shareItems lists everything that can be shared. A subscription goes as one
// link — the other side then refreshes it like its own; servers that came
// without a subscription address go one by one. The caller holds cfgMu.
func (a *app) shareItems() []shareItem {
	var items []shareItem
	subs := a.cfg.subscriptionServers()
	if a.cfg.SubURL != "" {
		name := a.cfg.SubName
		if name == "" {
			name = "без названия"
		}
		items = append(items, shareItem{
			ID: "sub", Kind: "sub", Name: "Подписка «" + name + "»",
			Note: fmt.Sprintf("%d серв., обновляется у получателя", len(subs)),
			link: subscriptionShareLink(a.cfg.SubURL, a.cfg.SubName),
		})
	} else {
		for _, s := range subs {
			items = append(items, shareItem{
				ID: s.ID, Kind: "server", Name: s.Name, Note: s.Carrier + " · " + s.Transport,
				link: serverURI(s),
			})
		}
	}
	for _, s := range a.cfg.manualServers() {
		items = append(items, shareItem{
			ID: s.ID, Kind: "vkturn", Name: s.Name, Note: "VK TURN · " + s.Peer,
			link: vkturnURI(s),
		})
	}
	return items
}

// shareQR draws the text as a QR code, or says why it cannot.
func shareQR(text string) (string, error) {
	hints := map[gozxing.EncodeHintType]any{
		gozxing.EncodeHintType_ERROR_CORRECTION: "L",
		gozxing.EncodeHintType_MARGIN:           2,
	}
	m, err := qrcode.NewQRCodeWriter().Encode(text, gozxing.BarcodeFormat_QR_CODE, 360, 360, hints)
	if err != nil {
		return "", fmt.Errorf("слишком много для QR-кода — отправьте текстом")
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// handleShare answers with the share text for the chosen items, or for all of
// them when none are named, plus its QR code.
func (a *app) handleShare(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	a.cfgMu.Lock()
	items := a.shareItems()
	a.cfgMu.Unlock()
	if len(items) == 0 {
		writeErr(w, fmt.Errorf("делиться нечем: серверов нет"))
		return
	}

	want := map[string]bool{}
	for _, id := range body.IDs {
		want[id] = true
	}
	var lines []string
	for _, it := range items {
		if body.IDs == nil || want[it.ID] {
			lines = append(lines, it.link)
		}
	}
	text := strings.Join(lines, "\n")

	resp := map[string]any{"items": items, "text": text}
	if text != "" {
		if qr, err := shareQR(text); err != nil {
			resp["qrNote"] = err.Error()
		} else {
			resp["qr"] = qr
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
