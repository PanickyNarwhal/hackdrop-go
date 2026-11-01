package lan

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"hackdrop-go/pkg/transport"
)

// Engine discovers and binds the best active Wi-Fi or Ethernet interface with IPv6 Link-Local (`fe80::`)
// connectivity. When combined with the BLE AirDrop Continuity trigger, Apple devices on the same
// wireless network or mobile hotspot can complete the AirDrop mDNS + HTTPS transfer over standard IPv6
// without requiring custom kernel Wi-Fi drivers.
type Engine struct {
	mu        sync.RWMutex
	running   bool
	iface     *net.Interface
	linkLocal net.IP
	ipv4      net.IP
	peers     []transport.PeerLinkInfo
}

func NewEngine() *Engine {
	return &Engine{}
}

func (e *Engine) Name() transport.Mode {
	return transport.ModeLANLinkLocal
}

func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	iface, ipv6LL, ipv4, err := detectBestWirelessInterface()
	if err != nil {
		return err
	}
	e.iface = iface
	e.linkLocal = ipv6LL
	e.ipv4 = ipv4
	e.running = true
	return nil
}

func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = false
	return nil
}

func (e *Engine) Interface() (*net.Interface, net.IP, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.iface == nil || e.linkLocal == nil {
		return nil, nil, fmt.Errorf("no active IPv6 link-local interface bound")
	}
	return e.iface, e.linkLocal, nil
}

func (e *Engine) IPv4() net.IP {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ipv4
}

func (e *Engine) Status() transport.TransportStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()

	ifaceName := "Not Bound"
	llStr := "fe80::"
	if e.iface != nil {
		ifaceName = e.iface.Name
	}
	if e.linkLocal != nil {
		llStr = fmt.Sprintf("%s%%%s", e.linkLocal.String(), ifaceName)
	}

	detail := "Bound to active IPv6 Link-Local interface for zero-driver AirDrop mDNS + HTTPS handshakes"
	if e.ipv4 != nil {
		detail = fmt.Sprintf("Active on %s (IPv6 %s / IPv4 %s)", ifaceName, llStr, e.ipv4.String())
	}

	return transport.TransportStatus{
		Mode:              transport.ModeLANLinkLocal,
		Active:            e.running,
		InterfaceName:     ifaceName,
		IPv6LinkLocal:     llStr,
		HardwareSupported: e.iface != nil,
		DriverBackend:     "Native OS IPv6 Link-Local + Multicast Socket",
		CurrentChannel:    0,
		Detail:            detail,
	}
}

func (e *Engine) Peers() []transport.PeerLinkInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.peers
}

func detectBestWirelessInterface() (*net.Interface, net.IP, net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, nil, err
	}

	var fallbackIface *net.Interface
	var fallbackLL net.IP
	var fallbackV4 net.IP

	for i := range ifaces {
		ifc := ifaces[i]
		if (ifc.Flags&net.FlagUp) == 0 || (ifc.Flags&net.FlagLoopback) != 0 {
			continue
		}
		nameLower := strings.ToLower(ifc.Name)
		if strings.Contains(nameLower, "vethernet") || strings.Contains(nameLower, "wsl") ||
			strings.Contains(nameLower, "docker") || strings.Contains(nameLower, "vmware") ||
			strings.Contains(nameLower, "virtualbox") || strings.Contains(nameLower, "zerotier") ||
			strings.Contains(nameLower, "tailscale") {
			continue
		}

		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}

		var llIP net.IP
		var v4IP net.IP
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			if ip4 := ip.To4(); ip4 != nil {
				// Skip VirtualBox Host-Only default IP (192.168.56.1) and Tailscale CGNAT (100.64.0.0/10)
				if ip4.String() == "192.168.56.1" || ip4[0] == 100 {
					continue
				}
				v4IP = ip4
			} else if ip.IsLinkLocalUnicast() {
				llIP = ip
			}
		}

		if llIP == nil && len(ifc.HardwareAddr) == 6 {
			llIP = transport.DeriveEUI64LinkLocal(ifc.HardwareAddr)
		}

		if llIP != nil && v4IP != nil {
			// Prefer Wi-Fi / WLAN named interfaces
			if strings.Contains(nameLower, "wi-fi") || strings.Contains(nameLower, "wlan") || strings.Contains(nameLower, "wl") {
				return &ifc, llIP, v4IP, nil
			}
			if fallbackIface == nil {
				fallbackIface = &ifc
				fallbackLL = llIP
				fallbackV4 = v4IP
			}
		}
	}

	if fallbackIface != nil {
		return fallbackIface, fallbackLL, fallbackV4, nil
	}
	return nil, nil, nil, fmt.Errorf("no suitable network interface found")
}
