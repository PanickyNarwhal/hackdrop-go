//go:build windows

package awdl

import (
	"net"
	"os"
)

// ProbeWindowsDriverSupport checks whether Npcap (`C:\Windows\System32\Npcap\wpcap.dll`) and
// Wintun are installed on Windows for raw 802.11 AWDL frame injection, in addition to native WinRT BLE & Wi-Fi Direct.
func ProbeWindowsDriverSupport() (hasNpcap bool, npcapPath string) {
	candidates := []string{
		`C:\Windows\System32\Npcap\wpcap.dll`,
		`C:\Windows\System32\wpcap.dll`,
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true, p
		}
	}
	return false, ""
}

// BuildRadiotapAWDLFrame constructs the full 802.11 Management Action frame with Radiotap header
// for transmission over an Npcap monitor-mode adapter on Windows.
func BuildRadiotapAWDLFrame(srcMAC net.HardwareAddr, actionPayload []byte) []byte {
	radiotap := []byte{0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}
	dot11 := make([]byte, 24)
	dot11[0] = 0xD0 // IEEE 802.11 Management Action Frame
	copy(dot11[4:10], AWDLBroadcastMAC)
	copy(dot11[10:16], srcMAC)
	copy(dot11[16:22], AWDLBroadcastMAC)

	frame := make([]byte, 0, len(radiotap)+len(dot11)+len(actionPayload))
	frame = append(frame, radiotap...)
	frame = append(frame, dot11...)
	frame = append(frame, actionPayload...)
	return frame
}
