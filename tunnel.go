package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type coreKind int

const (
	coreUnknown coreKind = iota
	coreFlags            // v0.2.x: every setting is a CLI flag
	coreYAML             // upstream: single positional config.yaml
)

func (k coreKind) String() string {
	switch k {
	case coreFlags:
		return "flags"
	case coreYAML:
		return "yaml"
	default:
		return "unknown"
	}
}

// detectCoreKind asks the binary for its usage text and classifies it.
func detectCoreKind(bin string) coreKind {
	cmd := exec.Command(bin, "--help")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, _ := cmd.CombinedOutput()
	text := string(out)
	switch {
	case strings.Contains(text, "-carrier"):
		return coreFlags
	case strings.Contains(text, "config.yaml"):
		return coreYAML
	}
	return coreUnknown
}

type phase string

const (
	phaseStopped   phase = "stopped"
	phaseStarting  phase = "starting"
	phaseWaiting   phase = "waiting"   // core up, no SOCKS yet
	phaseProxy     phase = "proxy"     // SOCKS listening
	phaseConnected phase = "connected" // SOCKS + TUN
	phaseError     phase = "error"
)

// Tunnel owns the core process and the optional sing-box TUN process.
type Tunnel struct {
	dir string
	log *logBus

	mu        sync.Mutex
	core      *exec.Cmd
	singbox   *exec.Cmd
	phase     phase
	detail    string
	server    *Server
	peerSeen  bool
	zeroTicks int
	lastRTT   int
	stopping  bool
}

func newTunnel(dir string, log *logBus) *Tunnel {
	return &Tunnel{dir: dir, log: log, phase: phaseStopped}
}

func (t *Tunnel) status() (phase, string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.phase, t.detail, t.peerSeen
}

func (t *Tunnel) setPhase(p phase, detail string) {
	t.mu.Lock()
	t.phase = p
	t.detail = detail
	t.mu.Unlock()
}

// SetTUN turns the system route on or off without dropping the session, so a
// user can switch between "whole system" and "proxy only" mid-connection.
func (t *Tunnel) SetTUN(cfg *Config, on bool) error {
	if !on {
		t.stopSingBox()
		if t.running() {
			t.setPhase(phaseProxy, "туннель работает")
		}
		return nil
	}
	ph, _, _ := t.status()
	if ph != phaseProxy && ph != phaseConnected {
		return fmt.Errorf("сначала подключитесь")
	}
	return t.startSingBox(cfg)
}

// rtt returns the last round-trip the core reported on its control stream.
func (t *Tunnel) rtt() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastRTT
}

// tunActive reports whether sing-box currently holds the system route.
func (t *Tunnel) tunActive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.singbox != nil
}

func (t *Tunnel) running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.core != nil
}

// Start launches the core for srv and, once SOCKS is up, sing-box when asked.
func (t *Tunnel) Start(cfg *Config, srv *Server, bin string, kind coreKind, withTUN bool) error {
	if t.running() {
		return fmt.Errorf("уже запущено")
	}
	if srv == nil {
		return fmt.Errorf("сервер не выбран")
	}

	dataDir := filepath.Join(t.dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	if err := ensureNameFiles(dataDir); err != nil {
		t.log.addf("предупреждение: не удалось подготовить data/: %v", err)
	}

	var cmd *exec.Cmd
	switch kind {
	case coreFlags:
		cmd = exec.Command(bin, flagArgs(cfg, srv, dataDir)...) //nolint:gosec // paths are app-owned
	case coreYAML:
		cfgPath := filepath.Join(t.dir, "core-config.yaml")
		if err := os.WriteFile(cfgPath, []byte(yamlConfig(cfg, srv)), 0o600); err != nil {
			return err
		}
		cmd = exec.Command(bin, cfgPath) //nolint:gosec // paths are app-owned
	default:
		return fmt.Errorf("не удалось определить тип ядра %s", filepath.Base(bin))
	}

	cmd.Dir = t.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	// The core talks to the conference API directly; a stale system proxy
	// (v2rayN / sing-box on 127.0.0.1:10809) would otherwise break the join.
	cmd.Env = append(os.Environ(),
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
		"http_proxy=", "https_proxy=", "all_proxy=",
	)

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

	t.mu.Lock()
	t.core = cmd
	t.server = srv
	t.peerSeen = false
	t.zeroTicks = 0
	t.lastRTT = 0
	t.stopping = false
	t.mu.Unlock()

	t.setPhase(phaseStarting, "запуск ядра")
	t.log.addf("ядро: %s (%s), сервер %q, транспорт %s", filepath.Base(bin), kind, srv.Name, srv.Transport)

	go t.pump(stdout, cfg, withTUN)
	go t.pump(stderr, cfg, withTUN)
	go t.wait(cmd)
	return nil
}

// pump reads one core stream, mirrors interesting lines and drives the phase.
func (t *Tunnel) pump(r io.Reader, cfg *Config, withTUN bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		if interesting(line) {
			t.log.add(line)
		}

		switch {
		case strings.Contains(line, "SOCKS5 server listening"):
			t.setPhase(phaseProxy, "SOCKS5 поднят")
			if withTUN {
				if err := t.startSingBox(cfg); err != nil {
					t.log.addf("sing-box: %v", err)
					t.setPhase(phaseError, "sing-box: "+err.Error())
				}
			}
		case strings.Contains(line, "peer latched"):
			t.markPeer()
			t.log.add("сервер найден — туннель установлен")
		case strings.Contains(line, "wait for peer"):
			t.stall("сервер не отвечает в комнате")
		case strings.Contains(line, "read welcome"), strings.Contains(line, "open control stream"):
			t.stall("сервер не прислал welcome")
		case strings.Contains(line, "METRICS mux"):
			t.onMetrics(line, withTUN)
		case strings.Contains(line, "control alive"):
			// The core's keepalive carries a round-trip time; it is both proof
			// the link is healthy and the number to show as "ping".
			if ms, ok := metricValue(line, "rtt="); ok {
				t.mu.Lock()
				t.lastRTT = int(ms + 0.5)
				t.peerSeen = true
				t.mu.Unlock()
			}
			if withTUN && t.tunActive() {
				t.setPhase(phaseConnected, "туннель работает")
			} else {
				t.setPhase(phaseProxy, "туннель работает")
			}
		case strings.Contains(line, "session ") && strings.Contains(line, "opened"):
			// The YAML-era core prints no metrics; an opened session is its
			// equivalent proof that the far side answered.
			t.markPeer()
			if withTUN {
				t.setPhase(phaseConnected, "туннель работает")
			} else {
				t.setPhase(phaseProxy, "туннель работает")
			}
		}
	}
}

// stall reports a handshake that did not complete. Once a session has already
// been established these are ordinary reconnect churn on a lossy link, not a
// dead server, and the status should say so.
func (t *Tunnel) stall(reason string) {
	t.mu.Lock()
	seen := t.peerSeen
	t.mu.Unlock()
	if seen {
		t.setPhase(phaseWaiting, "переподключение (канал теряет пакеты)")
		return
	}
	t.setPhase(phaseWaiting, reason)
}

func (t *Tunnel) markPeer() {
	t.mu.Lock()
	t.peerSeen = true
	t.mu.Unlock()
}

// onMetrics reads the core's periodic mux counters. They are the only signal
// the v0.2.x core gives about whether the far side actually answers: SOCKS
// listens either way, so a permanent 0 KB/s means the room has no server.
func (t *Tunnel) onMetrics(line string, withTUN bool) {
	rx, ok := metricValue(line, "rx=")
	if !ok {
		return
	}
	tx, _ := metricValue(line, "tx=")

	if rx > 0 || tx > 0 {
		t.mu.Lock()
		t.zeroTicks = 0
		first := !t.peerSeen
		t.peerSeen = true
		t.mu.Unlock()
		if first {
			t.log.add("данные пошли — туннель работает")
		}
		if withTUN {
			t.setPhase(phaseConnected, "трафик идёт")
		} else {
			t.setPhase(phaseProxy, "трафик идёт")
		}
		return
	}

	t.mu.Lock()
	t.zeroTicks++
	n := t.zeroTicks
	seen := t.peerSeen
	t.mu.Unlock()

	if n == 4 && !seen {
		t.setPhase(phaseWaiting, "SOCKS5 поднят, но сервер молчит (0 KB/s)")
		t.log.add("сервер в комнате не отвечает: за 20 секунд ни одного байта")
	}
}

// metricValue pulls the number that follows key in a METRICS line.
func metricValue(line, key string) (float64, bool) {
	i := strings.Index(line, key)
	if i < 0 {
		return 0, false
	}
	rest := line[i+len(key):]
	end := 0
	for end < len(rest) && (rest[end] == '.' || (rest[end] >= '0' && rest[end] <= '9')) {
		end++
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(rest[:end], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (t *Tunnel) wait(cmd *exec.Cmd) {
	err := cmd.Wait()
	t.mu.Lock()
	stopping := t.stopping
	if t.core == cmd {
		t.core = nil
	}
	t.mu.Unlock()
	t.stopSingBox()
	if stopping {
		t.setPhase(phaseStopped, "остановлено")
		t.log.add("остановлено")
		return
	}
	t.setPhase(phaseError, "ядро завершилось")
	if err != nil {
		t.log.addf("ядро завершилось: %v", err)
	} else {
		t.log.add("ядро завершилось")
	}
}

func (t *Tunnel) startSingBox(cfg *Config) error {
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
	if !isAdmin() {
		return fmt.Errorf("для TUN нужны права администратора — перезапустите от имени администратора")
	}

	confPath := filepath.Join(t.dir, "sing-box-config.json")
	conf, err := singBoxConfig(cfg, t.dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(confPath, conf, 0o600); err != nil {
		return err
	}

	cmd := exec.Command(sb, "run", "-c", confPath) //nolint:gosec // paths are app-owned
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

	t.mu.Lock()
	t.singbox = cmd
	t.mu.Unlock()

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			t.log.add("sing-box: " + strings.TrimRight(sc.Text(), "\r"))
		}
	}()

	t.setPhase(phaseConnected, "TUN активен")
	t.log.add("sing-box: системный TUN поднят")
	return nil
}

func (t *Tunnel) stopSingBox() {
	t.mu.Lock()
	cmd := t.singbox
	t.singbox = nil
	t.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

// Stop tears both processes down.
func (t *Tunnel) Stop() {
	t.mu.Lock()
	t.stopping = true
	cmd := t.core
	t.mu.Unlock()

	t.stopSingBox()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}

	t.mu.Lock()
	t.core = nil
	t.mu.Unlock()
	t.setPhase(phaseStopped, "остановлено")
}

func flagArgs(cfg *Config, s *Server, dataDir string) []string {
	args := []string{
		"-mode", "cnc",
		"-link", "direct",
		"-carrier", s.Carrier,
		"-id", s.RoomID,
		"-key", s.Key,
		"-transport", s.Transport,
		"-dns", cfg.DNS,
		"-data", dataDir,
		"-socks-host", cfg.SocksHost,
		"-socks-port", strconv.Itoa(cfg.SocksPort),
	}
	if s.Transport == "vp8channel" {
		args = append(args,
			"-vp8-fps", strconv.Itoa(s.VP8FPS),
			"-vp8-batch", strconv.Itoa(s.VP8Batch),
		)
	}
	return args
}

func yamlConfig(cfg *Config, s *Server) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# generated by olcvpn\nmode: cnc\n\n")
	fmt.Fprintf(&b, "auth:\n  provider: %s\n\n", s.Carrier)
	fmt.Fprintf(&b, "room:\n  id: %q\n", s.RoomID)
	if s.ClientID != "" {
		fmt.Fprintf(&b, "  channel: %q\n", s.ClientID)
	}
	fmt.Fprintf(&b, "\ncrypto:\n  key: %q\n\n", s.Key)
	fmt.Fprintf(&b, "net:\n  transport: %s\n  dns: %q\n\n", s.Transport, cfg.DNS)
	fmt.Fprintf(&b, "liveness:\n  interval: 10s\n  timeout: 5s\n  failures: 3\n\n")
	fmt.Fprintf(&b, "socks:\n  host: %q\n  port: %d\n\n", cfg.SocksHost, cfg.SocksPort)
	if s.Transport == "vp8channel" {
		fmt.Fprintf(&b, "vp8:\n  fps: %d\n  batch_size: %d\n\n", s.VP8FPS, s.VP8Batch)
	}
	fmt.Fprintf(&b, "data: data\ndebug: true\n")
	return b.String()
}

// singBoxConfig mirrors the layout the vendor GUI uses: a TUN inbound, the
// core's SOCKS as the only proxy outbound, and direct rules for the two
// binaries plus whatever the user pinned, so the tunnel cannot loop on itself.
func singBoxConfig(cfg *Config, dir string) ([]byte, error) {
	rules := []any{
		map[string]any{"ip_cidr": []string{"172.19.0.2/32"}, "port": 53, "action": "hijack-dns"},
		map[string]any{"protocol": "dns", "action": "hijack-dns"},
		map[string]any{"process_name": []string{"olcrtc-core.exe", "olcrtc.exe", "sing-box.exe", "olcvpn.exe"}, "outbound": "direct"},
	}
	if hosts := csv(cfg.DirectHosts); len(hosts) > 0 {
		rules = append(rules, map[string]any{"domain_suffix": hosts, "outbound": "direct"})
	}
	if ips := csv(cfg.DirectIPs); len(ips) > 0 {
		rules = append(rules, map[string]any{"ip_cidr": ips, "outbound": "direct"})
	}
	rules = append(rules, map[string]any{"ip_is_private": true, "outbound": "direct"})

	dnsRules := []any{
		map[string]any{"domain_suffix": []string{"local", "lan"}, "server": "local"},
	}
	if hosts := csv(cfg.DirectHosts); len(hosts) > 0 {
		dnsRules = append([]any{map[string]any{"domain_suffix": hosts, "server": "local"}}, dnsRules...)
	}

	conf := map[string]any{
		"log": map[string]any{
			"level":     "warn",
			"timestamp": true,
			"output":    filepath.Join(dir, "sing-box.log"),
		},
		"dns": map[string]any{
			"servers": []any{
				map[string]any{"tag": "remote", "address": "tcp://1.1.1.1", "detour": "proxy"},
				map[string]any{"tag": "local", "address": "local"},
			},
			"rules": dnsRules,
			"final": "remote",
		},
		"inbounds": []any{
			map[string]any{
				"type": "tun", "tag": "tun-in", "interface_name": "olcvpn-tun",
				"address":    []string{"172.19.0.1/30"},
				"auto_route": true, "strict_route": true, "stack": "mixed",
			},
		},
		"outbounds": []any{
			map[string]any{"type": "socks", "tag": "proxy", "server": cfg.SocksHost, "server_port": cfg.SocksPort, "version": "5"},
			map[string]any{"type": "direct", "tag": "direct"},
		},
		"route": map[string]any{
			"auto_detect_interface":   true,
			"rules":                   rules,
			"final":                   "proxy",
			"default_domain_resolver": "local",
		},
	}
	return json.MarshalIndent(conf, "", "  ")
}

func csv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// interesting filters the core's very chatty pion output down to lines a
// person would actually act on.
func interesting(line string) bool {
	noisy := []string{
		"[ice]", "[pc]", "[sctp]", "[mux]", "[srtp]", "[dtls]", "[rtp]", "[twcc]", "[nack]",
		"control alive", // parsed for the ping readout, too frequent to print
	}
	for _, n := range noisy {
		if strings.Contains(line, n) {
			return false
		}
	}
	keep := []string{
		"SOCKS5", "peer latched", "wait for peer", "welcome", "failover", "KCP started",
		"remote video track", "session", "reconnect", "error", "Error", "ERROR",
		"failed", "timeout", "METRICS mux", "listening", "Shutting down",
	}
	for _, k := range keep {
		if strings.Contains(line, k) {
			return true
		}
	}
	return false
}

func ensureNameFiles(dataDir string) error {
	for _, name := range []string{"names", "surnames"} {
		p := filepath.Join(dataDir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.WriteFile(p, []byte(fallbackNames), 0o600); err != nil {
			return err
		}
	}
	return nil
}

const fallbackNames = "Alex\nSam\nJordan\nMorgan\nRiley\nCasey\nTaylor\nJamie\nAvery\nQuinn\n"
