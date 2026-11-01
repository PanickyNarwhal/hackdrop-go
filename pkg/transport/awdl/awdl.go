package awdl

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	"hackdrop-go/pkg/transport"
)

// Apple AWDL 802.11 Vendor-Specific Action Frame Constants
var (
	// AppleOUI is the 3-byte IEEE OUI assigned to Apple, Inc. (00:17:F2)
	AppleOUI = [3]byte{0x00, 0x17, 0xF2}

	// AWDLBroadcastMAC is the multicast destination MAC used by AWDL synchronization & election frames.
	AWDLBroadcastMAC = net.HardwareAddr{0x00, 0x25, 0x00, 0xff, 0x94, 0x73}
)

const (
	// ActionCategoryVendorSpecific is the 802.11 management action category (0x7F = 127)
	ActionCategoryVendorSpecific uint8 = 0x7F

	// AWDLType is the Apple vendor frame type for AWDL (0x08)
	AWDLType uint8 = 0x08

	// AWDLVersion is Version 1.0 (0x10)
	AWDLVersion uint8 = 0x10

	// AWDL Subtypes
	SubtypePSF  uint8 = 0x00 // Periodic Synchronization Frame
	SubtypeMIF  uint8 = 0x03 // Master Indication Frame
	SubtypeData uint8 = 0x04 // AWDL Data Frame (L2 LLC/SNAP encapsulated IPv6)

	// AWDL TLV Types
	TLVTypeSyncTree           uint8 = 0x04
	TLVTypeElectionParams     uint8 = 0x05
	TLVTypeChanSeq            uint8 = 0x06
	TLVTypeEnhancedDataRate   uint8 = 0x07
	TLVTypeDataPathState      uint8 = 0x0C
	TLVTypeArpa               uint8 = 0x10
	TLVTypeVersion            uint8 = 0x15
	TLVTypeSynchronization    uint8 = 0x07
)

// SocialChannels are the primary 2.4GHz and 5GHz rendezvous channels used by AWDL (6, 44, 149).
var SocialChannels = []int{6, 44, 149}

// FrameHeader represents the 12-byte header inside an Apple AWDL 802.11 Action Frame.
type FrameHeader struct {
	Category   uint8    // 0x7F (Vendor Specific Action)
	OUI        [3]byte  // 00:17:F2
	Type       uint8    // 0x08 (AWDL)
	Version    uint8    // 0x10 (v1.0)
	Subtype    uint8    // 0x00 = PSF, 0x03 = MIF
	Reserved   uint8    // 0x00
	PhyTxTime  uint32   // PHY transmit timestamp (microseconds)
	TargetTx   uint32   // Target transmit timestamp (microseconds)
}

// ElectionState holds our local AWDL master election state so iPhones/Macs accept our synchronization clock.
type ElectionState struct {
	MasterAddr   net.HardwareAddr
	SelfAddr     net.HardwareAddr
	SelfMetric   uint32
	MasterMetric uint32
	Height       uint8
	SyncCounter  uint16
}

// Engine implements the Apple Wireless Direct Link (AWDL) protocol state machine and frame codec.
type Engine struct {
	mu             sync.RWMutex
	running        bool
	selfMAC        net.HardwareAddr
	selfIPv6       net.IP
	ifaceName      string
	currentChannel int
	election       ElectionState
	peers          map[string]transport.PeerLinkInfo
	cancel         context.CancelFunc
}

// NewEngine creates a new AWDL engine instance.
func NewEngine(preferredIface string) *Engine {
	mac := net.HardwareAddr{0x02, 0x17, 0xf2, 0xa4, 0x26, 0x01} // Locally administered AWDL MAC
	return &Engine{
		selfMAC:        mac,
		selfIPv6:       transport.DeriveEUI64LinkLocal(mac),
		ifaceName:      preferredIface,
		currentChannel: 44, // Default 5GHz social channel preferred by macOS 26/27 & iOS 27
		election: ElectionState{
			MasterAddr:   mac,
			SelfAddr:     mac,
			SelfMetric:   60,
			MasterMetric: 60,
			Height:       0,
		},
		peers: make(map[string]transport.PeerLinkInfo),
	}
}

func (e *Engine) Name() transport.Mode {
	return transport.ModeAWDL
}

func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	e.running = true

	// Start AWDL channel synchronization loop (16 TU / ~110ms window slot ticker)
	go e.syncLoop(runCtx)
	return nil
}

func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return nil
	}
	if e.cancel != nil {
		e.cancel()
	}
	e.running = false
	return nil
}

func (e *Engine) Interface() (*net.Interface, net.IP, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.ifaceName != "" {
		if iface, err := net.InterfaceByName(e.ifaceName); err == nil {
			return iface, e.selfIPv6, nil
		}
	}
	return nil, e.selfIPv6, fmt.Errorf("virtual awdl0 interface not bound to raw monitor radio yet")
}

func (e *Engine) Status() transport.TransportStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()

	backend := "Linux nl80211 Monitor + TUN/TAP (awdl0)"
	detail := "Ready to bind raw 802.11 monitor interface via nl80211 for AWDL channel hopping (ch 6, 44, 149)"
	if runtime.GOOS == "windows" {
		backend = "Windows Npcap 802.11 Raw Injection + Wintun"
		detail = "AWDL frame codec active (PSF/MIF/LLC). Requires Npcap monitor-mode USB adapter on Windows for raw 802.11 injection; falls back to Wi-Fi Aware / IPv6 Link-Local on primary NDIS adapter."
	}

	return transport.TransportStatus{
		Mode:              transport.ModeAWDL,
		Active:            e.running,
		InterfaceName:     e.ifaceName,
		IPv6LinkLocal:     e.selfIPv6.String(),
		HardwareSupported: true,
		DriverBackend:     backend,
		CurrentChannel:    e.currentChannel,
		Detail:            detail,
	}
}

func (e *Engine) Peers() []transport.PeerLinkInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]transport.PeerLinkInfo, 0, len(e.peers))
	for _, p := range e.peers {
		out = append(out, p)
	}
	return out
}

func (e *Engine) syncLoop(ctx context.Context) {
	ticker := time.NewTicker(110 * time.Millisecond) // 1 AWDL EAW (Availability Window) = 16 TUs (~16.384ms) x 6.7
	defer ticker.Stop()
	chIdx := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.mu.Lock()
			chIdx = (chIdx + 1) % len(SocialChannels)
			e.currentChannel = SocialChannels[chIdx]
			e.election.SyncCounter++
			e.mu.Unlock()
		}
	}
}

// BuildActionFrame constructs a complete Apple AWDL 802.11 Action Frame (PSF or MIF)
// including Election Parameters, Channel Sequence (6/44/149), and DataPath State TLVs.
func (e *Engine) BuildActionFrame(subtype uint8) []byte {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var buf bytes.Buffer
	nowMicros := uint32(time.Now().UnixMicro() & 0xFFFFFFFF)

	// 1. Vendor Specific Action Header
	buf.WriteByte(ActionCategoryVendorSpecific)
	buf.Write(AppleOUI[:])
	buf.WriteByte(AWDLType)
	buf.WriteByte(AWDLVersion)
	buf.WriteByte(subtype)
	buf.WriteByte(0x00) // Reserved
	_ = binary.Write(&buf, binary.LittleEndian, nowMicros)
	_ = binary.Write(&buf, binary.LittleEndian, nowMicros+1024)

	// 2. Election Parameters TLV (0x05)
	electionPayload := make([]byte, 21)
	electionPayload[0] = 0x00 // Flags
	binary.LittleEndian.PutUint16(electionPayload[1:3], 0x0000)
	electionPayload[3] = e.election.Height
	binary.LittleEndian.PutUint32(electionPayload[4:8], e.election.MasterMetric)
	copy(electionPayload[8:14], e.election.MasterAddr)
	copy(electionPayload[14:20], e.election.SelfAddr)
	writeTLV(&buf, TLVTypeElectionParams, electionPayload)

	// 3. Channel Sequence TLV (0x06) - 16 slots across social channels 6, 44, 149
	chanSeqPayload := make([]byte, 20)
	chanSeqPayload[0] = 15 // Count - 1 (16 channels)
	chanSeqPayload[1] = 0x00 // Encoding (legacy/social)
	chanSeqPayload[2] = 0x00 // Duplicate count
	chanSeqPayload[3] = 0x03 // Step count
	for i := 0; i < 16; i++ {
		ch := SocialChannels[i%len(SocialChannels)]
		chanSeqPayload[4+i] = uint8(ch)
	}
	writeTLV(&buf, TLVTypeChanSeq, chanSeqPayload)

	// 4. DataPath State TLV (0x0C) - Advertises our EUI-64 link-local AirDrop readiness
	dpPayload := make([]byte, 16)
	dpPayload[0] = 0x01 // Flags: AirDrop / DataPath active
	copy(dpPayload[2:8], e.selfMAC)
	writeTLV(&buf, TLVTypeDataPathState, dpPayload)

	// 5. Version TLV (0x15)
	writeTLV(&buf, TLVTypeVersion, []byte{0x10, 0x04}) // AWDL v1.0, Device Class macOS/iOS compatible

	return buf.Bytes()
}

// ParseActionFrame decodes an incoming 802.11 Vendor-Specific Action Frame and extracts AWDL peer state.
func (e *Engine) ParseActionFrame(srcMAC net.HardwareAddr, rssi int, payload []byte) (*transport.PeerLinkInfo, error) {
	if len(payload) < 16 {
		return nil, fmt.Errorf("frame too short for AWDL header (%d bytes)", len(payload))
	}
	if payload[0] != ActionCategoryVendorSpecific || !bytes.Equal(payload[1:4], AppleOUI[:]) || payload[4] != AWDLType {
		return nil, fmt.Errorf("not an Apple AWDL action frame")
	}

	peer := transport.PeerLinkInfo{
		MACAddress:    srcMAC,
		IPv6LinkLocal: transport.DeriveEUI64LinkLocal(srcMAC),
		InterfaceName: e.ifaceName,
		TransportMode: transport.ModeAWDL,
		Channel:       e.currentChannel,
		RSSI:          rssi,
		LastSeen:      time.Now(),
	}

	offset := 16 // Skip 16-byte AWDL fixed header
	for offset+3 <= len(payload) {
		tlvType := payload[offset]
		tlvLen := int(binary.LittleEndian.Uint16(payload[offset+1 : offset+3]))
		offset += 3
		if offset+tlvLen > len(payload) {
			break
		}
		tlvData := payload[offset : offset+tlvLen]
		offset += tlvLen

		if tlvType == TLVTypeElectionParams && len(tlvData) >= 8 {
			peer.PeerMasterRank = uint64(binary.LittleEndian.Uint32(tlvData[4:8]))
		}
	}

	e.mu.Lock()
	e.peers[srcMAC.String()] = peer
	e.mu.Unlock()

	return &peer, nil
}

func writeTLV(buf *bytes.Buffer, tlvType uint8, val []byte) {
	buf.WriteByte(tlvType)
	_ = binary.Write(buf, binary.LittleEndian, uint16(len(val)))
	buf.Write(val)
}
