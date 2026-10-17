//go:build android

package main

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// В Android нет /etc/resolv.conf, и Go без cgo резолвит через 127.0.0.1:53,
// где никто не отвечает. Поэтому olcvpn спрашивает DNS сети у оболочки и
// ходит к нему сам, а если не вышло — к публичным. Запросы идут мимо
// туннеля: приложение исключено из своего VPN.

// fallbackDNS are tried after the network's own servers. Yandex comes first:
// it stays reachable when a mobile network lets only whitelisted hosts out.
var fallbackDNS = []string{"77.88.8.8", "8.8.8.8", "1.1.1.1"}

var (
	dnsMu   sync.Mutex
	sysDNS  []string
	dnsNext atomic.Uint32
)

// setSystemDNS records the current network's resolvers, as the shell reports
// them on every network change.
func setSystemDNS(servers []string) {
	var ok []string
	for _, s := range servers {
		if a, err := netip.ParseAddr(s); err == nil && !a.IsLoopback() && !a.IsUnspecified() {
			ok = append(ok, a.String())
		}
	}
	dnsMu.Lock()
	sysDNS = ok
	dnsMu.Unlock()
}

// dnsServers is the network's resolvers followed by the public ones.
func dnsServers() []string {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	return append(append([]string{}, sysDNS...), fallbackDNS...)
}

// systemDNS is the one resolver sing-box gets for direct traffic: the
// network's first IPv4 one, else Yandex.
func systemDNS() string {
	for _, s := range dnsServers() {
		if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
			return s
		}
	}
	return fallbackDNS[0]
}

func init() {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		// Go's resolver retries over its own (useless) server list; every
		// attempt gets the next real server, so a dead one costs one timeout.
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			list := dnsServers()
			srv := list[int(dnsNext.Add(1)-1)%len(list)]
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, net.JoinHostPort(srv, "53"))
		},
	}
}
