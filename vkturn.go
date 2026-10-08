package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// VK TURN: WireGuard, обёрнутый в DTLS, идёт к TURN-серверам звонков VK,
// а те пересылают его на vk-turn-proxy на нашем сервере. Клиентская часть —
// это bin/vkturn-client.exe (cacggghp/vk-turn-proxy, PR #183 + наш патч),
// поверх которого sing-box поднимает WireGuard и отдаёт SOCKS или TUN.
//
//	sing-box (WireGuard) → 127.0.0.1:vkturnLocalPort → vkturn-client → TURN VK → сервер
const vkturnLocalPort = 9000

var (
	vkLinkRe     = regexp.MustCompile(`^https://(?:[a-z0-9-]+\.)?vk\.(?:com|ru)/call/join/[A-Za-z0-9_-]+$`)
	captchaURLRe = regexp.MustCompile(`Open this URL in your browser:\s*(http://(?:localhost|127\.0\.0\.1):\d+\S*)`)
)

// newVKTurnServer validates what the user typed and builds a server entry.
func newVKTurnServer(name, link, peer, wgText string) (Server, error) {
	link = strings.TrimSpace(link)
	peer = strings.TrimSpace(peer)
	if !vkLinkRe.MatchString(link) {
		return Server{}, fmt.Errorf("это не похоже на приглашение в звонок VK — нужна ссылка вида https://vk.com/call/join/…")
	}
	host, port, err := net.SplitHostPort(peer)
	if err != nil || host == "" {
		return Server{}, fmt.Errorf("адрес сервера нужен в виде IP:порт, например 203.0.113.10:56000")
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return Server{}, fmt.Errorf("неверный порт сервера: %s", port)
	}
	wg, err := parseWireGuardConf(wgText)
	if err != nil {
		return Server{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = "VK TURN · " + host
	}

	sum := sha1.Sum([]byte(kindVKTurn + "|" + peer + "|" + link)) //nolint:gosec // identifier only
	return Server{
		ID:        hex.EncodeToString(sum[:6]),
		Name:      strings.TrimSpace(name),
		Carrier:   "vk",
		Transport: "turn",
		Kind:      kindVKTurn,
		VKLink:    link,
		Peer:      peer,
		WG:        wg,
	}, nil
}

// parseWireGuardConf reads a standard WireGuard client config. Endpoint is
// ignored on purpose: the tunnel always goes to the local vk-turn client.
func parseWireGuardConf(text string) (*WireGuard, error) {
	wg := &WireGuard{}
	section := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch section + "." + k {
		case "interface.privatekey":
			wg.PrivateKey = v
		case "interface.address":
			for _, a := range strings.Split(v, ",") {
				if a = strings.TrimSpace(a); a != "" {
					wg.Address = append(wg.Address, a)
				}
			}
		case "interface.dns":
			wg.DNS = strings.TrimSpace(strings.Split(v, ",")[0])
		case "interface.mtu":
			wg.MTU, _ = strconv.Atoi(v)
		case "peer.publickey":
			wg.PeerPublicKey = v
		case "peer.presharedkey":
			wg.PresharedKey = v
		case "peer.persistentkeepalive":
			wg.Keepalive, _ = strconv.Atoi(v)
		}
	}

	switch {
	case !isWGKey(wg.PrivateKey):
		return nil, fmt.Errorf("в конфиге WireGuard нет PrivateKey в разделе [Interface]")
	case len(wg.Address) == 0:
		return nil, fmt.Errorf("в конфиге WireGuard нет Address в разделе [Interface]")
	case !isWGKey(wg.PeerPublicKey):
		return nil, fmt.Errorf("в конфиге WireGuard нет PublicKey в разделе [Peer]")
	}
	if wg.MTU <= 0 {
		wg.MTU = 1280 // так советует vk-turn-proxy: TURN и DTLS добавляют свои заголовки
	}
	if wg.Keepalive <= 0 {
		wg.Keepalive = 25
	}
	return wg, nil
}

// isWGKey checks the shape of a base64 Curve25519 key (32 bytes → 44 chars).
func isWGKey(s string) bool {
	return len(s) == 44 && strings.HasSuffix(s, "=")
}

// vkturnSingBoxConfig builds sing-box for a VK TURN connection: WireGuard to
// the local vk-turn client, a SOCKS/HTTP inbound on the app's proxy port, and
// with withTUN the system-wide interface as well.
func vkturnSingBoxConfig(cfg *Config, s *Server, dir string, withTUN bool) ([]byte, error) {
	wg := s.WG
	peer := map[string]any{
		"address":                       "127.0.0.1",
		"port":                          vkturnLocalPort,
		"public_key":                    wg.PeerPublicKey,
		"allowed_ips":                   []string{"0.0.0.0/0", "::/0"},
		"persistent_keepalive_interval": wg.Keepalive,
	}
	if wg.PresharedKey != "" {
		peer["pre_shared_key"] = wg.PresharedKey
	}

	dnsServer := wg.DNS
	if dnsServer == "" {
		dnsServer = "1.1.1.1"
	}

	// Свои процессы ходят мимо TUN. Без этого клиент VK TURN завернул бы в
	// туннель собственные соединения с TURN-серверами, и туннель замкнулся бы
	// сам на себя. Только для TUN: через прокси olcvpn.exe ходит нарочно,
	// проверяя IP выхода, и это должно идти в туннель.
	own := ownProcesses(dir)

	inbounds := []any{
		map[string]any{"type": "mixed", "tag": "proxy-in", "listen": cfg.SocksHost, "listen_port": cfg.SocksPort},
	}
	var rules []any
	dnsRules := []any{
		map[string]any{"domain_suffix": []string{"local", "lan"}, "server": "local"},
	}
	if withTUN {
		inbounds = append(inbounds, map[string]any{
			"type": "tun", "tag": "tun-in", "interface_name": "olcvpn-tun",
			"address":    []string{"172.19.0.1/30"},
			"auto_route": true, "strict_route": true, "stack": "mixed",
		})
		rules = append(rules,
			// Windows шлёт DNS на адрес туннеля, 172.19.0.2. Правило по
			// protocol без сниффинга не срабатывает, поэтому перехват — по
			// адресу; иначе запрос уйдёт по ip_is_private «напрямую» в никуда
			// и у системы не откроется ни один сайт.
			map[string]any{"ip_cidr": []string{"172.19.0.2/32"}, "port": 53, "action": "hijack-dns"},
			// Выше общего перехвата DNS: клиент VK TURN резолвит адреса VK сам,
			// через публичные DNS, и это должно идти напрямую, а не через
			// туннель, который от него же и зависит.
			map[string]any{"inbound": []string{"tun-in"}, "process_name": own, "outbound": "direct"},
			map[string]any{"protocol": "dns", "action": "hijack-dns"},
		)
		dnsRules = append([]any{map[string]any{"process_name": own, "server": "local"}}, dnsRules...)
	}
	// «Мимо туннеля» из настроек — как и для Телемоста.
	if hosts := csv(cfg.DirectHosts); len(hosts) > 0 {
		rules = append(rules, map[string]any{"domain_suffix": hosts, "outbound": "direct"})
		dnsRules = append([]any{map[string]any{"domain_suffix": hosts, "server": "local"}}, dnsRules...)
	}
	if ips := csv(cfg.DirectIPs); len(ips) > 0 {
		rules = append(rules, map[string]any{"ip_cidr": ips, "outbound": "direct"})
	}
	rules = append(rules, map[string]any{"ip_is_private": true, "outbound": "direct"})

	dns := map[string]any{
		"servers": []any{
			// UDP, а не TCP: WireGuard его несёт, а TCP-рукопожатие ради
			// каждого запроса через ПК → VK → сервер стоит лишних секунд.
			map[string]any{"tag": "remote", "address": dnsServer, "detour": "wg"},
			map[string]any{"tag": "local", "address": "local"},
		},
		"rules": dnsRules,
		"final": "remote",
	}
	// Туннель только IPv4 — не раздаём приложениям IPv6-адреса, до которых
	// через него не достучаться: они тратили бы время на попытки.
	if !hasIPv6(wg.Address) {
		dns["strategy"] = "ipv4_only"
	}

	conf := map[string]any{
		"log": map[string]any{"level": "warn", "timestamp": true, "output": filepath.Join(dir, "sing-box.log")},
		"dns": dns,
		"endpoints": []any{
			map[string]any{
				"type": "wireguard", "tag": "wg", "system": false,
				"mtu": wg.MTU, "address": wg.Address, "private_key": wg.PrivateKey,
				"peers": []any{peer},
				// Пир — клиент VK TURN на 127.0.0.1. Без явного адреса sing-box
				// привязал бы сокет WireGuard к сетевой карте (auto_detect_interface),
				// а с неё Windows не шлёт на loopback: «requested address is not valid».
				"inet4_bind_address": "127.0.0.1",
			},
		},
		"inbounds":  inbounds,
		"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}},
		"route": map[string]any{
			"auto_detect_interface":   withTUN,
			"rules":                   rules,
			"final":                   "wg",
			"default_domain_resolver": "local",
		},
	}
	return json.MarshalIndent(conf, "", "  ")
}

// hasIPv6 reports whether any of the WireGuard interface addresses is IPv6.
func hasIPv6(addrs []string) bool {
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Is6() {
			return true
		}
	}
	return false
}

// StartVKTurn brings up a VK TURN connection: the vk-turn client first, then
// — once it has a DTLS stream through VK — sing-box with WireGuard on top.
func (t *Tunnel) StartVKTurn(cfg *Config, s *Server, withTUN bool) error {
	t.startMu.Lock()
	defer t.startMu.Unlock()
	if t.running() {
		return fmt.Errorf("уже запущено")
	}
	if s.WG == nil {
		return fmt.Errorf("у сервера VK TURN нет конфига WireGuard")
	}
	client := filepath.Join(t.dir, "bin", "vkturn-client.exe")
	if _, err := os.Stat(client); err != nil {
		return fmt.Errorf("не найден bin\\vkturn-client.exe")
	}
	if withTUN && !isAdmin() {
		return fmt.Errorf("для режима «Весь трафик» нужны права администратора — запустите olcvpn через start.cmd")
	}

	cmd := exec.Command(client, //nolint:gosec // app-owned binary and validated arguments
		"-listen", fmt.Sprintf("127.0.0.1:%d", vkturnLocalPort),
		"-peer", s.Peer,
		"-vk-link", s.VKLink,
		"-manual-captcha",
	)
	cmd.Dir = t.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	// Капчу показывает окно olcvpn, а не внешний браузер.
	cmd.Env = append(os.Environ(), "VKTURN_NO_BROWSER=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	bindToApp(cmd.Process)

	t.mu.Lock()
	t.core = cmd
	t.server = s
	t.peerSeen = false
	t.mu.Unlock()

	t.setPhase(phaseStarting, "получаю доступ к звонку VK")
	t.log.addf("VK TURN: %s → %s", s.Name, s.Peer)

	go t.pumpVKTurn(stdout, cfg, s, withTUN)
	go t.pumpVKTurn(stderr, cfg, s, withTUN)
	go t.wait(cmd)
	return nil
}

// pumpVKTurn follows the vk-turn client's log and drives the phases.
func (t *Tunnel) pumpVKTurn(r io.Reader, cfg *Config, s *Server, withTUN bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case captchaURLRe.MatchString(line):
			url := captchaURLRe.FindStringSubmatch(line)[1]
			t.setPhase(phaseCaptcha, "VK просит подтвердить, что вы не робот")
			t.log.add("VK TURN: нужна капча — открываю проверку VK в окне")
			t.notifyCaptcha(url)

		case strings.Contains(line, "[VK Auth] Success"):
			t.notifyCaptcha("")
			t.setPhase(phaseStarting, "доступ к звонку получен, подключаюсь")
			t.log.add("VK TURN: доступ к звонку получен")

		case strings.Contains(line, "Established DTLS"):
			t.mu.Lock()
			first := !t.peerSeen
			t.peerSeen = true
			t.mu.Unlock()
			if !first {
				continue
			}
			t.log.add("VK TURN: канал через TURN-сервер VK установлен")
			if err := t.startVKTurnSingBox(cfg, s, withTUN); err != nil {
				t.log.addf("sing-box: %v", err)
				t.setPhase(phaseError, "sing-box: "+err.Error())
				continue
			}
			if withTUN {
				t.setPhase(phaseConnected, "VK TURN, весь трафик")
			} else {
				t.setPhase(phaseProxy, "VK TURN")
			}

		case strings.Contains(line, "failed to get TURN credentials"),
			strings.Contains(line, "all VK credentials failed"):
			t.log.add("VK TURN: VK не выдал доступ к звонку, пробую снова")

		case vkInteresting(line):
			t.log.add("VK TURN: " + line)
		}
	}
}

// vkInteresting picks the vk-turn client lines worth showing: failures and
// dropped streams. Without them a client that died on, say, a busy port left
// only «завершился» in the log, with no hint why. Captcha proxy lines are
// never shown — they carry VK session tokens.
func vkInteresting(line string) bool {
	for _, skip := range []string{"[Captcha Proxy]", "session_token", "failed to close TURN allocated connection"} {
		if strings.Contains(line, skip) {
			return false
		}
	}
	l := strings.ToLower(line)
	for _, keep := range []string{"fail", "error", "panic", "fatal", "bind", "closed dtls", "not found", "invalid"} {
		if strings.Contains(l, keep) {
			return true
		}
	}
	return false
}

func (t *Tunnel) startVKTurnSingBox(cfg *Config, s *Server, withTUN bool) error {
	t.mu.Lock()
	already := t.singbox != nil
	t.mu.Unlock()
	if already {
		return nil
	}

	sb := filepath.Join(t.dir, "bin", "sing-box.exe")
	if _, err := os.Stat(sb); err != nil {
		return fmt.Errorf("не найден bin\\sing-box.exe")
	}
	conf, err := vkturnSingBoxConfig(cfg, s, t.dir, withTUN)
	if err != nil {
		return err
	}
	// В конфиге закрытый ключ WireGuard — только для текущего пользователя.
	confPath := filepath.Join(t.dir, "sing-box-config.json")
	if err := os.WriteFile(confPath, conf, 0o600); err != nil {
		return err
	}

	cmd := exec.Command(sb, "run", "-c", confPath) //nolint:gosec // app-owned paths
	cmd.Dir = t.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Env = append(os.Environ(),
		"ENABLE_DEPRECATED_LEGACY_DNS_SERVERS=true",
		"ENABLE_DEPRECATED_MISSING_DOMAIN_RESOLVER=true",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	bindToApp(cmd.Process)
	t.mu.Lock()
	t.singbox = cmd
	t.tunOn = withTUN
	t.mu.Unlock()

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			t.log.add("sing-box: " + strings.TrimRight(sc.Text(), "\r"))
		}
	}()
	return nil
}

// switchVKTurnMode flips between proxy-only and system-wide without touching
// the vk-turn client: the channel through VK stays up — and so does the
// captcha the user already passed — while only sing-box is restarted.
func (t *Tunnel) switchVKTurnMode(cfg *Config, s *Server, on bool) error {
	if on && !isAdmin() {
		return fmt.Errorf("для режима «Весь трафик» нужны права администратора — запустите olcvpn через start.cmd")
	}
	t.mu.Lock()
	ready := t.peerSeen
	t.mu.Unlock()
	if !ready {
		return fmt.Errorf("дождитесь подключения — режим можно сменить после него")
	}
	t.stopSingBox()
	if err := t.startVKTurnSingBox(cfg, s, on); err != nil {
		t.setPhase(phaseError, "sing-box: "+err.Error())
		return err
	}
	if on {
		t.setPhase(phaseConnected, "VK TURN, весь трафик")
	} else {
		t.setPhase(phaseProxy, "VK TURN")
	}
	return nil
}

// onVKTurn reports whether the running connection is a VK TURN one.
func (t *Tunnel) onVKTurn() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.core != nil && t.server != nil && t.server.isVKTurn()
}

// pendingCaptcha returns the captcha page the user still has to pass, if any.
func (t *Tunnel) pendingCaptcha() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.captchaURL
}

// notifyCaptcha tells the window to show (url) or to leave (empty) the VK
// captcha page.
func (t *Tunnel) notifyCaptcha(url string) {
	t.mu.Lock()
	t.captchaURL = url
	cb := t.onCaptcha
	t.mu.Unlock()
	if cb != nil {
		cb(url)
	}
}
