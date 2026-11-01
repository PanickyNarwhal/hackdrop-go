package mdns

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// AirDropService is the Bonjour service type queried and advertised by Apple's sharingd.
	AirDropService = "_airdrop._tcp.local."

	// CompanionLinkService is advertised continuously by all iOS and macOS devices on the local network,
	// allowing us to discover nearby iPhones and MacBooks even before their AirDrop window is opened.
	CompanionLinkService = "_companion-link._tcp.local."

	// Flag constants advertised in the _airdrop._tcp.local TXT record (`flags=...`)
	DefaultAirDropCapabilityMask = 0x3ff
)

// PeerService represents an AirDrop / Apple Continuity peer discovered via Bonjour / mDNS.
type PeerService struct {
	InstanceID  string            `json:"instanceId"`
	HostName    string            `json:"hostName"`
	DisplayName string            `json:"displayName"`
	ModelName   string            `json:"modelName"`
	IPv6Addr    string            `json:"ipv6Addr"`
	IPv4Addr    string            `json:"ipv4Addr"`
	Interface   string            `json:"interface"`
	Port        int               `json:"port"`
	Flags       int               `json:"flags"`
	TXTRecords  map[string]string `json:"txtRecords"`
	Transport   string            `json:"transport"`
	LastSeen    time.Time         `json:"lastSeen"`
}

// Engine handles zero-dependency mDNS (Bonjour) multicast queries and responses
// over both IPv6 Link-Local (ff02::fb:5353) and IPv4 (224.0.0.251:5353) for `_airdrop._tcp.local.`.
type Engine struct {
	mu           sync.RWMutex
	instanceID   string
	computerName string
	modelName    string
	port         int
	peers        map[string]*PeerService
	onPeerFound  func(PeerService)
	cancel       context.CancelFunc
}

func NewEngine(instanceID, computerName, modelName string, port int, onPeer func(PeerService)) *Engine {
	return &Engine{
		instanceID:   instanceID,
		computerName: computerName,
		modelName:    modelName,
		port:         port,
		peers:        make(map[string]*PeerService),
		onPeerFound:  onPeer,
	}
}

func (e *Engine) Start(ctx context.Context, iface *net.Interface, ipv6LL, ipv4 net.IP) error {
	runCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel

	// 1. Shared-port (:5353) multicast listeners
	go e.listenMulticastIPv4(runCtx, iface, ipv6LL, ipv4)
	go e.listenMulticastIPv6(runCtx, iface, ipv6LL, ipv4)

	// 2. RFC 6762 §5.1 One-Shot / Unicast-Response UDP transceivers (never blocked by OS port 5353 Dnscache/mDNSResponder)
	go e.runActiveTransceiverIPv4(runCtx, iface, ipv6LL, ipv4)
	go e.runActiveTransceiverIPv6(runCtx, iface, ipv6LL, ipv4)

	return nil
}

func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
}

func (e *Engine) Peers() []PeerService {
	e.mu.RLock()
	defer e.mu.RUnlock()
	list := make([]PeerService, 0, len(e.peers))
	for _, p := range e.peers {
		list = append(list, *p)
	}
	return list
}

func (e *Engine) UpsertPeer(peer PeerService) {
	e.mu.Lock()
	peer.LastSeen = time.Now()
	// Merge with existing entry by IP if already discovered
	key := peer.InstanceID
	for k, existing := range e.peers {
		if (peer.IPv4Addr != "" && existing.IPv4Addr == peer.IPv4Addr) ||
			(peer.IPv6Addr != "" && existing.IPv6Addr == peer.IPv6Addr) {
			key = k
			if peer.IPv4Addr == "" {
				peer.IPv4Addr = existing.IPv4Addr
			}
			if peer.IPv6Addr == "" {
				peer.IPv6Addr = existing.IPv6Addr
			}
			if strings.HasPrefix(peer.DisplayName, "Apple ") && !strings.HasPrefix(existing.DisplayName, "Apple ") {
				peer.DisplayName = existing.DisplayName
			}
			break
		}
	}
	peer.InstanceID = key
	e.peers[key] = &peer
	cb := e.onPeerFound
	e.mu.Unlock()

	if cb != nil {
		cb(peer)
	}
}

// runActiveTransceiverIPv4 binds directly to the active Wi-Fi IPv4 address on an ephemeral port,
// sends RFC 6762 queries (with Unicast-Response bit + multicast) for `_airdrop._tcp.local.` and `_companion-link._tcp.local.`,
// and announces our `_airdrop._tcp.local.` service to `224.0.0.251:5353`.
func (e *Engine) runActiveTransceiverIPv4(ctx context.Context, iface *net.Interface, ipv6LL, ipv4 net.IP) {
	bindIP := net.IPv4zero
	if ipv4 != nil {
		bindIP = ipv4
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: bindIP, Port: 0})
	if err != nil {
		return
	}
	defer conn.Close()

	mcastDst := &net.UDPAddr{IP: net.ParseIP("224.0.0.251"), Port: 5353}
	_ = e.sendDiscoveryQueries(conn, mcastDst)
	_ = e.sendAirDropAnnouncement(conn, mcastDst, ipv6LL, ipv4)

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = e.sendDiscoveryQueries(conn, mcastDst)
				_ = e.sendAirDropAnnouncement(conn, mcastDst, ipv6LL, ipv4)
			}
		}
	}()

	buf := make([]byte, 9000)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			e.handlePacket(buf[:n], src, iface, conn, mcastDst, ipv6LL, ipv4)
		}
	}
}

func (e *Engine) runActiveTransceiverIPv6(ctx context.Context, iface *net.Interface, ipv6LL, ipv4 net.IP) {
	localAddr := &net.UDPAddr{IP: net.IPv6zero, Port: 0}
	if ipv6LL != nil && iface != nil {
		localAddr = &net.UDPAddr{IP: ipv6LL, Port: 0, Zone: iface.Name}
	}
	conn, err := net.ListenUDP("udp6", localAddr)
	if err != nil {
		return
	}
	defer conn.Close()

	mcastDst := &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}
	if iface != nil {
		mcastDst.Zone = iface.Name
	}

	_ = e.sendDiscoveryQueries(conn, mcastDst)
	_ = e.sendAirDropAnnouncement(conn, mcastDst, ipv6LL, ipv4)

	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = e.sendDiscoveryQueries(conn, mcastDst)
				_ = e.sendAirDropAnnouncement(conn, mcastDst, ipv6LL, ipv4)
			}
		}
	}()

	buf := make([]byte, 9000)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			e.handlePacket(buf[:n], src, iface, conn, mcastDst, ipv6LL, ipv4)
		}
	}
}

func (e *Engine) listenMulticastIPv4(ctx context.Context, iface *net.Interface, ipv6LL, ipv4 net.IP) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return nil
		},
	}
	pktConn, err := lc.ListenPacket(ctx, "udp4", "0.0.0.0:5353")
	if err != nil {
		conn, err2 := net.ListenMulticastUDP("udp4", iface, &net.UDPAddr{IP: net.ParseIP("224.0.0.251"), Port: 5353})
		if err2 != nil {
			return
		}
		pktConn = conn
	}
	defer pktConn.Close()

	udpConn, ok := pktConn.(*net.UDPConn)
	if !ok {
		return
	}
	mcast := &net.UDPAddr{IP: net.ParseIP("224.0.0.251"), Port: 5353}
	buf := make([]byte, 9000)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			_ = udpConn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, src, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			e.handlePacket(buf[:n], src, iface, udpConn, mcast, ipv6LL, ipv4)
		}
	}
}

func (e *Engine) listenMulticastIPv6(ctx context.Context, iface *net.Interface, ipv6LL, ipv4 net.IP) {
	addr := &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}
	if iface != nil {
		addr.Zone = iface.Name
	}
	conn, err := net.ListenMulticastUDP("udp6", iface, addr)
	if err != nil {
		return
	}
	defer conn.Close()

	buf := make([]byte, 9000)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			e.handlePacket(buf[:n], src, iface, conn, addr, ipv6LL, ipv4)
		}
	}
}

// sendDiscoveryQueries queries both `_airdrop._tcp.local.` and `_companion-link._tcp.local.`
// with both Unicast-Response (QU=1, 0x8001) and Multicast-Response (QM=0, 0x0001) questions.
func (e *Engine) sendDiscoveryQueries(conn *net.UDPConn, dst *net.UDPAddr) error {
	var pkt bytes.Buffer
	// QDCOUNT = 2 (_airdrop._tcp.local. and _companion-link._tcp.local.)
	_ = binary.Write(&pkt, binary.BigEndian, [6]uint16{0, 0x0000, 2, 0, 0, 0})

	encodeDNSName(&pkt, AirDropService)
	_ = binary.Write(&pkt, binary.BigEndian, [2]uint16{12, 0x8001}) // PTR + QU (Unicast Response requested)

	encodeDNSName(&pkt, CompanionLinkService)
	_ = binary.Write(&pkt, binary.BigEndian, [2]uint16{12, 0x8001}) // PTR + QU

	_, err := conn.WriteToUDP(pkt.Bytes(), dst)
	return err
}

func (e *Engine) sendAirDropAnnouncement(conn *net.UDPConn, dst *net.UDPAddr, ipv6LL, ipv4 net.IP) error {
	var pkt bytes.Buffer
	instanceFQDN := fmt.Sprintf("%s.%s", e.instanceID, AirDropService)
	hostFQDN := fmt.Sprintf("%s.local.", sanitizeHost(e.computerName))

	ancount := uint16(3)
	if ipv6LL != nil {
		ancount++
	}
	if ipv4 != nil {
		ancount++
	}

	_ = binary.Write(&pkt, binary.BigEndian, [6]uint16{0, 0x8400, 0, ancount, 0, 0})

	// 1. PTR Record
	encodeDNSName(&pkt, AirDropService)
	var ptrRData bytes.Buffer
	encodeDNSName(&ptrRData, instanceFQDN)
	writeResourceRecordHeader(&pkt, 12, 0x0001, 120, uint16(ptrRData.Len()))
	pkt.Write(ptrRData.Bytes())

	// 2. SRV Record
	encodeDNSName(&pkt, instanceFQDN)
	var srvRData bytes.Buffer
	_ = binary.Write(&srvRData, binary.BigEndian, [3]uint16{0, 0, uint16(e.port)})
	encodeDNSName(&srvRData, hostFQDN)
	writeResourceRecordHeader(&pkt, 33, 0x8001, 120, uint16(srvRData.Len()))
	pkt.Write(srvRData.Bytes())

	// 3. TXT Record
	encodeDNSName(&pkt, instanceFQDN)
	txtEntries := []string{
		fmt.Sprintf("flags=%d", DefaultAirDropCapabilityMask),
	}
	var txtRData bytes.Buffer
	for _, entry := range txtEntries {
		txtRData.WriteByte(uint8(len(entry)))
		txtRData.WriteString(entry)
	}
	writeResourceRecordHeader(&pkt, 16, 0x8001, 120, uint16(txtRData.Len()))
	pkt.Write(txtRData.Bytes())

	// 4. AAAA Record (IPv6 Link-Local fe80::)
	if ipv6LL != nil && len(ipv6LL.To16()) == 16 {
		encodeDNSName(&pkt, hostFQDN)
		writeResourceRecordHeader(&pkt, 28, 0x8001, 120, 16)
		pkt.Write(ipv6LL.To16())
	}

	// 5. A Record (IPv4)
	if ipv4 != nil && len(ipv4.To4()) == 4 {
		encodeDNSName(&pkt, hostFQDN)
		writeResourceRecordHeader(&pkt, 1, 0x8001, 120, 4)
		pkt.Write(ipv4.To4())
	}

	_, err := conn.WriteToUDP(pkt.Bytes(), dst)
	return err
}

func (e *Engine) handlePacket(pkt []byte, src *net.UDPAddr, iface *net.Interface, conn *net.UDPConn, mcast *net.UDPAddr, ipv6LL, ipv4 net.IP) {
	if len(pkt) < 12 {
		return
	}
	// Ignore our own IP's loopback packets
	if ipv4 != nil && src.IP.Equal(ipv4) {
		return
	}
	if ipv6LL != nil && src.IP.Equal(ipv6LL) {
		return
	}

	rawLower := strings.ToLower(string(pkt))
	if !strings.Contains(rawLower, "_airdrop") && !strings.Contains(rawLower, "_companion-link") {
		return
	}

	flags := binary.BigEndian.Uint16(pkt[2:4])
	isResponse := (flags & 0x8000) != 0

	if !isResponse {
		_ = e.sendAirDropAnnouncement(conn, mcast, ipv6LL, ipv4)
		return
	}

	if strings.Contains(string(pkt), e.instanceID) {
		return
	}

	parsed := parseDNSResponseRecords(pkt, src, iface)
	if parsed != nil {
		e.UpsertPeer(*parsed)
	}
}

func parseDNSResponseRecords(pkt []byte, src *net.UDPAddr, iface *net.Interface) *PeerService {
	if len(pkt) < 12 {
		return nil
	}
	qdCount := int(binary.BigEndian.Uint16(pkt[4:6]))
	anCount := int(binary.BigEndian.Uint16(pkt[6:8]))
	nsCount := int(binary.BigEndian.Uint16(pkt[8:10]))
	arCount := int(binary.BigEndian.Uint16(pkt[10:12]))
	totalRRs := anCount + nsCount + arCount

	offset := 12
	for q := 0; q < qdCount && offset < len(pkt); q++ {
		_, nextOff := decodeDNSName(pkt, offset)
		offset = nextOff + 4
	}

	ifaceName := "wlan0"
	if iface != nil {
		ifaceName = iface.Name
	}

	peer := &PeerService{
		InstanceID:  fmt.Sprintf("apple-%s", src.IP.String()),
		HostName:    fmt.Sprintf("peer-%s.local.", src.IP.String()),
		DisplayName: fmt.Sprintf("Apple Device (%s)", src.IP.String()),
		ModelName:   "macOS / iOS",
		Interface:   ifaceName,
		Port:        8770,
		Flags:       DefaultAirDropCapabilityMask,
		TXTRecords:  make(map[string]string),
		Transport:   "Bonjour mDNS (IPv6/IPv4)",
		LastSeen:    time.Now(),
	}
	if src.IP.To4() != nil {
		peer.IPv4Addr = src.IP.String()
	} else {
		peer.IPv6Addr = src.IP.String()
	}

	for r := 0; r < totalRRs && offset+10 <= len(pkt); r++ {
		rrName, nextOff := decodeDNSName(pkt, offset)
		offset = nextOff
		if offset+10 > len(pkt) {
			break
		}
		rrType := binary.BigEndian.Uint16(pkt[offset : offset+2])
		rdLen := int(binary.BigEndian.Uint16(pkt[offset+8 : offset+10]))
		offset += 10
		if offset+rdLen > len(pkt) {
			break
		}
		rdata := pkt[offset : offset+rdLen]
		rdataStart := offset
		offset += rdLen

		rrLower := strings.ToLower(rrName)
		if strings.Contains(rrLower, "_airdrop._tcp") {
			peer.Transport = "AirDrop Bonjour (_airdrop._tcp)"
			if parts := strings.Split(rrName, "."); len(parts) > 0 && parts[0] != "_airdrop" {
				peer.InstanceID = parts[0]
			}
		} else if strings.Contains(rrLower, "_companion-link._tcp") {
			if parts := strings.Split(rrName, "."); len(parts) > 0 && parts[0] != "_companion-link" {
				peer.DisplayName = parts[0]
			}
		}

		switch rrType {
		case 12: // PTR
			targetName, _ := decodeDNSName(pkt, rdataStart)
			tLower := strings.ToLower(targetName)
			if strings.Contains(tLower, "_airdrop._tcp") {
				peer.Transport = "AirDrop Bonjour (_airdrop._tcp)"
				if parts := strings.Split(targetName, "."); len(parts) > 0 && parts[0] != "_airdrop" {
					peer.InstanceID = parts[0]
				}
			} else if strings.Contains(tLower, "_companion-link._tcp") {
				if parts := strings.Split(targetName, "."); len(parts) > 0 && parts[0] != "_companion-link" {
					peer.DisplayName = parts[0]
				}
			}
		case 33: // SRV
			if len(rdata) >= 6 {
				srvPort := int(binary.BigEndian.Uint16(rdata[4:6]))
				targetHost, _ := decodeDNSName(pkt, rdataStart+6)
				if strings.Contains(rrLower, "_airdrop._tcp") && srvPort > 0 {
					peer.Port = srvPort
				}
				if targetHost != "" {
					peer.HostName = targetHost
					cleanHost := strings.TrimSuffix(strings.TrimSuffix(targetHost, "."), ".local")
					if cleanHost != "" && strings.HasPrefix(peer.DisplayName, "Apple ") {
						peer.DisplayName = cleanHost
					}
				}
			}
		case 16: // TXT
			pos := 0
			for pos < len(rdata) {
				txtLen := int(rdata[pos])
				pos++
				if pos+txtLen > len(rdata) {
					break
				}
				kv := string(rdata[pos : pos+txtLen])
				pos += txtLen
				if eq := strings.IndexByte(kv, '='); eq != -1 {
					k := kv[:eq]
					v := kv[eq+1:]
					peer.TXTRecords[k] = v
					switch strings.ToLower(k) {
					case "flags":
						if fv, err := strconv.Atoi(v); err == nil {
							peer.Flags = fv
						}
					case "model", "rpmd":
						peer.ModelName = v
					case "name":
						peer.DisplayName = v
					}
				}
			}
		case 28: // AAAA (IPv6)
			if len(rdata) == 16 {
				ip := net.IP(rdata)
				if ip.IsLinkLocalUnicast() || peer.IPv6Addr == "" {
					peer.IPv6Addr = ip.String()
				}
			}
		case 1: // A (IPv4)
			if len(rdata) == 4 {
				peer.IPv4Addr = net.IP(rdata).String()
			}
		}
	}

	return peer
}

func decodeDNSName(pkt []byte, start int) (string, int) {
	var labels []string
	curr := start
	nextOffset := start
	jumped := false
	hops := 0

	for curr < len(pkt) && hops < 32 {
		hops++
		length := int(pkt[curr])
		if length == 0 {
			if !jumped {
				nextOffset = curr + 1
			}
			break
		}
		if (length & 0xC0) == 0xC0 {
			if curr+1 >= len(pkt) {
				break
			}
			ptr := ((length & 0x3F) << 8) | int(pkt[curr+1])
			if !jumped {
				nextOffset = curr + 2
				jumped = true
			}
			curr = ptr
			continue
		}
		curr++
		if curr+length > len(pkt) {
			break
		}
		labels = append(labels, string(pkt[curr:curr+length]))
		curr += length
		if !jumped {
			nextOffset = curr
		}
	}

	return strings.Join(labels, "."), nextOffset
}

func encodeDNSName(buf *bytes.Buffer, fqdn string) {
	trimmed := strings.TrimSuffix(fqdn, ".")
	parts := strings.Split(trimmed, ".")
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		buf.WriteByte(uint8(len(part)))
		buf.WriteString(part)
	}
	buf.WriteByte(0x00)
}

func writeResourceRecordHeader(buf *bytes.Buffer, rrType, rrClass uint16, ttl uint32, rdLength uint16) {
	_ = binary.Write(buf, binary.BigEndian, rrType)
	_ = binary.Write(buf, binary.BigEndian, rrClass)
	_ = binary.Write(buf, binary.BigEndian, ttl)
	_ = binary.Write(buf, binary.BigEndian, rdLength)
}

func sanitizeHost(name string) string {
	clean := strings.ReplaceAll(strings.TrimSpace(name), " ", "-")
	if clean == "" {
		return "HackDrop-Node"
	}
	return clean
}
