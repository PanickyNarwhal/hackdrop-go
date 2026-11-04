package daemon

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"hackdrop-go/pkg/airdrop"
	"hackdrop-go/pkg/ble"
	"hackdrop-go/pkg/mdns"
	"hackdrop-go/pkg/transport"
	"hackdrop-go/pkg/transport/awdl"
	"hackdrop-go/pkg/transport/lan"
	"hackdrop-go/pkg/transport/wifiaware"
)

// PacketLog represents a real-time radio/protocol frame captured or transmitted by the stack.
type PacketLog struct {
	Timestamp string `json:"timestamp"`
	Layer     string `json:"layer"`
	Direction string `json:"direction"`
	Summary   string `json:"summary"`
	HexBytes  string `json:"hexBytes"`
}

// SystemState aggregates the full status of the local HackDrop node for the UI.
type SystemState struct {
	DeviceID        string                      `json:"deviceId"`
	ComputerName    string                      `json:"computerName"`
	ModelName       string                      `json:"modelName"`
	OS              string                      `json:"os"`
	LocalIPv4       string                      `json:"localIpv4"`
	LocalIPv6       string                      `json:"localIpv6"`
	VisibilityMode  string                      `json:"visibilityMode"` // "Everyone", "Off"
	AutoAccept      bool                        `json:"autoAccept"`
	HTTPSPort       int                         `json:"httpsPort"`
	DownloadDir     string                      `json:"downloadDir"`
	BLEAdvertising  bool                        `json:"bleAdvertising"`
	BLEFrameHex     string                      `json:"bleFrameHex"`
	Transports      []transport.TransportStatus `json:"transports"`
	DiscoveredPeers []mdns.PeerService          `json:"discoveredPeers"`
	BLEPeers        []ble.DiscoveredBLEPeer     `json:"blePeers"`
	RecentPackets   []PacketLog                 `json:"recentPackets"`
	RecentEvents    []airdrop.TransferEvent     `json:"recentEvents"`
}

// Coordinator manages the BLE Continuity Advertiser, AWDL + Wi-Fi Aware + LAN transports,
// Bonjour mDNS responder, and AirDrop HTTPS server.
type Coordinator struct {
	mu             sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
	deviceID       string
	computerName   string
	modelName      string
	visibilityMode string
	autoAccept     bool
	downloadDir    string

	bleFrame      *ble.AirDropFrame
	blePeers      map[string]ble.DiscoveredBLEPeer
	awdlEngine    *awdl.Engine
	nanEngine     *wifiaware.Engine
	lanEngine     *lan.Engine
	mdnsEngine    *mdns.Engine
	airdropServer *airdrop.Server

	packetLogs []PacketLog
	events     []airdrop.TransferEvent
}

func NewCoordinator() (*Coordinator, error) {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "HackDrop-PC"
	}

	deviceID := "a4e027" + fmt.Sprintf("%06x", time.Now().Unix()%0xffffff)
	modelName := "PC-Windows-11"
	switch runtime.GOOS {
	case "linux":
		modelName = "Linux-Workstation"
	case "darwin":
		modelName = "MacBookPro16,1"
	}

	home, _ := os.UserHomeDir()
	downloadDir := filepath.Join(home, "Downloads", "HackDrop")
	_ = os.MkdirAll(downloadDir, 0755)

	c := &Coordinator{
		deviceID:       deviceID,
		computerName:   hostname,
		modelName:      modelName,
		visibilityMode: "Everyone",
		autoAccept:     false,
		downloadDir:    downloadDir,
		bleFrame:       ble.NewAirDropFrame("", "", "", true),
		blePeers:       make(map[string]ble.DiscoveredBLEPeer),
		awdlEngine:     awdl.NewEngine("awdl0"),
		nanEngine:      wifiaware.NewEngine(),
		lanEngine:      lan.NewEngine(),
	}

	c.mdnsEngine = mdns.NewEngine(
		deviceID,
		hostname,
		modelName,
		airdrop.DefaultAirDropPort,
		func(peer mdns.PeerService) {
			c.recordPacket("mDNS Bonjour", "RX",
				fmt.Sprintf("Discovered peer %s (%s:%d)", peer.DisplayName, peer.IPv4Addr+peer.IPv6Addr, peer.Port),
				fmt.Sprintf("PTR %s._airdrop._tcp.local. flags=0x%x", peer.InstanceID, peer.Flags))
		},
	)

	srv, err := airdrop.NewServer(
		deviceID,
		hostname,
		modelName,
		downloadDir,
		airdrop.DefaultAirDropPort,
		func(ev airdrop.TransferEvent) {
			c.mu.Lock()
			c.events = append([]airdrop.TransferEvent{ev}, c.events...)
			if len(c.events) > 40 {
				c.events = c.events[:40]
			}
			c.mu.Unlock()

			c.recordPacket("AirDrop HTTPS", "RX",
				fmt.Sprintf("AirDrop /%s event from %s", ev.Type, ev.PeerName),
				ev.TransferID)
			if ev.Type == "discover" {
				if remoteIP, _, err := net.SplitHostPort(ev.PeerName); err == nil && remoteIP != "" && remoteIP != "127.0.0.1" && remoteIP != "::1" {
					go func(ip, nameHint string) {
						for _, existing := range c.mdnsEngine.Peers() {
							if existing.IPv4Addr == ip || existing.IPv6Addr == ip {
								return
							}
						}
						c.AddManualPeer(nameHint, "", ip, airdrop.DefaultAirDropPort)
					}(remoteIP, ev.TransferID)
				}
			}
		},
	)
	if err != nil {
		return nil, err
	}
	c.airdropServer = srv

	return c, nil
}

func (c *Coordinator) Start(parentCtx context.Context) error {
	ctx, cancel := context.WithCancel(parentCtx)
	c.ctx = ctx
	c.cancel = cancel

	_ = c.lanEngine.Start(ctx)
	_ = c.awdlEngine.Start(ctx)
	_ = c.nanEngine.Start(ctx)

	if err := c.airdropServer.Start(ctx); err != nil {
		return err
	}

	iface, ipv6LL, _ := c.lanEngine.Interface()
	_ = c.mdnsEngine.Start(ctx, iface, ipv6LL, c.lanEngine.IPv4())

	bleBytes := c.bleFrame.EncodeManufacturerData(true)
	c.recordPacket("BLE 0x4c00", "TX",
		"Broadcasting Apple Continuity AirDrop Trigger (Type 0x05, Len 18, NAN=1)",
		hex.EncodeToString(bleBytes))

	awdlPSF := c.awdlEngine.BuildActionFrame(awdl.SubtypePSF)
	c.recordPacket("AWDL 802.11", "TX",
		"Constructed AWDL Periodic Sync Frame (OUI 00:17:f2, Social Channels 6/44/149)",
		hex.EncodeToString(awdlPSF[:min(len(awdlPSF), 48)]))

	nanSDF := c.nanEngine.BuildServiceDiscoveryFrame(true)
	c.recordPacket("Wi-Fi Aware NAN", "TX",
		"Constructed NAN Service Discovery Frame (OUI 50:6f:9a:13, _airdrop._tcp)",
		hex.EncodeToString(nanSDF))

	// Run an initial non-blocking subnet scan in background so peers on the same Wi-Fi show up automatically
	go func() {
		time.Sleep(800 * time.Millisecond)
		c.ScanLocalSubnet()
	}()

	go c.telemetryLoop(ctx)
	return nil
}

func (c *Coordinator) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	_ = c.awdlEngine.Stop()
	_ = c.nanEngine.Stop()
	_ = c.lanEngine.Stop()
	c.mdnsEngine.Stop()
}

func (c *Coordinator) TriggerBLEWakeup() string {
	bleBytes := c.bleFrame.EncodeManufacturerData(true)
	hexStr := hex.EncodeToString(bleBytes)
	go func() {
		backend, _ := ble.BroadcastOSAdvertisement(c.ctx, c.bleFrame)
		c.recordPacket("BLE 0x4c00", "TX",
			fmt.Sprintf("Transmitted 22B Continuity AirDrop Frame via %s", backend),
			hexStr)
		c.ScanLocalSubnet()
	}()
	return hexStr
}

// AddManualPeer adds a target Apple or HackDrop device by direct IP address (IPv4 or fe80:: IPv6)
// and immediately probes `/Discover` on ports 8770/8771 to verify reachability and pull its real name.
func (c *Coordinator) AddManualPeer(displayName, modelName, ipAddr string, port int) mdns.PeerService {
	cleanIP := strings.TrimSpace(ipAddr)
	if h, pStr, err := net.SplitHostPort(cleanIP); err == nil {
		cleanIP = h
		fmt.Sscanf(pStr, "%d", &port)
	}
	if port == 0 {
		port = airdrop.DefaultAirDropPort
	}

	// Probe the target over TLS POST /Discover to auto-detect its real name, model, and active port (8770 or 8771)
	probedName, probedModel, activePort, probeErr := c.airdropServer.ProbePeer(c.ctx, cleanIP, port)
	transportTag := "Direct IP (TLS Verified)"
	if probeErr == nil {
		port = activePort
		if displayName == "" || displayName == "MacBook Pro" || displayName == "Apple Device" {
			displayName = probedName
		}
		if modelName == "" || modelName == "MacBookPro16,1" {
			modelName = probedModel
		}
		c.recordPacket("AirDrop HTTPS", "RX",
			fmt.Sprintf("Direct IP /Discover handshake succeeded with %s (%s:%d)", displayName, cleanIP, port),
			fmt.Sprintf("TLS :%d model=%s", port, modelName))
	} else {
		transportTag = fmt.Sprintf("Direct IP (Port :%d)", port)
		if displayName == "" {
			displayName = "Direct IP (" + cleanIP + ")"
		}
		if modelName == "" {
			modelName = "macOS / iOS / HackDrop"
		}
		c.recordPacket("AirDrop HTTPS", "TX",
			fmt.Sprintf("Added Direct IP target %s (%s:%d) — %v", displayName, cleanIP, port, probeErr),
			cleanIP)
	}

	peer := mdns.PeerService{
		InstanceID:  fmt.Sprintf("ip-%s-%d", strings.ReplaceAll(cleanIP, ".", "-"), port),
		HostName:    displayName + ".local.",
		DisplayName: displayName,
		ModelName:   modelName,
		Port:        port,
		Flags:       mdns.DefaultAirDropCapabilityMask,
		TXTRecords:  map[string]string{"flags": "1023"},
		Transport:   transportTag,
		LastSeen:    time.Now(),
	}
	if strings.Contains(cleanIP, ":") {
		peer.IPv6Addr = cleanIP
	} else {
		peer.IPv4Addr = cleanIP
	}

	c.mdnsEngine.UpsertPeer(peer)
	return peer
}

// ScanLocalSubnet concurrently scans the local Wi-Fi `/24` IPv4 subnet for any device listening on
// AirDrop HTTPS ports `:8770` or `:8771` and performs a `/Discover` handshake on responsive nodes.
func (c *Coordinator) ScanLocalSubnet() int {
	myIP := c.lanEngine.IPv4()
	if myIP == nil || myIP.To4() == nil {
		return 0
	}
	ip4 := myIP.To4()
	prefix := fmt.Sprintf("%d.%d.%d.", ip4[0], ip4[1], ip4[2])
	selfLastOctet := int(ip4[3])

	c.recordPacket("AirDrop HTTPS", "TX",
		fmt.Sprintf("Scanning local subnet %s0/24 on ports 8770 & 8771...", prefix),
		fmt.Sprintf("src=%s", ip4.String()))

	var wg sync.WaitGroup
	sem := make(chan struct{}, 48)
	foundCount := 0
	var countMu sync.Mutex

	for host := 1; host <= 254; host++ {
		if host == selfLastOctet {
			continue
		}
		targetIP := fmt.Sprintf("%s%d", prefix, host)
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			for _, port := range []int{8770, 8771} {
				conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), 280*time.Millisecond)
				if err != nil {
					continue
				}
				_ = conn.Close()

				// Port is open! Perform TLS /Discover probe
				cName, mName, activePort, probeErr := c.airdropServer.ProbePeer(c.ctx, ip, port)
				if probeErr != nil {
					cName = fmt.Sprintf("AirDrop Host (%s)", ip)
					mName = "macOS / iOS"
					activePort = port
				}

				c.mdnsEngine.UpsertPeer(mdns.PeerService{
					InstanceID:  fmt.Sprintf("subnet-%s-%d", strings.ReplaceAll(ip, ".", "-"), activePort),
					HostName:    cName + ".local.",
					DisplayName: cName,
					ModelName:   mName,
					IPv4Addr:    ip,
					Port:        activePort,
					Flags:       mdns.DefaultAirDropCapabilityMask,
					Transport:   fmt.Sprintf("Direct IP :%d (Subnet Scan)", activePort),
					LastSeen:    time.Now(),
				})

				countMu.Lock()
				foundCount++
				countMu.Unlock()
				break
			}
		}(targetIP)
	}

	wg.Wait()
	return foundCount
}

func (c *Coordinator) SendFilesToPeer(peerID string, filePaths []string) error {
	peers := c.mdnsEngine.Peers()
	var target *mdns.PeerService
	for i := range peers {
		if peers[i].InstanceID == peerID {
			target = &peers[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("peer %q not found", peerID)
	}

	host := target.IPv4Addr
	if host == "" {
		host = target.IPv6Addr
	}
	if host == "" {
		return fmt.Errorf("peer %s has no reachable IP address", target.DisplayName)
	}

	c.recordPacket("AirDrop HTTPS", "TX",
		fmt.Sprintf("Starting /Discover -> /Ask -> /Upload to %s (%s:%d)", target.DisplayName, host, target.Port),
		fmt.Sprintf("files=%d", len(filePaths)))

	return c.airdropServer.SendFiles(c.ctx, host, target.Port, filePaths)
}

func (c *Coordinator) RespondToPrompt(transferID string, accept bool) bool {
	return c.airdropServer.RespondToAsk(transferID, accept)
}

func (c *Coordinator) UpdateSettings(computerName, visibilityMode string, autoAccept bool) {
	c.mu.Lock()
	if computerName != "" {
		c.computerName = computerName
	}
	if visibilityMode != "" {
		c.visibilityMode = visibilityMode
	}
	c.autoAccept = autoAccept
	c.mu.Unlock()
	c.airdropServer.SetConfig(c.computerName, c.visibilityMode != "Off", autoAccept)
}

func (c *Coordinator) State() SystemState {
	c.mu.RLock()
	defer c.mu.RUnlock()

	bleList := make([]ble.DiscoveredBLEPeer, 0, len(c.blePeers))
	for _, bp := range c.blePeers {
		bleList = append(bleList, bp)
	}

	localV4 := "127.0.0.1"
	if ip4 := c.lanEngine.IPv4(); ip4 != nil {
		localV4 = ip4.String()
	}
	localV6 := "fe80::"
	if _, ll, err := c.lanEngine.Interface(); err == nil && ll != nil {
		localV6 = ll.String()
	}

	return SystemState{
		DeviceID:        c.deviceID,
		ComputerName:    c.computerName,
		ModelName:       c.modelName,
		OS:              runtime.GOOS + "/" + runtime.GOARCH,
		LocalIPv4:       localV4,
		LocalIPv6:       localV6,
		VisibilityMode:  c.visibilityMode,
		AutoAccept:      c.autoAccept,
		HTTPSPort:       c.airdropServer.Port(),
		DownloadDir:     c.downloadDir,
		BLEAdvertising:  c.visibilityMode != "Off",
		BLEFrameHex:     hex.EncodeToString(c.bleFrame.EncodeManufacturerData(true)),
		Transports: []transport.TransportStatus{
			c.awdlEngine.Status(),
			c.nanEngine.Status(),
			c.lanEngine.Status(),
		},
		DiscoveredPeers: c.mdnsEngine.Peers(),
		BLEPeers:        bleList,
		RecentPackets:   append([]PacketLog(nil), c.packetLogs...),
		RecentEvents:    append([]airdrop.TransferEvent(nil), c.events...),
	}
}

func (c *Coordinator) telemetryLoop(ctx context.Context) {
	ticker := time.NewTicker(12 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := c.awdlEngine.Status()
			psf := c.awdlEngine.BuildActionFrame(awdl.SubtypePSF)
			c.recordPacket("AWDL 802.11", "TX",
				fmt.Sprintf("AWDL EAW Sync Slot on Social Channel %d (IPv6 %s)", st.CurrentChannel, st.IPv6LinkLocal),
				hex.EncodeToString(psf[:min(len(psf), 36)]))
		}
	}
}

func (c *Coordinator) recordPacket(layer, dir, summary, hexBytes string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := PacketLog{
		Timestamp: time.Now().Format("15:04:05.000"),
		Layer:     layer,
		Direction: dir,
		Summary:   summary,
		HexBytes:  hexBytes,
	}
	c.packetLogs = append([]PacketLog{entry}, c.packetLogs...)
	if len(c.packetLogs) > 50 {
		c.packetLogs = c.packetLogs[:50]
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
