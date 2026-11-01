//go:build linux

package awdl

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ProbeLinuxWirelessCapabilities inspects /sys/class/net and /sys/class/ieee80211 on Linux
// to find Wi-Fi PHYs capable of raw AWDL monitor injection and nl80211 Wi-Fi Aware (NAN).
func ProbeLinuxWirelessCapabilities() ([]string, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil, err
	}

	var candidates []string
	for _, entry := range entries {
		name := entry.Name()
		wirelessPath := filepath.Join("/sys/class/net", name, "wireless")
		phyPath := filepath.Join("/sys/class/net", name, "phy80211")
		if _, err := os.Stat(wirelessPath); err == nil {
			candidates = append(candidates, name)
			continue
		}
		if _, err := os.Stat(phyPath); err == nil {
			candidates = append(candidates, name)
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no 802.11 wireless interfaces detected under /sys/class/net")
	}
	return candidates, nil
}

// RadiotapHeader prepends an 8-byte IEEE 802.11 Radiotap header for raw AF_PACKET injection on Linux
// so the driver transmits the AWDL Action Frame without waiting for ACK on multicast.
func BuildRadiotapAWDLFrame(srcMAC net.HardwareAddr, actionPayload []byte) []byte {
	// 8-byte minimal Radiotap header (it_version=0, it_pad=0, it_len=8, it_present=0)
	radiotap := []byte{0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}

	// 24-byte IEEE 802.11 Action Management Frame Header (Subtype 0xD0 = Action)
	dot11 := make([]byte, 24)
	dot11[0] = 0xD0 // Frame Control: Type=Management (0), Subtype=Action (13)
	dot11[1] = 0x00
	// Duration = 0
	copy(dot11[4:10], AWDLBroadcastMAC) // Addr1 (DA): 00:25:00:ff:94:73
	copy(dot11[10:16], srcMAC)          // Addr2 (SA): Our AWDL MAC
	copy(dot11[16:22], AWDLBroadcastMAC) // Addr3 (BSSID): AWDL BSSID

	frame := make([]byte, 0, len(radiotap)+len(dot11)+len(actionPayload))
	frame = append(frame, radiotap...)
	frame = append(frame, dot11...)
	frame = append(frame, actionPayload...)
	return frame
}

func FormatKernelDriverHint(ifaces []string) string {
	return "Detected Linux 802.11 interfaces: " + strings.Join(ifaces, ", ")
}
