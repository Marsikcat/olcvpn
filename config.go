package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
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
	Core      string `json:"core"` // legacy / empty — hint from the subscription
}

// Config is persisted next to the executable.
type Config struct {
	SubURL      string   `json:"subUrl"`
	SubName     string   `json:"subName"`
	Servers     []Server `json:"servers"`
	SelectedID  string   `json:"selectedId"`
	SocksHost   string   `json:"socksHost"`
	SocksPort   int      `json:"socksPort"`
	DNS         string   `json:"dns"`
	CoreBinary  string   `json:"coreBinary"`
	UseTUN      bool     `json:"useTun"`
	DirectIPs   string   `json:"directIps"`
	DirectHosts string   `json:"directHosts"`

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
	if err := json.Unmarshal(b, c); err != nil {
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
