//go:build android

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// Канал к оболочке приложения — unix-сокет в личном каталоге приложения
// (olcvpn.sock): другие приложения туда не попадут. По нему ходят строки JSON
// в обе стороны, а дескриптор TUN — вложением SCM_RIGHTS к строке tun.fd.
//
// Бэкенд → оболочка:
//
//	hello     адрес интерфейса и токен уже известны оболочке, тут — версия
//	state     фаза для уведомления: заголовок, подсказка, идёт ли сессия
//	speed     раз в секунду, пока туннель работает: скорость через него
//	tun.open  открыть VpnService с такими адресами и маршрутами
//	tun.close закрыть его
//	captcha   показать страницу капчи VK (url) или вернуться в приложение
//	install   открыть установщик для скачанного APK (path)
//
// Оболочка → бэкенд:
//
//	tun.fd / tun.err  ответ на tun.open с тем же id
//	net        DNS текущей сети — для запросов в обход туннеля
//	connect / disconnect  кнопки уведомления, плитки и автозапуска
//	revoked    систему VPN отобрало другое приложение или сам пользователь

type shellMsg struct {
	Op string `json:"op"`
	ID int    `json:"id,omitempty"`

	// state
	Phase   string `json:"phase,omitempty"`
	Title   string `json:"title,omitempty"`
	Hint    string `json:"hint,omitempty"`
	Server  string `json:"server,omitempty"`
	Running bool   `json:"running,omitempty"`
	TUN     bool   `json:"tun,omitempty"`

	// speed: байт в секунду через туннель; Counted — считал ли их sing-box
	// (в «Только прокси» для olcrtc его нет, и считать нечем)
	Down    int64 `json:"down,omitempty"`
	Up      int64 `json:"up,omitempty"`
	Counted bool  `json:"counted,omitempty"`

	// tun.open
	Addrs  []string `json:"addrs,omitempty"`
	Routes []string `json:"routes,omitempty"`
	DNS    []string `json:"dns,omitempty"`
	MTU    int      `json:"mtu,omitempty"`

	URL     string `json:"url,omitempty"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

type tunAnswer struct {
	fd  int
	err error
}

type shell struct {
	a *app

	mu      sync.Mutex
	conn    *net.UnixConn
	waiting map[int]chan tunAnswer
	nextID  int
	last    shellMsg // последнее состояние — новому подключению сразу
}

func newShell(a *app) *shell {
	return &shell{a: a, waiting: map[int]chan tunAnswer{}}
}

// listen opens the socket the shell connects to. Only the app's own user can
// reach it: it lives in the app's private directory and is chmod 0600.
func (s *shell) listen(path string) error {
	_ = os.Remove(path)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	go func() {
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			s.attach(c)
		}
	}()
	return nil
}

// attach makes c the current shell connection. The shell reconnects when its
// process is restarted; the newest connection wins.
func (s *shell) attach(c *net.UnixConn) {
	s.mu.Lock()
	old := s.conn
	s.conn = c
	last := s.last
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	s.send(shellMsg{Op: "hello", Version: version, URL: s.a.url})
	if last.Op != "" {
		s.send(last)
	}
	go s.read(c)
}

// send writes one message; without a shell it is dropped, and state is kept
// for the next one.
func (s *shell) send(m shellMsg) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Op == "state" {
		s.last = m
	}
	if s.conn == nil {
		return
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := s.conn.Write(b); err != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

// read handles the shell's messages. File descriptors arrive as ancillary
// data with the bytes of the line they belong to; they are queued in order and
// taken by the tun.fd lines.
func (s *shell) read(c *net.UnixConn) {
	defer c.Close()
	var (
		pending bytes.Buffer
		fds     []int
		buf     = make([]byte, 16<<10)
		oob     = make([]byte, syscall.CmsgSpace(4*4))
	)
	for {
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if oobn > 0 {
			fds = append(fds, parseRights(oob[:oobn])...)
		}
		pending.Write(buf[:n])
		for {
			line, rest, ok := bytes.Cut(pending.Bytes(), []byte{'\n'})
			if !ok {
				break
			}
			var m shellMsg
			if json.Unmarshal(line, &m) == nil {
				if m.Op == "tun.fd" {
					fd := -1
					if len(fds) > 0 {
						fd, fds = fds[0], fds[1:]
					}
					s.answerTun(m.ID, fd, nil)
				} else {
					s.handle(m)
				}
			}
			rest = append([]byte{}, rest...)
			pending.Reset()
			pending.Write(rest)
		}
		if err != nil {
			for _, fd := range fds {
				_ = syscall.Close(fd)
			}
			return
		}
	}
}

func parseRights(oob []byte) []int {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	var fds []int
	for i := range msgs {
		if got, err := syscall.ParseUnixRights(&msgs[i]); err == nil {
			fds = append(fds, got...)
		}
	}
	return fds
}

func (s *shell) handle(m shellMsg) {
	a := s.a
	switch m.Op {
	case "tun.err":
		s.answerTun(m.ID, -1, errors.New(m.Error))
	case "net":
		setSystemDNS(m.DNS)
	case "connect":
		go func() {
			if a.tun.running() {
				return
			}
			if err := a.connectSelected(); err != nil {
				a.log.addf("подключение: %v", err)
			}
		}()
	case "disconnect":
		go a.tun.Stop()
	case "revoked":
		// VPN отобрали — туннель без TUN тут никому не нужен: пользователь
		// ждёт, что olcvpn отключился, как и показывает система.
		a.log.add("Android отключил VPN olcvpn — отключаюсь")
		go a.tun.Stop()
	}
}

func (s *shell) answerTun(id, fd int, err error) {
	s.mu.Lock()
	ch := s.waiting[id]
	delete(s.waiting, id)
	s.mu.Unlock()
	if ch == nil {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		return
	}
	ch <- tunAnswer{fd: fd, err: err}
}

// openTun asks the shell for the VpnService interface and waits for its file
// descriptor. The shell answers at once: the permission dialog is shown before
// connecting, from the page, so nothing waits on a person here.
func (s *shell) openTun(req shellMsg) (int, error) {
	// Автоподключение при запуске может обогнать оболочку, которая ещё
	// только подключается к сокету.
	for i := 0; ; i++ {
		s.mu.Lock()
		if s.conn != nil {
			break
		}
		s.mu.Unlock()
		if i == 100 {
			return -1, fmt.Errorf("оболочка приложения не на связи")
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.nextID++
	id := s.nextID
	ch := make(chan tunAnswer, 1)
	s.waiting[id] = ch
	s.mu.Unlock()

	req.Op, req.ID = "tun.open", id
	s.send(req)
	select {
	case ans := <-ch:
		if ans.err != nil {
			return -1, ans.err
		}
		if ans.fd < 0 {
			return -1, fmt.Errorf("оболочка не передала дескриптор TUN")
		}
		return ans.fd, nil
	case <-time.After(30 * time.Second):
		s.mu.Lock()
		delete(s.waiting, id)
		s.mu.Unlock()
		return -1, fmt.Errorf("VPN не открылся за 30 секунд")
	}
}

func (s *shell) closeTun() {
	s.send(shellMsg{Op: "tun.close"})
}

// watchState keeps the shell's notification in step with the tunnel.
func (s *shell) watchState() {
	push := func() {
		ph, detail, peer := s.a.tun.status()
		tun := s.a.tun.tunActive()
		title, hint := statusText(ph, detail, peer, tun)
		name := ""
		s.a.cfgMu.Lock()
		if srv, ok := s.a.cfg.server(s.a.tun.serverID()); ok {
			name = srv.Name
		}
		s.a.cfgMu.Unlock()
		s.send(shellMsg{
			Op: "state", Phase: string(ph), Title: title, Hint: hint,
			Server: name, Running: ph.running(), TUN: tun,
		})
	}
	push()
	for range s.a.stateChanged(nil) {
		push()
	}
}

// watchSpeed reports the tunnel's speed once a second while a session is up.
func (s *shell) watchSpeed() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastUp, lastDown := tunnelTraffic.up.Load(), tunnelTraffic.down.Load()
	for range tick.C {
		up, down := tunnelTraffic.up.Load(), tunnelTraffic.down.Load()
		// Пока сессии нет, молчим: строку скорости убирает смена фазы.
		if ph, _, _ := s.a.tun.status(); ph == phaseProxy || ph == phaseConnected {
			s.send(shellMsg{
				Op: "speed", Up: up - lastUp, Down: down - lastDown,
				Counted: tunnelTraffic.boxes.Load() > 0,
			})
		}
		lastUp, lastDown = up, down
	}
}
