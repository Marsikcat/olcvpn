package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Server is one olcrtc endpoint, normally imported from a subscription.
type Server struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Carrier   string `json:"carrier"` // telemost, wbstream, jazz, jitsi
	RoomID    string `json:"roomId"`
	Key       string `json:"key"`
	Transport string `json:"transport"` // vp8channel, datachannel, seichannel, videochannel
	VP8FPS    int    `json:"vp8Fps"`
	VP8Batch  int    `json:"vp8Batch"`
	ClientID  string `json:"clientId"`
	DNS       string `json:"dns,omitempty"` // DNS этого сервера, если панель его задала
	Core      string `json:"core"`          // legacy / empty — hint from the subscription

	// Kind разделяет способы подключения: пусто — olcrtc из подписки,
	// kindVKTurn — WireGuard через TURN-серверы звонков VK.
	Kind   string     `json:"kind,omitempty"`
	VKLink string     `json:"vkLink,omitempty"` // приглашение в звонок VK
	Peer   string     `json:"peer,omitempty"`   // vk-turn-proxy на сервере, host:port
	WG     *WireGuard `json:"wg,omitempty"`
}

const kindVKTurn = "vkturn"

// isVKTurn reports whether the server is a VK TURN entry rather than an olcrtc
// one. Such entries are added by hand and must survive subscription refreshes.
func (s *Server) isVKTurn() bool { return s.Kind == kindVKTurn }

// WireGuard is the client side of the tunnel that rides inside VK TURN.
type WireGuard struct {
	PrivateKey    string   `json:"privateKey"`
	Address       []string `json:"address"`
	PeerPublicKey string   `json:"peerPublicKey"`
	PresharedKey  string   `json:"presharedKey,omitempty"`
	MTU           int      `json:"mtu,omitempty"`
	DNS           string   `json:"dns,omitempty"`
	Keepalive     int      `json:"keepalive,omitempty"`
}

// Config is persisted next to the executable.
type Config struct {
	SubURL      string    `json:"subUrl"`
	SubName     string    `json:"subName"`
	SubUpdated  time.Time `json:"subUpdated,omitempty"` // когда список серверов в последний раз сверяли с панелью
	Servers     []Server  `json:"servers"`
	SelectedID  string    `json:"selectedId"`
	SocksHost   string    `json:"socksHost"`
	SocksPort   int       `json:"socksPort"`
	DNS         string    `json:"dns"`
	CoreBinary  string    `json:"coreBinary"`
	UseTUN      bool      `json:"useTun"`
	DirectIPs   string    `json:"directIps"`
	DirectHosts string    `json:"directHosts"`

	// Оформление и поведение приложения.
	Theme       string `json:"theme"`       // auto | dark | light
	Autoconnect bool   `json:"autoconnect"` // подключаться при запуске
	TrayClose   bool   `json:"trayClose"`   // «закрыть» сворачивает в трей
	StartHidden bool   `json:"startHidden"` // стартовать сразу в трее

	mu   sync.Mutex
	path string
}

func defaultConfig(path string) *Config {
	return &Config{
		SocksHost:   "127.0.0.1",
		SocksPort:   8808,
		DNS:         "8.8.8.8:53",
		UseTUN:      true,
		DirectHosts: "telemost.yandex.ru,yandex.net,yandex.ru",
		Theme:       "auto",
		TrayClose:   true,
		path:        path,
	}
}

func loadConfig(dir string) *Config {
	path := filepath.Join(dir, "olcvpn.json")
	c := defaultConfig(path)
	b, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	// Блокнот и PowerShell сохраняют UTF-8 с BOM, а json.Unmarshal на нём
	// спотыкается: без этого правки файла руками молча сбрасывали настройки.
	b = bytes.TrimPrefix(bytes.TrimSpace(b), []byte("\xef\xbb\xbf"))

	if err := json.Unmarshal(b, c); err != nil {
		// Не терять чужой файл: следующее сохранение перезаписало бы его
		// дефолтами, и серверы подписки исчезли бы без следа.
		_ = os.Rename(path, path+".bad")
		return defaultConfig(path)
	}
	c.path = path
	if c.SocksHost == "" {
		c.SocksHost = "127.0.0.1"
	}
	if c.SocksPort == 0 {
		c.SocksPort = 8808
	}
	if c.DNS == "" {
		c.DNS = "8.8.8.8:53"
	}
	if c.Theme == "" {
		c.Theme = "auto"
	}
	return c
}

func (c *Config) save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, b, 0o600)
}

// manualServers returns the VK TURN entries — the ones added by hand, which a
// subscription import or refresh must carry over rather than replace.
func (c *Config) manualServers() []Server {
	var out []Server
	for _, s := range c.Servers {
		if s.isVKTurn() {
			out = append(out, s)
		}
	}
	return out
}

// subscriptionServers returns everything that came from the subscription.
func (c *Config) subscriptionServers() []Server {
	var out []Server
	for _, s := range c.Servers {
		if !s.isVKTurn() {
			out = append(out, s)
		}
	}
	return out
}

// hasServer reports whether id names one of the current servers.
func (c *Config) hasServer(id string) bool {
	for i := range c.Servers {
		if c.Servers[i].ID == id {
			return true
		}
	}
	return false
}

func (c *Config) selected() *Server {
	for i := range c.Servers {
		if c.Servers[i].ID == c.SelectedID {
			return &c.Servers[i]
		}
	}
	if len(c.Servers) > 0 {
		return &c.Servers[0]
	}
	return nil
}
