//go:build android

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/control"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/logger"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/x/list"
	"github.com/sagernet/sing/service"
)

// В Android sing-box работает не отдельным процессом, а библиотекой внутри
// olcvpn: создать TUN без root нельзя, его открывает VpnService оболочки, а
// дескриптор оболочка передаёт сюда. Конфиг — тот же, что строится для ПК
// (singBoxConfig, vkturnSingBoxConfig), с поправками из platform_android.go.

// tunShell is how sing-box reaches the shell's VpnService; main sets it.
var tunShell interface {
	openTun(req shellMsg) (int, error)
	closeTun()
}

type boxProc struct {
	b       *box.Box
	cancel  context.CancelFunc
	withTUN bool
	once    sync.Once
}

func (p *boxProc) stop() {
	p.once.Do(func() {
		tunnelTraffic.boxes.Add(-1)
		_ = p.b.Close()
		p.cancel()
		if p.withTUN && tunShell != nil {
			tunShell.closeTun()
		}
	})
}

// runSingBox starts sing-box in-process with conf. Nothing goes to disk: the
// config holds the WireGuard key, and here it never has to leave memory.
func (t *Tunnel) runSingBox(conf []byte, withTUN bool) (singBoxProc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = include.Context(ctx)
	ctx = service.ContextWith[adapter.PlatformInterface](ctx, &androidPlatform{})

	var opts option.Options
	if err := json.UnmarshalContext(ctx, conf, &opts); err != nil {
		cancel()
		return nil, fmt.Errorf("конфиг sing-box: %w", err)
	}
	// Журнал sing-box — в sing-box.log, как на ПК. PlatformLogWriter здесь не
	// годится: с ним sing-box 1.13 требует clash API и пишет файл кеша.
	b, err := box.New(box.Options{Context: ctx, Options: opts})
	if err != nil {
		cancel()
		return nil, err
	}
	b.Router().AppendTracker(trafficTracker{})
	tunnelTraffic.boxes.Add(1)
	p := &boxProc{b: b, cancel: cancel, withTUN: withTUN}
	if err := b.Start(); err != nil {
		p.stop()
		return nil, err
	}
	return p, nil
}

// tunnelTraffic counts the bytes sing-box carries through the tunnel, for the
// speed in the notification. Through the tunnel means routed anywhere but
// "direct": what goes around it (private networks, the bypass lists) is not
// the tunnel's speed.
var tunnelTraffic struct {
	up, down atomic.Int64
	boxes    atomic.Int32 // сколько sing-box сейчас считает
}

type trafficTracker struct{}

// An inbound connection is read for what the app sends and written with what
// comes back — the same split sing-box's own clash API makes.
func (trafficTracker) RoutedConnection(_ context.Context, conn net.Conn, _ adapter.InboundContext,
	_ adapter.Rule, out adapter.Outbound) net.Conn {
	if out == nil || out.Tag() == "direct" {
		return conn
	}
	return bufio.NewInt64CounterConn(conn,
		[]*atomic.Int64{&tunnelTraffic.up}, []*atomic.Int64{&tunnelTraffic.down})
}

func (trafficTracker) RoutedPacketConnection(_ context.Context, conn N.PacketConn, _ adapter.InboundContext,
	_ adapter.Rule, out adapter.Outbound) N.PacketConn {
	if out == nil || out.Tag() == "direct" {
		return conn
	}
	return bufio.NewInt64CounterPacketConn(conn,
		[]*atomic.Int64{&tunnelTraffic.up}, nil, []*atomic.Int64{&tunnelTraffic.down}, nil)
}

// androidPlatform is the part of sing-box's platform interface olcvpn needs:
// the TUN from the shell. The app is excluded from its own VPN, so sockets
// need no protecting and no interface binding, and everything else is off.
type androidPlatform struct{}

var _ adapter.PlatformInterface = (*androidPlatform)(nil)

func (*androidPlatform) Initialize(adapter.NetworkManager) error     { return nil }
func (*androidPlatform) UsePlatformAutoDetectInterfaceControl() bool { return false }
func (*androidPlatform) AutoDetectInterfaceControl(int) error         { return nil }
func (*androidPlatform) UsePlatformInterface() bool                   { return true }

func (*androidPlatform) OpenInterface(options *tun.Options, _ option.TunPlatformOptions) (tun.Tun, error) {
	if tunShell == nil {
		return nil, errors.New("оболочка приложения не на связи")
	}
	routes, err := options.BuildAutoRouteRanges(true)
	if err != nil {
		return nil, err
	}
	req := shellMsg{MTU: int(options.MTU)}
	for _, p := range options.Inet4Address {
		req.Addrs = append(req.Addrs, p.String())
	}
	for _, p := range options.Inet6Address {
		req.Addrs = append(req.Addrs, p.String())
	}
	for _, p := range routes {
		req.Routes = append(req.Routes, p.String())
	}
	if len(options.Inet4Address) > 0 {
		// Тот же адрес, что ловит правило hijack-dns в конфиге.
		req.DNS = []string{options.Inet4Address[0].Addr().Next().String()}
	}

	fd, err := tunShell.openTun(req)
	if err != nil {
		return nil, err
	}
	name, err := tunName(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	options.Name = name
	options.FileDescriptor = fd
	if options.InterfaceMonitor != nil {
		options.InterfaceMonitor.RegisterMyInterface(name)
	}
	return tun.New(*options)
}

func (*androidPlatform) UsePlatformDefaultInterfaceMonitor() bool { return true }

func (*androidPlatform) CreateDefaultInterfaceMonitor(logger.Logger) tun.DefaultInterfaceMonitor {
	return &quietMonitor{}
}

// Список интерфейсов Android 11+ приложению через netlink не отдаёт, а
// sing-box он здесь и не нужен: ни к чему не привязываемся.
func (*androidPlatform) UsePlatformNetworkInterfaces() bool { return true }
func (*androidPlatform) NetworkInterfaces() ([]adapter.NetworkInterface, error) {
	return nil, nil
}

func (*androidPlatform) UnderNetworkExtension() bool                { return false }
func (*androidPlatform) NetworkExtensionIncludeAllNetworks() bool   { return false }
func (*androidPlatform) ClearDNSCache()                             {}
func (*androidPlatform) RequestPermissionForWIFIState() error       { return nil }
func (*androidPlatform) ReadWIFIState() adapter.WIFIState           { return adapter.WIFIState{} }
func (*androidPlatform) SystemCertificates() []string               { return nil }
func (*androidPlatform) UsePlatformConnectionOwnerFinder() bool     { return false }
func (*androidPlatform) UsePlatformWIFIMonitor() bool               { return false }
func (*androidPlatform) UsePlatformNotification() bool              { return false }
func (*androidPlatform) MyInterfaceAddress() []netip.Addr           { return nil }
func (*androidPlatform) SendNotification(*adapter.Notification) error { return nil }

func (*androidPlatform) FindConnectionOwner(*adapter.FindConnectionOwnerRequest) (*adapter.ConnectionOwner, error) {
	return nil, errors.New("не поддерживается")
}

// quietMonitor is a default-interface monitor that never reports a change.
// Nothing in olcvpn's configs binds to an interface, so sing-box has no use
// for the answer; reporting "no interface" instead would pause every
// connection.
type quietMonitor struct {
	mu        sync.Mutex
	callbacks list.List[tun.DefaultInterfaceUpdateCallback]
	mine      string
}

func (*quietMonitor) Start() error                         { return nil }
func (*quietMonitor) Close() error                         { return nil }
func (*quietMonitor) DefaultInterface() *control.Interface { return nil }
func (*quietMonitor) OverrideAndroidVPN() bool             { return false }
func (*quietMonitor) AndroidVPNEnabled() bool              { return false }

func (m *quietMonitor) RegisterCallback(cb tun.DefaultInterfaceUpdateCallback) *list.Element[tun.DefaultInterfaceUpdateCallback] {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callbacks.PushBack(cb)
}

func (m *quietMonitor) UnregisterCallback(e *list.Element[tun.DefaultInterfaceUpdateCallback]) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbacks.Remove(e)
}

func (m *quietMonitor) RegisterMyInterface(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mine = name
}

func (m *quietMonitor) MyInterface() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mine
}
