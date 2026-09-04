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
	Servers []Server
}

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
// a subscription URL, or one or more olcrtc:// links.
func importAny(text string) (*importResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("пустой ввод")
	}

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

	if strings.Contains(text, "olcrtc://") {
		servers := parseURIList(text)
		if len(servers) == 0 {
			return nil, fmt.Errorf("не удалось разобрать ни одной olcrtc:// ссылки")
		}
		return &importResult{Name: "Импорт по ссылке", Servers: servers}, nil
	}

	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		return fetchSubscription(text)
	}

	return nil, fmt.Errorf("не похоже ни на ссылку подписки, ни на olcrtc:// URI")
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
	servers := parseURIList(string(body))
	if len(servers) == 0 {
		return nil, fmt.Errorf("в подписке нет ни одной olcrtc:// строки")
	}
	name := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "#name:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "#name:"))
			break
		}
	}
	return &importResult{Name: name, SubURL: subURL, Servers: servers}, nil
}

func parseURIList(text string) []Server {
	var out []Server
	for _, raw := range strings.Fields(text) {
		raw = strings.TrimSpace(raw)
		if !strings.HasPrefix(raw, "olcrtc://") {
			continue
		}
		if s, ok := parseURI(raw); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseURI understands the query-string form used by current subscriptions:
//
//	olcrtc://<carrier>@room/<roomID>?key=..&transport=..&vp8_fps=..&client_id=..#label
func parseURI(raw string) (Server, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return Server{}, false
	}
	q := u.Query()

	s := Server{
		Carrier:   strings.ToLower(u.User.Username()),
		Key:       q.Get("key"),
		Transport: q.Get("transport"),
		ClientID:  q.Get("client_id"),
		Core:      q.Get("core"),
		VP8FPS:    atoiDefault(q.Get("vp8_fps"), 30),
		VP8Batch:  atoiDefault(q.Get("vp8_batch"), 64),
	}

	room := strings.TrimPrefix(u.Path, "/")
	if u.Host != "" && u.Host != "room" {
		// Some links put the room in the host part instead.
		room = strings.TrimPrefix(u.Host+u.Path, "room/")
	}
	s.RoomID = strings.TrimPrefix(room, "room/")

	if s.Carrier == "" {
		s.Carrier = "telemost"
	}
	if s.Transport == "" {
		s.Transport = "vp8channel"
	}
	if s.RoomID == "" || s.Key == "" {
		return Server{}, false
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
