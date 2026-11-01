//go:build darwin

package awdl

import (
	"net"
	"os/exec"
)

// EnableHackintoshItlwmBridge configures macOS NetworkBrowser to allow AirDrop discovery over
// standard Intel (`itlwm` / `AirportItlwm`) `en0`/`en1` IPv6 link-local interfaces when native Broadcom `awdl0` is absent.
func EnableHackintoshItlwmBridge() error {
	return exec.Command("defaults", "write", "com.apple.NetworkBrowser", "BrowseAllInterfaces", "-bool", "true").Run()
}

// BuildRadiotapAWDLFrame constructs the 802.11 Management Action frame with Radiotap header on darwin.
func BuildRadiotapAWDLFrame(srcMAC net.HardwareAddr, actionPayload []byte) []byte {
	radiotap := []byte{0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}
	dot11 := make([]byte, 24)
	dot11[0] = 0xD0
	copy(dot11[4:10], AWDLBroadcastMAC)
	copy(dot11[10:16], srcMAC)
	copy(dot11[16:22], AWDLBroadcastMAC)

	frame := make([]byte, 0, len(radiotap)+len(dot11)+len(actionPayload))
	frame = append(frame, radiotap...)
	frame = append(frame, dot11...)
	frame = append(frame, actionPayload...)
	return frame
}
