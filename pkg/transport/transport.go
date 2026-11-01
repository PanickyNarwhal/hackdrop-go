package transport

import (
	"context"
	"net"
	"time"
)

// Mode identifies the underlying wireless peer-to-peer link mechanism.
type Mode string

const (
	// ModeAWDL uses Apple Wireless Direct Link (802.11 Action frames with OUI 00:17:f2 + channel hopping on 6/44/149).
	ModeAWDL Mode = "AWDL (Raw 802.11 P2P)"

	// ModeWiFiAware uses Wi-Fi Alliance Neighbor Awareness Networking (NAN / Wi-Fi Aware) supported on iOS/macOS 26+.
	ModeWiFiAware Mode = "Wi-Fi Aware (NAN P2P)"

	// ModeLANLinkLocal uses standard IPv6 Link-Local (fe80::) over shared Wi-Fi / Hotspot / USB-C / Ethernet.
	ModeLANLinkLocal Mode = "IPv6 Link-Local (Zero-Driver)"
)

// PeerLinkInfo describes a low-level P2P radio peer discovered at the 802.11 / NAN / Link-Local layer.
type PeerLinkInfo struct {
	MACAddress     net.HardwareAddr `json:"macAddress"`
	IPv6LinkLocal  net.IP           `json:"ipv6LinkLocal"`
	InterfaceName  string           `json:"interfaceName"`
	TransportMode  Mode             `json:"transportMode"`
	Channel        int              `json:"channel"`
	RSSI           int              `json:"rssi"`
	PeerMasterRank uint64           `json:"peerMasterRank,omitempty"`
	SupportsNAN    bool             `json:"supportsNan"`
	LastSeen       time.Time        `json:"lastSeen"`
}

// Engine defines the contract for a low-level P2P radio transport (AWDL, Wi-Fi Aware, or Link-Local).
type Engine interface {
	// Name returns the human-readable transport name and mode.
	Name() Mode

	// Start initializes the radio interface (e.g., monitor mode + virtual awdl0, NAN session, or IPv6 multicast bind).
	Start(ctx context.Context) error

	// Stop tears down the transport cleanly.
	Stop() error

	// Interface returns the OS network interface carrying the IPv6 link-local (fe80::) traffic for AirDrop HTTPS/mDNS.
	Interface() (*net.Interface, net.IP, error)

	// Status returns a diagnostic description of driver readiness on the current OS (Windows NDIS/Npcap vs Linux nl80211).
	Status() TransportStatus

	// Peers returns currently synchronized P2P radio peers.
	Peers() []PeerLinkInfo
}

// TransportStatus reports real-time readiness of a P2P radio transport layer to the Wails GUI.
type TransportStatus struct {
	Mode              Mode   `json:"mode"`
	Active            bool   `json:"active"`
	InterfaceName     string `json:"interfaceName"`
	IPv6LinkLocal     string `json:"ipv6LinkLocal"`
	HardwareSupported bool   `json:"hardwareSupported"`
	DriverBackend     string `json:"driverBackend"`
	CurrentChannel    int    `json:"currentChannel"`
	Detail            string `json:"detail"`
}

// DeriveEUI64LinkLocal computes the standard RFC 4291 EUI-64 IPv6 link-local address (fe80::...)
// from a 6-byte IEEE 802 MAC address — the exact mapping used by Apple's awdl0 interface.
func DeriveEUI64LinkLocal(mac net.HardwareAddr) net.IP {
	if len(mac) != 6 {
		return nil
	}
	ip := make(net.IP, 16)
	ip[0] = 0xfe
	ip[1] = 0x80
	// bytes 2..7 are 0x00
	ip[8] = mac[0] ^ 0x02 // Invert universal/local bit
	ip[9] = mac[1]
	ip[10] = mac[2]
	ip[11] = 0xff
	ip[12] = 0xfe
	ip[13] = mac[3]
	ip[14] = mac[4]
	ip[15] = mac[5]
	return ip
}
