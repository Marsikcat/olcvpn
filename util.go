package main

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// logBus keeps a bounded history and fans new lines out to SSE subscribers.
type logBus struct {
	mu    sync.Mutex
	lines []string
	subs  map[chan string]struct{}
}

func newLogBus() *logBus {
	return &logBus{subs: map[chan string]struct{}{}}
}

func (l *logBus) add(line string) {
	stamped := time.Now().Format("15:04:05") + "  " + line
	l.mu.Lock()
	l.lines = append(l.lines, stamped)
	if len(l.lines) > 800 {
		l.lines = l.lines[len(l.lines)-800:]
	}
	subs := make([]chan string, 0, len(l.subs))
	for c := range l.subs {
		subs = append(subs, c)
	}
	l.mu.Unlock()

	for _, c := range subs {
		select {
		case c <- stamped:
		default:
		}
	}
}

func (l *logBus) addf(format string, args ...any) { l.add(fmt.Sprintf(format, args...)) }

func (l *logBus) history() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.lines))
	copy(out, l.lines)
	return out
}

func (l *logBus) subscribe() (chan string, func()) {
	c := make(chan string, 256)
	l.mu.Lock()
	l.subs[c] = struct{}{}
	l.mu.Unlock()
	return c, func() {
		l.mu.Lock()
		delete(l.subs, c)
		l.mu.Unlock()
	}
}

// isAdmin reports whether the process is elevated. Opening a raw physical
// drive handle succeeds only for administrators, which is enough of a probe
// and avoids pulling in golang.org/x/sys just for the token check.
func isAdmin() bool {
	f, err := os.Open(`\\.\PHYSICALDRIVE0`)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// socksDial performs a SOCKS5 CONNECT to host:port through the local proxy.
func socksDial(proxyAddr, host string, port uint16) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))

	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		c.Close()
		return nil, err
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		c.Close()
		return nil, fmt.Errorf("SOCKS5: прокси отклонил рукопожатие")
	}

	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		c.Close()
		return nil, err
	}
	if head[1] != 0x00 {
		c.Close()
		return nil, fmt.Errorf("SOCKS5: соединение отклонено (код %d)", head[1])
	}
	switch head[3] {
	case 0x01:
		_, err = io.ReadFull(c, make([]byte, 4+2))
	case 0x04:
		_, err = io.ReadFull(c, make([]byte, 16+2))
	case 0x03:
		n := make([]byte, 1)
		if _, err = io.ReadFull(c, n); err == nil {
			_, err = io.ReadFull(c, make([]byte, int(n[0])+2))
		}
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// ipInfo is the subset of ipinfo.io's answer the UI shows.
type ipInfo struct {
	IP      string `json:"ip"`
	City    string `json:"city"`
	Country string `json:"country"`
	Org     string `json:"org"`
}

// ipInfoThroughSocks asks where traffic leaves the tunnel.
func ipInfoThroughSocks(proxyAddr string) (ipInfo, error) {
	body, err := getThroughSocks(proxyAddr, "ipinfo.io", "/json", 2048)
	if err != nil {
		return ipInfo{}, err
	}
	var out ipInfo
	if err := json.Unmarshal(body, &out); err != nil {
		return ipInfo{}, err
	}
	if out.IP == "" {
		return ipInfo{}, fmt.Errorf("пустой ответ")
	}
	return out, nil
}

// publicIPThroughSocks fetches the exit IP so the user can confirm the tunnel
// really carries traffic.
func publicIPThroughSocks(proxyAddr string) (string, error) {
	const host = "icanhazip.com"
	body, err := getThroughSocks(proxyAddr, host, "/", 128)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", fmt.Errorf("пустой ответ")
	}
	return ip, nil
}

// getThroughSocks performs one HTTPS GET over the local SOCKS5 proxy.
func getThroughSocks(proxyAddr, host, path string, limit int64) ([]byte, error) {
	raw, err := socksDial(proxyAddr, host, 443)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(25 * time.Second))

	conn := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err := conn.Handshake(); err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn,
		"GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: olcvpn\r\nConnection: close\r\n\r\n",
		path, host); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
